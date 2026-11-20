package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrHomeRoom = errors.New("home_room")

// ErrFlowRoom: the room is one of the team flow's own (general, routing, advice); an agent tool cannot close it.
var ErrFlowRoom = errors.New("flow_room")
var ErrOpenChildren = errors.New("open_children")

type RoomCloseFailure struct {
	RoomID string
	Err    error
}

// BeginRoomClose blocks new dispatch before any running turn is interrupted.
// A cascade records every descendant as closing in the same transaction.
func (s *Store) BeginRoomClose(ctx context.Context, id, actor string, rev int64, cascade bool) (Room, []AgentCard, error) {
	controls := []AgentCard{}
	err := s.Tx(ctx, func(tx *Tx) error {
		var closeErr error
		controls, closeErr = tx.beginRoomClose(id, actor, rev, cascade)
		return closeErr
	})
	if err != nil {
		return Room{}, nil, err
	}
	room, err := s.RoomByID(ctx, id)
	return room, controls, err
}

// beginRoomClose is shared with the idempotent lead toolbox route.
func (tx *Tx) beginRoomClose(id, actor string, rev int64, cascade bool) (controls []AgentCard, err error) {
	controls = []AgentCard{}
	err = func() error {
		var projectID, kind, lifecycle string
		var currentRev int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,kind,lifecycle,lifecycle_rev FROM room WHERE id=?`, id).Scan(&projectID, &kind, &lifecycle, &currentRev); err != nil {
			return err
		}
		if err := tx.roomClosePermission(projectID, id, kind, actor); err != nil {
			return err
		}
		var agentActor bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent WHERE id=?)`, actor).Scan(&agentActor); err != nil {
			return err
		}
		if kind == "home" {
			return ErrHomeRoom
		}
		if rev != currentRev {
			return ErrStaleRev
		}
		if lifecycle != "open" {
			return nil
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `WITH RECURSIVE descendants(id,depth) AS (
			SELECT id,depth FROM room WHERE id=? UNION ALL
			SELECT r.id,r.depth FROM room r JOIN descendants d ON r.parent_id=d.id
		) SELECT r.id,r.lifecycle FROM descendants d JOIN room r ON r.id=d.id ORDER BY d.depth DESC`, id)
		if err != nil {
			return err
		}
		type target struct{ id, lifecycle string }
		targets := []target{}
		for rows.Next() {
			var x target
			if err := rows.Scan(&x.id, &x.lifecycle); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !cascade {
			for _, x := range targets {
				if x.id != id && x.lifecycle != "closed" {
					return ErrOpenChildren
				}
			}
		}
		for _, x := range targets {
			if x.lifecycle != "open" || !cascade && x.id != id {
				continue
			}
			if _, err := tx.Exec(`UPDATE room SET lifecycle='closing',lifecycle_rev=lifecycle_rev+1 WHERE id=?`, x.id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE schedule SET enabled=0,rev=rev+1,updated_at=? WHERE room_id=? AND enabled=1`, timestamp(), x.id); err != nil {
				return err
			}
			pendingRows, err := tx.tx.QueryContext(tx.ctx, `SELECT b.agent_id FROM boundary_pending b JOIN inbox_item i ON i.id=b.inbox_id
				WHERE i.room_id=? AND b.state='open'`, x.id)
			if err != nil {
				return err
			}
			var pendingAgents []string
			for pendingRows.Next() {
				var agentID string
				if err := pendingRows.Scan(&agentID); err != nil {
					pendingRows.Close()
					return err
				}
				pendingAgents = append(pendingAgents, agentID)
			}
			err = pendingRows.Err()
			pendingRows.Close()
			if err != nil {
				return err
			}
			for _, agentID := range pendingAgents {
				if err := tx.withdrawBoundaryPending(agentID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE inbox_item SET state='withdrawn',rev=rev+1,resolved_at=? WHERE room_id=? AND state='open' AND kind IN ('question','approval')`, timestamp(), x.id); err != nil {
				return err
			}
			actorKind := "user"
			if agentActor {
				actorKind = "agent"
			}
			if _, err := tx.AppendEvent(Event{Kind: "room_closing", ProjectID: projectID, RoomID: x.id, PayloadJSON: payloadJSON(map[string]string{"room_id": x.id, "actor_kind": actorKind, "actor_id": actor})}); err != nil {
				return err
			}
			cards, err := tx.roomHomeAgents(x.id)
			if err != nil {
				return err
			}
			for _, card := range cards {
				if card.Desired != "active" {
					continue
				}
				var updated AgentCard
				if agentActor {
					updated, err = tx.SetAgentDesiredByAgent(card.ID, actor, "not_accepting", card.DesiredRev)
				} else {
					updated, err = tx.SetAgentDesired(card.ID, actor, "not_accepting", card.DesiredRev)
				}
				if err != nil {
					return err
				}
				controls = append(controls, updated)
			}
		}
		return nil
	}()
	return controls, err
}

