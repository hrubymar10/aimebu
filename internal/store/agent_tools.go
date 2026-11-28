package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

var ErrResumeClosedRoom = errors.New("resume_room_closed")
var ErrRoomClosingByPerson = errors.New("room_closing_by_person")
var ErrRoomClosingActorUnknown = errors.New("room_closing_actor_unknown")

// ResumeAgentAsTool keeps the worker's checkout and native session. The room
// transition and agent reactivation commit together, so a refused resume
// cannot leave a closing room open.
func (tx *Tx) ResumeAgentAsTool(agentID, actorID, reason string) (AgentCard, []AgentCard, error) {
	var projectID, roomID, roleKey, desired, observed, lifecycle, kind, parentLifecycle string
	var rev int64
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,COALESCE(a.home_room_id,''),r.role_key,a.desired,a.observed,a.desired_rev,
		COALESCE(room.lifecycle,''),COALESCE(room.kind,''),COALESCE(parent.lifecycle,'open')
		FROM agent a JOIN role r ON r.id=a.role_id LEFT JOIN room ON room.id=a.home_room_id
		LEFT JOIN room parent ON parent.id=room.parent_id
		WHERE a.id=? AND a.revoked_at IS NULL`, agentID).
		Scan(&projectID, &roomID, &roleKey, &desired, &observed, &rev, &lifecycle, &kind, &parentLifecycle)
	if err != nil {
		return AgentCard{}, nil, err
	}
	var actorProject string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, actorID).Scan(&actorProject); err != nil {
		return AgentCard{}, nil, err
	}
	if actorProject != projectID {
		return AgentCard{}, nil, sql.ErrNoRows
	}
	if kind != "task" || roomID == "" || roleKey == "leader" || roleKey == "dispatcher" {
		return AgentCard{}, nil, ErrInvalidTransition
	}
	if lifecycle == "closed" {
		return AgentCard{}, nil, ErrResumeClosedRoom
	}
	if lifecycle != "open" && lifecycle != "closing" || desired != "stopped" || observed != "stopped" {
		return AgentCard{}, nil, ErrInvalidTransition
	}
	if parentLifecycle != "open" {
		return AgentCard{}, nil, ErrInvalidTransition
	}
	if err := tx.authorizeSpawnActor(projectID, "agent", actorID, false); err != nil {
		return AgentCard{}, nil, err
	}
	if err := tx.flowActorMayTargetAgent(projectID, actorID, agentID, roleKey); err != nil {
		return AgentCard{}, nil, err
	}
	var restored []AgentCard
	if lifecycle == "closing" {
		var closingActorKind sql.NullString
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT json_extract(payload_json,'$.actor_kind') FROM event
			WHERE room_id=? AND kind='room_closing' ORDER BY seq DESC LIMIT 1`, roomID).Scan(&closingActorKind)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (!closingActorKind.Valid || closingActorKind.String != "user" && closingActorKind.String != "agent") {
			return AgentCard{}, nil, ErrRoomClosingActorUnknown
		}
		if err != nil {
			return AgentCard{}, nil, err
		}
		if closingActorKind.String == "user" {
			return AgentCard{}, nil, ErrRoomClosingByPerson
		}
		if _, err := tx.Exec(`UPDATE room SET lifecycle='open',gate=CASE WHEN control_desired='active' THEN 'open' ELSE 'paused' END,
			lifecycle_rev=lifecycle_rev+1,closed_at=NULL WHERE id=?`, roomID); err != nil {
			return AgentCard{}, nil, err
		}
		restored, err = tx.restoreRoomClosePausedAgents(projectID, roomID, actorID)
		if err != nil {
			return AgentCard{}, nil, err
		}
		if _, err := tx.AppendEvent(Event{Kind: "room_close_cancelled", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"room_id": roomID, "actor_id": actorID})}); err != nil {
			return AgentCard{}, nil, err
		}
	}
	if err := tx.AddMember(roomID, "agent", agentID); err != nil {
		return AgentCard{}, nil, err
	}
	if _, err := tx.SetAgentDesiredByAgent(agentID, actorID, "active", rev); err != nil {
		return AgentCard{}, nil, err
	}
	card, err := tx.agentCardByID(agentID)
	if err != nil {
		return AgentCard{}, nil, err
	}
	if card.BoundaryPending != "" {
		return card, restored, nil
	}
	if _, err := tx.AppendEvent(Event{Kind: "agent_resumed", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
		PayloadJSON: payloadJSON(map[string]string{"agent_id": agentID, "actor_id": actorID, "reason": strings.TrimSpace(reason)})}); err != nil {
		return AgentCard{}, nil, err
	}
	return card, restored, nil
}

