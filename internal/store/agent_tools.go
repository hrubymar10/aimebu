package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/goccy/go-json"
)

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
		CASE WHEN length(m.body)>2000 THEN substr(m.body,1,2000)||'…' ELSE m.body END,m.created_at
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
	for rows.Next() {
		m := AgentMessage{Available: true, RoomID: roomID}
		if err := rows.Scan(&m.No, &m.Room, &m.From, &m.Body, &m.CreatedAt); err != nil {
			return AgentHistoryPage{}, err
		}
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		return AgentHistoryPage{}, err
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
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT r.kind,r.team_flow_room_key,r.title FROM room_member m JOIN room r ON r.id=m.room_id WHERE m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL AND r.kind<>'dm'`, agentID)
		if err != nil {
			return AgentCard{}, err
		}
		for rows.Next() {
			var kind, key, title string
			if err := rows.Scan(&kind, &key, &title); err != nil {
				rows.Close()
				return AgentCard{}, err
			}
			selector := flowRoomKind(kind, key, title)
			allowed := false
			for _, rule := range flow.JoinRules {
				if rule.RoomKind == selector && flowAllows(rule.AllowedRoles, roleKey) {
					allowed = true
					break
				}
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
	actor, err := tx.agentDisplay(actorID)
	if err != nil {
		return AgentCard{}, err
	}
	target, err := tx.agentDisplay(agentID)
	if err != nil {
		return AgentCard{}, err
	}
	var roomID string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM event WHERE agent_id=? AND kind='agent_spawn_requested' ORDER BY seq LIMIT 1`, agentID).Scan(&roomID)
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
	if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
		Body: fmt.Sprintf("%s changed %s's role to %s", actor, target, roleKey)}); err != nil {
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
		r.lifecycle,r.gate,r.lifecycle_rev,r.closed_at,
		`+roomMemberCountSQL+`,
		(SELECT COUNT(*) FROM message m WHERE m.room_id=r.id)
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
			&closed, &room.MemberCount, &room.MessageCount); err != nil {
			return nil, err
		}
		room.ParentID, room.DMKey, room.ClosedAt = parent.String, dm.String, closed.String
		out = append(out, room)
	}
	return out, rows.Err()
}

// CreateTaskRoomAsTool makes the coordinating agent a member before any turn can address the
// new room. The parent notice and room creation share the request transaction.
func (tx *Tx) CreateTaskRoomAsTool(agentID, projectID, parentID, title string) (Room, error) {
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
	var parentProject, parentKind, parentLifecycle string
	var joined bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,kind,lifecycle,EXISTS(SELECT 1 FROM room_member
		WHERE room_id=room.id AND member_kind='agent' AND member_id=? AND left_at IS NULL)
		FROM room WHERE id=?`, agentID, parentID).Scan(&parentProject, &parentKind, &parentLifecycle, &joined); err != nil {
		return Room{}, err
	}
	if parentProject != projectID || parentKind == "dm" || !joined {
		return Room{}, ErrForbidden
	}
	if parentLifecycle != "open" {
		return Room{}, ErrInvalidTransition
	}
	room, err := tx.CreateRoom(Room{ProjectID: projectID, ParentID: parentID, Kind: "task", Title: title, CreatedBy: agentID})
	if err != nil {
		return Room{}, err
	}
	// CreateRoom joined the leads quietly; the one that made the room is a member on purpose.
	if err := tx.AddMember(room.ID, "agent", agentID); err != nil {
		return Room{}, err
	}
	actor, err := tx.agentDisplay(agentID)
	if err != nil {
		return Room{}, err
	}
	if _, err := tx.InsertMessage(Message{RoomID: parentID, AuthorKind: "system", AuthorID: "system",
		Body: fmt.Sprintf("%s created task room %s", actor, room.Title)}); err != nil {
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
	if err := s.db.QueryRowContext(ctx, `SELECT max_live_agents,spawn_rate_per_min FROM settings_project WHERE project_id=?`, projectID).
		Scan(&result.MaxLiveAgents, &result.SpawnRateLimit); err != nil {
		return ProjectToolHeadroom{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE project_id=? AND (desired!='stopped' OR observed!='stopped') AND revoked_at IS NULL`, projectID).
		Scan(&result.LiveAgents); err != nil {
		return ProjectToolHeadroom{}, err
	}
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
	actor, err := tx.agentDisplay(actorID)
	if err != nil {
		return AgentCard{}, err
	}
	target, err := tx.agentDisplay(agentID)
	if err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.SetAgentDesiredByAgent(agentID, actorID, "stopped", rev); err != nil {
		return AgentCard{}, err
	}
	var roomID string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM event WHERE agent_id=? AND kind='agent_spawn_requested' ORDER BY seq LIMIT 1`, agentID).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM room_member WHERE member_kind='agent' AND member_id=? ORDER BY joined_at LIMIT 1`, agentID).Scan(&roomID)
	}
	if err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
		Body: fmt.Sprintf("%s stopped %s", actor, target)}); err != nil {
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
		COALESCE((SELECT b.inbox_id FROM boundary_pending b WHERE b.agent_id=a.id AND b.state='open'),'')
		FROM agent a JOIN role r ON r.id=a.role_id JOIN room_member m ON m.member_id=a.id
			AND m.member_kind='agent' AND m.left_at IS NULL WHERE a.id=? ORDER BY m.joined_at LIMIT 1`, id).
		Scan(&card.ID, &card.Slug, &card.ProjectID, &card.RoleKey, &card.RoleEmoji, &card.RoleDeleted, &card.RoleUpdated, &card.Tool, &card.Variant,
			&card.Model, &card.Provider, &card.MachineID, &card.Desired, &card.DesiredRev,
			&card.Observed, &card.Generation, &card.RecoveryBlock, &card.RoomID, &card.BoundaryPending)
	if err == nil {
		var item AgentSwitch
		switchErr := tx.tx.QueryRowContext(tx.ctx, `SELECT state,tool,variant,model,error FROM agent_switch WHERE agent_id=?`, id).
			Scan(&item.State, &item.Tool, &item.Variant, &item.Model, &item.Error)
		if switchErr == nil {
			card.Switch = &item
		} else if !errors.Is(switchErr, sql.ErrNoRows) {
			return AgentCard{}, switchErr
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