func (tx *Tx) CloseTaskRoomAsTool(id, actor string, rev int64, cascade bool) (Room, []AgentCard, error) {
	var wasOpen bool
	var kind, flowKey string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle='open',kind,team_flow_room_key FROM room WHERE id=?`, id).Scan(&wasOpen, &kind, &flowKey); err != nil {
		return Room{}, nil, err
	}
	if kind == "home" {
		return Room{}, nil, ErrHomeRoom
	}
	if flowKey != "" {
		return Room{}, nil, ErrFlowRoom
	}
	controls, err := tx.beginRoomClose(id, actor, rev, cascade)
	if err != nil {
		return Room{}, nil, err
	}
	var room Room
	var parent, dm, closed sql.NullString
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT id,project_id,parent_id,depth,kind,dm_key,title,created_by,lifecycle,gate,lifecycle_rev,closed_at FROM room WHERE id=?`, id).
		Scan(&room.ID, &room.ProjectID, &parent, &room.Depth, &room.Kind, &dm, &room.Title, &room.CreatedBy, &room.Lifecycle, &room.Gate, &room.LifecycleRev, &closed)
	if err != nil {
		return Room{}, nil, err
	}
	room.ParentID, room.DMKey, room.ClosedAt = parent.String, dm.String, closed.String
	if wasOpen && room.Lifecycle == "closing" && room.ParentID != "" {
		actorName, err := tx.agentDisplay(actor)
		if err != nil {
			return Room{}, nil, err
		}
		if _, err := tx.InsertMessage(Message{RoomID: room.ParentID, AuthorKind: "system", AuthorID: "system",
			Body: actorName + " archived task room " + room.Title}); err != nil {
			return Room{}, nil, err
		}
	}
	return room, controls, nil
}

func (tx *Tx) roomLifecyclePermission(projectID, actor string) error {
	var role string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, actor).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if role != "owner" && role != "operator" {
		return ErrForbidden
	}
	return nil
}

// A private chat belongs to its human peer. The project owner may also
// archive or reopen it, but other project members may not manage it.
func (tx *Tx) roomClosePermission(projectID, roomID, kind, actor string) error {
	// An agent manages a task room it belongs to or one it created; a
	// coordinator may have left a room it opened, or never been joined to it.
	var joined, active bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT
		EXISTS(SELECT 1 FROM room_member m WHERE m.room_id=? AND m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL)
			OR EXISTS(SELECT 1 FROM room x WHERE x.id=? AND x.created_by=a.id),
		a.desired='active' AND a.revoked_at IS NULL
		FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=? AND a.project_id=?`, roomID, roomID, actor, projectID).
		Scan(&joined, &active)
	if err == nil {
		if kind == "task" && joined && active {
			return tx.authorizeSpawnActor(projectID, "agent", actor, false)
		}
		// A flow coordinator may also close a task room directly under the home or a flow's general room: rooms made
		// before a flow switch (by the leader) have no dispatcher member, and joining each would be a larger change.
		if kind == "task" && active {
			var underShared bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room x JOIN room p ON p.id=x.parent_id
				WHERE x.id=? AND (p.kind='home' OR p.team_flow_room_key='general'))`, roomID).Scan(&underShared); err != nil {
				return err
			}
			if underShared {
				return tx.authorizeSpawnActor(projectID, "agent", actor, false)
			}
		}
		return ErrForbidden
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if kind != "dm" {
		return tx.roomLifecyclePermission(projectID, actor)
	}
	var role string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, actor).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if role == "owner" {
		return nil
	}
	var peer bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member
		WHERE room_id=? AND member_kind='user' AND member_id=? AND left_at IS NULL)`, roomID, actor).Scan(&peer); err != nil {
		return err
	}
	if !peer {
		return ErrForbidden
	}
	return nil
}

func (tx *Tx) roomHomeAgents(roomID string) ([]AgentCard, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT DISTINCT a.id,a.desired,a.desired_rev,a.generation FROM agent a
		JOIN room_member m ON m.member_kind='agent' AND m.member_id=a.id AND m.room_id=? AND m.left_at IS NULL
		WHERE a.home_room_id=? ORDER BY a.id`, roomID, roomID)
	if err != nil {
		return nil, err
	}
	cards := []AgentCard{}
	for rows.Next() {
		var card AgentCard
		if err := rows.Scan(&card.ID, &card.Desired, &card.DesiredRev, &card.Generation); err != nil {
			rows.Close()
			return nil, err
		}
		cards = append(cards, card)
	}
	err = rows.Err()
	rows.Close()
	return cards, err
}