// restoreRoomClosePausedAgents reverses only the control transitions made by
// this room's latest close. The desired revision excludes later manual changes.
func (tx *Tx) restoreRoomClosePausedAgents(projectID, roomID, actorID string) ([]AgentCard, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT agent_id,CAST(json_extract(payload_json,'$.desired_rev') AS INTEGER)
		FROM event WHERE room_id=? AND kind='room_close_agent_paused'
		AND seq>(SELECT COALESCE(MAX(seq),0) FROM event WHERE room_id=? AND kind='room_closing')
		ORDER BY seq`, roomID, roomID)
	if err != nil {
		return nil, err
	}
	type pause struct {
		agentID string
		rev     int64
	}
	var pauses []pause
	for rows.Next() {
		var p pause
		if err := rows.Scan(&p.agentID, &p.rev); err != nil {
			rows.Close()
			return nil, err
		}
		pauses = append(pauses, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var restored []AgentCard
	var unauthorized []string
	for _, p := range pauses {
		var desired, homeRoomID, slug, roleKey string
		var rev int64
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.desired,a.desired_rev,COALESCE(a.home_room_id,''),a.slug,r.role_key
			FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=? AND a.project_id=? AND a.revoked_at IS NULL`, p.agentID, projectID).
			Scan(&desired, &rev, &homeRoomID, &slug, &roleKey)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if desired != "not_accepting" || rev != p.rev || homeRoomID != roomID {
			continue
		}
		if err := tx.flowActorMayTargetAgent(projectID, actorID, p.agentID, roleKey); err != nil {
			if !errors.Is(err, ErrTeamFlowRule) {
				return nil, err
			}
			unauthorized = append(unauthorized, slug)
			continue
		}
		card, err := tx.setAgentDesired(p.agentID, "agent", actorID, "active", rev, false, false)
		if err != nil {
			return nil, err
		}
		restored = append(restored, card)
	}
	if len(unauthorized) > 0 {
		body := "Agents " + strings.Join(unauthorized, ", ") + " remain paused after the room closure was cancelled because the resuming agent cannot manage them under this team flow. An authorized coordinator can reactivate them."
		if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: body}); err != nil {
			return nil, err
		}
	}
	return restored, nil
}

type AgentToolIdentity struct {
	ID        string
	Slug      string
	ProjectID string
	Tool      string
	Provider  string
	IsLead    bool
	CanSpawn  bool
	CanManage bool
	Desired   string
}

type ProjectAgent struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	RoleKey    string `json:"role_key"`
	Desired    string `json:"desired"`
	DesiredRev int64  `json:"desired_rev"`
	Observed   string `json:"observed"`
	RoomID     string `json:"room_id"`
}

