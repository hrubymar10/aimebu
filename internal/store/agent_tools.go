package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type AgentToolIdentity struct {
	ID        string
	Slug      string
	ProjectID string
	Tool      string
	Provider  string
	IsLead    bool
	Desired   string
}

func (s *Store) AgentToolIdentityByKeyHash(ctx context.Context, hash string) (AgentToolIdentity, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentToolIdentity{}, err
	}
	var identity AgentToolIdentity
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.slug,a.project_id,a.tool,a.provider,r.cap_is_lead,a.desired
		FROM agent a JOIN role r ON r.id=a.role_id WHERE a.key_hash=? AND a.revoked_at IS NULL`, hash).
		Scan(&identity.ID, &identity.Slug, &identity.ProjectID, &identity.Tool, &identity.Provider, &identity.IsLead, &identity.Desired)
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
		WHERE r.project_id=? ORDER BY r.depth,r.created_at,r.id`, agentID, projectID)
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

// CreateTaskRoomAsTool makes the lead a member before any turn can address the
// new room. The parent notice and room creation share the request transaction.
func (tx *Tx) CreateTaskRoomAsTool(agentID, projectID, parentID, title string) (Room, error) {
	var lead bool
	var desired string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.cap_is_lead,a.desired FROM agent a JOIN role r ON r.id=a.role_id
		WHERE a.id=? AND a.project_id=? AND a.revoked_at IS NULL`, agentID, projectID).Scan(&lead, &desired); err != nil {
		return Room{}, err
	}
	if !lead || desired != "active" {
		return Room{}, ErrForbidden
	}
	if parentID == "" {
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home'`, projectID).Scan(&parentID); err != nil {
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