// AdvanceRoomClosures is safe to repeat after a restart. The close-start event
// supplies the grace-period clock, and the ordinary Stop transition handles
// pending and running turns and their recovery state.
func (s *Store) AdvanceRoomClosures(ctx context.Context, grace time.Duration) ([]AgentCard, []RoomCloseFailure, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM room WHERE lifecycle='closing' ORDER BY depth DESC,created_at,id`)
	if err != nil {
		return nil, nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	controls := []AgentCard{}
	failures := []RoomCloseFailure{}
	for _, id := range ids {
		roomControls := []AgentCard{}
		err := s.Tx(ctx, func(tx *Tx) error {
			var projectID, lifecycle, started string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,lifecycle FROM room WHERE id=?`, id).Scan(&projectID, &lifecycle); err != nil {
				return err
			}
			if lifecycle != "closing" {
				return nil
			}
			var children int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room WHERE parent_id=? AND lifecycle!='closed'`, id).Scan(&children); err != nil {
				return err
			}
			if children != 0 {
				return nil
			}
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT created_at FROM event WHERE room_id=? AND kind='room_closing' ORDER BY seq DESC LIMIT 1`, id).Scan(&started); err != nil {
				return err
			}
			begin, err := time.Parse(timestampLayout, started)
			if err != nil {
				return err
			}
			var actor string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT user_id FROM membership WHERE project_id=? AND role='owner' ORDER BY user_id LIMIT 1`, projectID).Scan(&actor); err != nil {
				return err
			}
			cards, err := tx.roomHomeAgents(id)
			if err != nil {
				return err
			}
			withinGrace := time.Since(begin) < grace
			waiting := false
			if withinGrace {
				var roomRunning int
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE room_id=? AND exec='running'`, id).Scan(&roomRunning); err != nil {
					return err
				}
				waiting = roomRunning > 0
			}
			blocked := false
			for _, card := range cards {
				if card.Desired == "stopped" {
					continue
				}
				if withinGrace {
					var agentRunning int
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, card.ID).Scan(&agentRunning); err != nil {
						return err
					}
					if agentRunning > 0 {
						waiting = true
						continue
					}
				}
				updated, err := tx.SetAgentDesired(card.ID, actor, "stopped", card.DesiredRev)
				if err != nil {
					if errors.Is(err, ErrInvalidTransition) {
						blocked = true
						var already bool
						if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE kind='room_close_blocked' AND room_id=? AND agent_id=?)`, id, card.ID).Scan(&already); err != nil {
							return err
						}
						if !already {
							if _, err := tx.AppendEvent(Event{Kind: "room_close_blocked", ProjectID: projectID, RoomID: id, AgentID: card.ID,
								PayloadJSON: payloadJSON(map[string]string{"agent_id": card.ID, "reason": "agent_stop_invalid_transition"})}); err != nil {
								return err
							}
						}
						continue
					}
					return err
				}
				roomControls = append(roomControls, updated)
			}
			if blocked || waiting {
				return nil
			}
			if _, err := tx.Exec(`UPDATE outbox SET state='held',held_reason='room_closed' WHERE room_id=? AND state IN ('proposed','validated')`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE room SET lifecycle='closed',lifecycle_rev=lifecycle_rev+1,closed_at=? WHERE id=?`, timestamp(), id); err != nil {
				return err
			}
			if err := tx.settleRoomInbox(id); err != nil {
				return err
			}
			_, err = tx.AppendEvent(Event{Kind: "room_closed", ProjectID: projectID, RoomID: id, PayloadJSON: payloadJSON(map[string]string{"room_id": id})})
			return err
		})
		if err != nil {
			failures = append(failures, RoomCloseFailure{RoomID: id, Err: err})
			continue
		}
		controls = append(controls, roomControls...)
	}
	return controls, failures, nil
}

func (s *Store) ReopenRoom(ctx context.Context, id, actor string, rev int64) (Room, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, kind, lifecycle string
		var parentID sql.NullString
		var currentRev int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,parent_id,kind,lifecycle,lifecycle_rev FROM room WHERE id=?`, id).Scan(&projectID, &parentID, &kind, &lifecycle, &currentRev); err != nil {
			return err
		}
		if err := tx.roomClosePermission(projectID, id, kind, actor); err != nil {
			return err
		}
		if rev != currentRev {
			return ErrStaleRev
		}
		if lifecycle == "open" {
			return nil
		}
		if lifecycle != "closed" {
			return ErrInvalidTransition
		}
		if parentID.Valid {
			var parentLifecycle string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle FROM room WHERE id=?`, parentID.String).Scan(&parentLifecycle); err != nil {
				return err
			}
			if parentLifecycle != "open" {
				return ErrInvalidTransition
			}
		}
		// Daily limits are per project now, so no room is paused by a budget of its own.
		if _, err := tx.Exec(`UPDATE room SET lifecycle='open',gate=CASE WHEN control_desired='active' THEN 'open' ELSE 'paused' END,
			lifecycle_rev=lifecycle_rev+1,closed_at=NULL WHERE id=?`, id); err != nil {
			return err
		}
		// A closed room gets no automatic lead, so a lead that arrived or was backfilled meanwhile is put in now.
		if kind != "dm" {
			if err := tx.joinLeadsToRoom(id, projectID); err != nil {
				return err
			}
		}
		_, err := tx.AppendEvent(Event{Kind: "room_reopened", ProjectID: projectID, RoomID: id, PayloadJSON: payloadJSON(map[string]string{"room_id": id})})
		return err
	})
	if err != nil {
		return Room{}, err
	}
	return s.RoomByID(ctx, id)
}