func (s *Store) ProjectAgentsForTool(ctx context.Context, projectID string) ([]ProjectAgent, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.slug,r.role_key,a.desired,a.desired_rev,a.observed,COALESCE(a.home_room_id,'')
		FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND a.revoked_at IS NULL ORDER BY a.created_at,a.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectAgent{}
	for rows.Next() {
		var a ProjectAgent
		if err := rows.Scan(&a.ID, &a.Slug, &a.RoleKey, &a.Desired, &a.DesiredRev, &a.Observed, &a.RoomID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type AgentHistoryPage struct {
	Messages   []AgentMessage `json:"messages"`
	NextBefore int64          `json:"next_before,omitempty"`
	HasMore    bool           `json:"has_more"`
}

// AgentRoomHistory uses the same project and private-chat boundary as message_get.
// The bounded excerpt/page keeps every returned cursor safe for the MCP client.
func (s *Store) AgentRoomHistory(ctx context.Context, agentID, projectID, roomID string, beforeNo int64, limit int) (AgentHistoryPage, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentHistoryPage{}, err
	}
	var allowed bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM room r WHERE r.id=? AND r.project_id IN (?,`+agentDMProjectSQL+`)
		AND (r.kind<>'dm' AND r.restricted=0 OR EXISTS(SELECT 1 FROM room_member m WHERE m.room_id=r.id AND m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL)))`, roomID, projectID, agentID).Scan(&allowed); err != nil {
		return AgentHistoryPage{}, err
	}
	if !allowed {
		return AgentHistoryPage{}, ErrForbidden
	}
	if beforeNo == 0 {
		beforeNo = 1<<63 - 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.no,r.title,COALESCE(u.login,NULLIF(a.removed_slug,''),a.slug,m.author_id),
		CASE WHEN length(m.body)>2000 THEN substr(m.body,1,2000)||'…' ELSE m.body END,m.created_at,COALESCE(m.appendix_pages_json,''),COALESCE(m.outcome,'')
		FROM message m JOIN room r ON r.id=m.room_id
		LEFT JOIN user u ON m.author_kind='user' AND u.id=m.author_id
		LEFT JOIN agent a ON m.author_kind='agent' AND a.id=m.author_id
		WHERE m.room_id=? AND m.no>0 AND m.no<? AND r.project_id IN (?,`+agentDMProjectSQL+`) AND
		(r.kind<>'dm' AND r.restricted=0 OR EXISTS(SELECT 1 FROM room_member own WHERE own.room_id=r.id AND own.member_kind='agent' AND own.member_id=? AND own.left_at IS NULL))
		ORDER BY m.no DESC LIMIT ?`, roomID, beforeNo, projectID, agentID, limit+1)
	if err != nil {
		return AgentHistoryPage{}, err
	}
	defer rows.Close()
	all := make([]AgentMessage, 0, limit+1)
	type raw struct {
		m    AgentMessage
		body string
	}
	var raws []raw
	for rows.Next() {
		r := raw{m: AgentMessage{Available: true, RoomID: roomID}}
		var pages string
		if err := rows.Scan(&r.m.No, &r.m.Room, &r.m.From, &r.body, &r.m.CreatedAt, &pages, &r.m.Outcome); err != nil {
			return AgentHistoryPage{}, err
		}
		r.m.Body = r.body
		r.m.AppendixPageCount = appendixPageCount(pages)
		raws = append(raws, r)
	}
	if err := rows.Err(); err != nil {
		return AgentHistoryPage{}, err
	}
	rows.Close()
	// The entities table is computed after the scan: the entity queries share
	// this database, and an open cursor here would deadlock a single connection.
	for _, r := range raws {
		entities, err := mentionEntities(ctx, s.db, agentID, projectID, roomID, r.body)
		if err != nil {
			return AgentHistoryPage{}, err
		}
		r.m.Entities = make([]protocol.Entity, 0, len(entities))
		for _, entity := range entities {
			r.m.Entities = append(r.m.Entities, protocol.Entity{ID: entity.ID, Name: entity.Name, Kind: entity.Kind})
		}
		all = append(all, r.m)
	}
	page := AgentHistoryPage{Messages: make([]AgentMessage, 0, limit)}
	size := 32 // JSON wrapper and cursor
	for _, m := range all {
		if len(page.Messages) == limit {
			break
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			return AgentHistoryPage{}, err
		}
		if size+len(encoded)+1 > 240<<10 {
			break
		}
		page.Messages = append(page.Messages, m)
		size += len(encoded) + 1
	}
	page.HasMore = len(all) > len(page.Messages)
	if len(page.Messages) > 0 {
		page.NextBefore = page.Messages[len(page.Messages)-1].No
	}
	slices.Reverse(page.Messages)
	return page, nil
}

// SetAgentRoleAsTool changes the role used for the target's next turn. A live
// turn keeps its original instructions, so role changes wait for an idle agent.
func (tx *Tx) SetAgentRoleAsTool(agentID, actorID, roleKey string, rev int64) (AgentCard, error) {
	var projectID, observed, desired, oldRoleID, oldRoleKey string
	var oldRev int64
	var oldLead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.observed,a.desired,a.desired_rev,a.role_id,r.role_key,r.cap_is_lead
		FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=? AND a.revoked_at IS NULL`, agentID).
		Scan(&projectID, &observed, &desired, &oldRev, &oldRoleID, &oldRoleKey, &oldLead); err != nil {
		return AgentCard{}, err
	}
	if err := tx.authorizeSpawnActor(projectID, "agent", actorID, false); err != nil {
		return AgentCard{}, err
	}
	if oldLead {
		return AgentCard{}, ErrForbidden
	}
	if rev != oldRev {
		return AgentCard{}, ErrStaleRev
	}
	if observed != "idle" && observed != "stopped" {
		return AgentCard{}, ErrInvalidTransition
	}
	var newRoleID, cardinality string
	var newLead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,cardinality,cap_is_lead FROM role WHERE role_key=? AND deleted_at IS NULL`, roleKey).
		Scan(&newRoleID, &cardinality, &newLead); err != nil {
		return AgentCard{}, err
	}
	if newLead {
		return AgentCard{}, ErrForbidden
	}
	if newRoleID == oldRoleID {
		return tx.agentCardByID(agentID)
	}
	if disabled, err := tx.reviewerSpawnDisabled(projectID, roleKey); err != nil {
		return AgentCard{}, err
	} else if disabled {
		return AgentCard{}, ErrReviewerAgentsDisabled
	}
	var flowKey string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key FROM project WHERE id=?`, projectID).Scan(&flowKey); err != nil {
		return AgentCard{}, err
	}
	if flowKey != "" && flowKey != "custom" {
		flow, _ := teamFlowTemplate(flowKey)
		if actorID != agentID {
			if err := tx.flowActorMayTarget(projectID, actorID, oldRoleKey); err != nil {
				return AgentCard{}, err
			}
		}
		if err := tx.flowActorMayTarget(projectID, actorID, roleKey); err != nil {
			return AgentCard{}, err
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT r.kind,r.team_flow_room_key,r.title,r.purpose FROM room_member m JOIN room r ON r.id=m.room_id WHERE m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL AND r.kind<>'dm'`, agentID)
		if err != nil {
			return AgentCard{}, err
		}
		for rows.Next() {
			var kind, key, title, purpose string
			if err := rows.Scan(&kind, &key, &title, &purpose); err != nil {
				rows.Close()
				return AgentCard{}, err
			}
			selector := flowRoomKind(kind, key, title, purpose)
			allowed := false
			if rule, ok := flowJoinRule(flow, selector); ok {
				allowed = flowAllows(rule.AllowedRoles, roleKey)
			}
			if !allowed {
				rows.Close()
				return AgentCard{}, ErrTeamFlowRule
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return AgentCard{}, err
		}
	}
	var roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM event WHERE agent_id=? AND kind='agent_spawn_requested' ORDER BY seq LIMIT 1`, agentID).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM room_member WHERE member_kind='agent' AND member_id=? AND left_at IS NULL ORDER BY joined_at LIMIT 1`, agentID).Scan(&roomID)
	}
	if err != nil {
		return AgentCard{}, err
	}
	if cardinality == "singleton" && desired != "stopped" {
		var occupied int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent WHERE project_id=? AND role_id=? AND id<>? AND revoked_at IS NULL AND (desired!='stopped' OR observed!='stopped')`, projectID, newRoleID, agentID).Scan(&occupied); err != nil {
			return AgentCard{}, err
		}
		if occupied > 0 {
			return AgentCard{}, ErrRoleOccupied
		}
	}
	if _, err := tx.Exec(`UPDATE agent SET role_id=?,desired_rev=desired_rev+1 WHERE id=?`, newRoleID, agentID); err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.AppendEvent(Event{Kind: "agent_role_changed", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
		PayloadJSON: payloadJSON(map[string]string{"actor_id": actorID, "old_role": oldRoleKey, "role": roleKey})}); err != nil {
		return AgentCard{}, err
	}
	// The role-change notice names both agents by their stored tokens; the human view translates them.
	if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
		Body: fmt.Sprintf("@%s changed @%s's role to %s", actorID, agentID, roleKey)}); err != nil {
		return AgentCard{}, err
	}
	return tx.agentCardByID(agentID)
}

func (s *Store) AgentToolIdentityByKeyHash(ctx context.Context, hash string) (AgentToolIdentity, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentToolIdentity{}, err
	}
	var identity AgentToolIdentity
	var roleKey, flowKey string
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.slug,a.project_id,a.tool,a.provider,r.cap_is_lead,r.cap_can_spawn,a.desired,r.role_key,p.team_flow_key
		FROM agent a JOIN role r ON r.id=a.role_id JOIN project p ON p.id=a.project_id WHERE a.key_hash=? AND a.revoked_at IS NULL`, hash).
		Scan(&identity.ID, &identity.Slug, &identity.ProjectID, &identity.Tool, &identity.Provider, &identity.IsLead, &identity.CanSpawn, &identity.Desired, &roleKey, &flowKey)
	identity.CanManage = flowManages(flowKey, roleKey, identity.IsLead, identity.CanSpawn)
	return identity, err
}

// RoomsForAgent lists only active memberships and omits message previews.
// Agent toolbox credentials must not expose other rooms' conversation text.
func (s *Store) RoomsForAgent(ctx context.Context, agentID, projectID string) ([]Room, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.project_id,r.parent_id,r.depth,r.kind,r.dm_key,r.title,r.created_by,
		r.lifecycle,r.gate,r.lifecycle_rev,r.closed_at,r.purpose,
		`+roomMemberCountSQL+`,
		(SELECT COUNT(*) FROM message m WHERE m.room_id=r.id),
		(SELECT COUNT(*) FROM message m WHERE m.room_id=r.id AND m.author_kind!='system')
		FROM room r JOIN room_member own ON own.room_id=r.id AND own.member_kind='agent'
			AND own.member_id=? AND own.left_at IS NULL
		WHERE r.project_id IN (?,`+agentDMProjectSQL+`) ORDER BY r.depth,r.created_at,r.id`, agentID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var room Room
		var parent, dm, closed sql.NullString
		if err := rows.Scan(&room.ID, &room.ProjectID, &parent, &room.Depth, &room.Kind, &dm,
			&room.Title, &room.CreatedBy, &room.Lifecycle, &room.Gate, &room.LifecycleRev,
			&closed, &room.Purpose, &room.MemberCount, &room.MessageCount, &room.UserMessageCount); err != nil {
			return nil, err
		}
		room.ParentID, room.DMKey, room.ClosedAt = parent.String, dm.String, closed.String
		out = append(out, room)
	}
	return out, rows.Err()
}

// CreateTaskRoomAsTool makes the coordinating agent a member before any turn can address the
// new room. The parent notice and room creation share the request transaction.
func (tx *Tx) CreateTaskRoomAsTool(agentID, projectID, parentID, title, purpose string) (Room, error) {
	if err := tx.authorizeSpawnActor(projectID, "agent", agentID, false); err != nil {
		return Room{}, err
	}
	if parentID == "" {
		var flowKey string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key FROM project WHERE id=?`, projectID).Scan(&flowKey); err != nil {
			return Room{}, err
		}
		parentKey := "home"
		if flow, ok := teamFlowTemplate(flowKey); ok && flow.TaskParentKey != "" {
			parentKey = flow.TaskParentKey
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND (team_flow_room_key=? OR ?='home' AND kind='home') ORDER BY created_at LIMIT 1`, projectID, parentKey, parentKey).Scan(&parentID); err != nil {
			return Room{}, err
		}
	}
	var parentProject, parentKind, parentLifecycle, parentKey string
	var joined bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,kind,lifecycle,team_flow_room_key,EXISTS(SELECT 1 FROM room_member
		WHERE room_id=room.id AND member_kind='agent' AND member_id=? AND left_at IS NULL)
		FROM room WHERE id=?`, agentID, parentID).Scan(&parentProject, &parentKind, &parentLifecycle, &parentKey, &joined); err != nil {
		return Room{}, err
	}
	if parentProject != projectID || parentKind == "dm" {
		return Room{}, ErrForbidden
	}
	if !joined {
		// In the dispatcher flow, home is the default parent for new tasks
		// (general stays an explicit choice), but the dispatcher joins
		// neither. Creating a task beneath either does not add it to that room.
		var mayCreate bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent a JOIN role ro ON ro.id=a.role_id
			JOIN project p ON p.id=a.project_id WHERE a.id=? AND a.project_id=?
			AND p.team_flow_key='leader-dispatcher' AND ro.role_key='dispatcher' AND (?='general' OR ?='home'))`,
			agentID, projectID, parentKey, parentKind).Scan(&mayCreate); err != nil {
			return Room{}, err
		}
		if !mayCreate {
			return Room{}, ErrForbidden
		}
	}
	if parentLifecycle != "open" {
		return Room{}, ErrInvalidTransition
	}
	room, err := tx.CreateRoom(Room{ProjectID: projectID, ParentID: parentID, Kind: "task", Title: title, CreatedBy: agentID, Purpose: purpose})
	if err != nil {
		return Room{}, err
	}
	// CreateRoom joined the leads quietly; the one that made the room is a member on purpose.
	if err := tx.AddMember(room.ID, "agent", agentID); err != nil {
		return Room{}, err
	}
	// The notice names the creator and the new room by their stored tokens (an agent name or a title is reusable);
	// the human view translates both.
	if _, err := tx.InsertMessage(Message{RoomID: parentID, AuthorKind: "system", AuthorID: "system",
		Body: fmt.Sprintf("@%s created task room #%s", agentID, room.ID)}); err != nil {
		return Room{}, err
	}
	// The response carries the real member count, so the creator can see it
	// was joined and may brief the room from its final answer.
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT `+roomMemberCountSQL+` FROM room r WHERE r.id=?`, room.ID).Scan(&room.MemberCount); err != nil {
		return Room{}, err
	}
	return room, nil
}

type ProjectToolHeadroom struct {
	MaxLiveAgents    int    `json:"max_live_agents"`
	LiveAgents       int    `json:"live_agents"`
	LiveSlotsLeft    int    `json:"live_slots_left"`
	SpawnRateLimit   int    `json:"spawn_rate_limit"`
	SpawnsInWindow   int    `json:"spawns_in_window"`
	SpawnSlotsLeft   int    `json:"spawn_slots_left"`
	SpawnRateResetAt string `json:"spawn_rate_reset_at,omitempty"`
}

func (s *Store) ProjectToolHeadroom(ctx context.Context, projectID string) (ProjectToolHeadroom, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return ProjectToolHeadroom{}, err
	}
	var result ProjectToolHeadroom
	var flowKey string
	if err := s.db.QueryRowContext(ctx, `SELECT sp.max_live_agents,sp.spawn_rate_per_min,p.team_flow_key FROM settings_project sp JOIN project p ON p.id=sp.project_id WHERE sp.project_id=?`, projectID).
		Scan(&result.MaxLiveAgents, &result.SpawnRateLimit, &flowKey); err != nil {
		return ProjectToolHeadroom{}, err
	}
	live, err := meteredLiveAgents(ctx, s.db, projectID, "", flowKey)
	if err != nil {
		return ProjectToolHeadroom{}, err
	}
	result.LiveAgents = live
	cutoff := time.Now().UTC().Add(-time.Minute).Format(timestampLayout)
	var oldest sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(created_at) FROM event WHERE project_id=? AND kind='agent_created' AND created_at>=?`, projectID, cutoff).
		Scan(&result.SpawnsInWindow, &oldest); err != nil {
		return ProjectToolHeadroom{}, err
	}
	result.LiveSlotsLeft = max(0, result.MaxLiveAgents-result.LiveAgents)
	result.SpawnSlotsLeft = max(0, result.SpawnRateLimit-result.SpawnsInWindow)
	if oldest.Valid {
		when, err := time.Parse(timestampLayout, oldest.String)
		if err != nil {
			return ProjectToolHeadroom{}, err
		}
		result.SpawnRateResetAt = when.Add(time.Minute).Format(timestampLayout)
	}
	return result, nil
}

func (tx *Tx) StopAgentAsTool(agentID, actorID string, rev int64) (AgentCard, error) {
	var targetLead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, agentID).Scan(&targetLead); err != nil {
		return AgentCard{}, err
	}
	if targetLead && agentID != actorID {
		return AgentCard{}, ErrForbidden
	}
	var actorRole, targetRole string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.role_key FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, actorID).Scan(&actorRole); err != nil {
		return AgentCard{}, err
	}
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.role_key FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, agentID).Scan(&targetRole); err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.SetAgentDesiredByAgent(agentID, actorID, "stopped", rev); err != nil {
		return AgentCard{}, err
	}
	var roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM event WHERE agent_id=? AND kind='agent_spawn_requested' ORDER BY seq LIMIT 1`, agentID).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM room_member WHERE member_kind='agent' AND member_id=? ORDER BY joined_at LIMIT 1`, agentID).Scan(&roomID)
	}
	if err != nil {
		return AgentCard{}, err
	}
	// The stop notice names both agents by their stored tokens, with their roles as plain text; the human view translates the tokens.
	if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
		Body: fmt.Sprintf("@%s (%s) stopped @%s (%s)", actorID, actorRole, agentID, targetRole)}); err != nil {
		return AgentCard{}, err
	}
	return tx.agentCardByID(agentID)
}

func (tx *Tx) agentCardByID(id string) (AgentCard, error) {
	var card AgentCard
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.id,a.slug,a.project_id,r.role_key,r.emoji,r.deleted_at IS NOT NULL,
		EXISTS(SELECT 1 FROM role current WHERE current.project_id IS NULL AND current.role_key=r.role_key AND current.id<>r.id AND current.deleted_at IS NULL AND (current.body<>r.body OR current.extends<>r.extends OR current.label<>r.label OR current.emoji<>r.emoji OR current.cardinality<>r.cardinality OR current.cap_is_lead<>r.cap_is_lead OR current.cap_can_spawn<>r.cap_can_spawn OR current.cap_singleton_writer<>r.cap_singleton_writer)),
		a.tool,a.variant,a.model,a.provider,
		a.machine_id,a.desired,a.desired_rev,a.observed,a.generation,a.recovery_block,m.room_id,
		COALESCE((SELECT b.inbox_id FROM boundary_pending b WHERE b.agent_id=a.id AND b.state='open'),''),COALESCE(a.approved_by,'')
		FROM agent a JOIN role r ON r.id=a.role_id JOIN room_member m ON m.member_id=a.id
			AND m.member_kind='agent' AND m.left_at IS NULL WHERE a.id=? ORDER BY m.joined_at LIMIT 1`, id).
		Scan(&card.ID, &card.Slug, &card.ProjectID, &card.RoleKey, &card.RoleEmoji, &card.RoleDeleted, &card.RoleUpdated, &card.Tool, &card.Variant,
			&card.Model, &card.Provider, &card.MachineID, &card.Desired, &card.DesiredRev,
			&card.Observed, &card.Generation, &card.RecoveryBlock, &card.RoomID, &card.BoundaryPending, &card.ApprovedBy)
	if err == nil {
		var item AgentSwitch
		switchErr := tx.tx.QueryRowContext(tx.ctx, `SELECT state,tool,variant,model,COALESCE(machine_id,''),error FROM agent_switch WHERE agent_id=?`, id).
			Scan(&item.State, &item.Tool, &item.Variant, &item.Model, &item.MachineID, &item.Error)
		if switchErr == nil {
			card.Switch = &item
		} else if !errors.Is(switchErr, sql.ErrNoRows) {
			return AgentCard{}, switchErr
		}
		var status, source, raw, deadline, overall string
		var turnOpen bool
		handoffErr := tx.tx.QueryRowContext(tx.ctx, `SELECT status,source,body_json,COALESCE(handoff_deadline,''),overall_deadline,
			CASE WHEN turn_id IS NULL THEN 0 ELSE EXISTS(SELECT 1 FROM turn WHERE id=switch_handoff.turn_id AND (exec IN ('pending','running') OR settle='pending')) END
			FROM switch_handoff WHERE agent_id=?`, id).
			Scan(&status, &source, &raw, &deadline, &overall, &turnOpen)
		if handoffErr == nil {
			if card.Switch == nil && status == "started" {
				card.Switch = &AgentSwitch{State: "started", Tool: card.Tool, Variant: card.Variant, Model: card.Model, MachineID: card.MachineID}
			}
			if card.Switch != nil {
				card.Switch.HandoffStatus = status
				card.Switch.HandoffSource = source
				card.Switch.HandoffDeadline = deadline
				card.Switch.OverallDeadline = overall
				if raw != "" {
					card.Switch.Handoff = json.RawMessage(raw)
				}
				card.Switch.setContinuityActions(turnOpen)
			}
		} else if !errors.Is(handoffErr, sql.ErrNoRows) {
			return AgentCard{}, handoffErr
		}
		card.BoundaryGrants, err = tx.boundaryGrants(id)
	}
	return card, err
}

func (tx *Tx) agentDisplay(agentID string) (string, error) {
	var slug, role string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.slug,r.role_key FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, agentID).Scan(&slug, &role)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (%s)", slug, role), nil
}
