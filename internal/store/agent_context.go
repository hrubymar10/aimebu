package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RequestAgentContextClear holds new dispatches until the current turn has
// settled. The request is durable across a server restart.
func (tx *Tx) RequestAgentContextClear(id, actorID string, rev int64) (AgentCard, error) {
	var projectID, desired string
	var blocked bool
	var generation int
	var currentRev int64
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,desired,generation,desired_rev,recovery_block FROM agent WHERE id=? AND revoked_at IS NULL`, id).
		Scan(&projectID, &desired, &generation, &currentRev, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentCard{}, ErrForbidden
	}
	if err != nil {
		return AgentCard{}, err
	}
	if err := tx.authorizeSpawnActor(projectID, "user", actorID, false); err != nil {
		return AgentCard{}, err
	}
	if rev != currentRev {
		return AgentCard{}, ErrStaleRev
	}
	if desired != "active" || blocked {
		return AgentCard{}, ErrInvalidTransition
	}
	var switching bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent_switch WHERE agent_id=? AND state IN ('draining','starting','stopping'))`, id).Scan(&switching); err != nil {
		return AgentCard{}, err
	}
	if switching {
		return AgentCard{}, ErrInvalidTransition
	}
	if err := tx.refuseArchived(projectID); err != nil {
		return AgentCard{}, err
	}
	var pending bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent_context_clear WHERE agent_id=?)`, id).Scan(&pending); err != nil {
		return AgentCard{}, err
	}
	if pending {
		return AgentCard{}, ErrInvalidTransition
	}
	if _, err := tx.Exec(`INSERT INTO agent_context_clear(agent_id,generation,requested_by,requested_at) VALUES (?,?,?,?)`, id, generation, actorID, timestamp()); err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.Exec(`UPDATE agent SET desired_rev=desired_rev+1 WHERE id=?`, id); err != nil {
		return AgentCard{}, err
	}
	return tx.agentCardByID(id)
}

// AdvanceAgentContextClears starts a fresh generation only after every prior
// turn has finished, so a request never interrupts work already in progress.
func (s *Store) AdvanceAgentContextClears(ctx context.Context) ([]AgentCard, error) {
	var changed []AgentCard
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT agent_id,generation,requested_by FROM agent_context_clear ORDER BY requested_at,agent_id`)
		if err != nil {
			return err
		}
		type pending struct {
			id, by     string
			generation int
		}
		var items []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.generation, &p.by); err != nil {
				rows.Close()
				return err
			}
			items = append(items, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, p := range items {
			var projectID, roomID, slug, tool, desired, observed string
			var generation int
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,COALESCE(home_room_id,''),slug,tool,desired,observed,generation FROM agent WHERE id=? AND revoked_at IS NULL`, p.id).
				Scan(&projectID, &roomID, &slug, &tool, &desired, &observed, &generation)
			if errors.Is(err, sql.ErrNoRows) {
				_, err = tx.Exec(`DELETE FROM agent_context_clear WHERE agent_id=?`, p.id)
				if err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if roomID == "" {
				err := tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM room_member WHERE member_kind='agent' AND member_id=? AND left_at IS NULL ORDER BY joined_at LIMIT 1`, p.id).Scan(&roomID)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			if generation != p.generation {
				_, err = tx.Exec(`DELETE FROM agent_context_clear WHERE agent_id=?`, p.id)
				if err != nil {
					return err
				}
				continue
			}
			if desired == "not_accepting" {
				continue
			} // Pause already retires warm work; resume before making the fresh generation.
			if observed != "idle" && observed != "stopped" {
				continue
			}
			var open int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn t WHERE t.agent_id=? AND (t.exec IN ('pending','running') OR t.settle='pending' OR (t.exec='lost' AND NOT EXISTS(SELECT 1 FROM event e WHERE e.kind='recovery_override' AND (e.turn_id=t.id OR e.turn_id=t.parent_turn_id))))`, p.id).Scan(&open); err != nil {
				return err
			}
			if open != 0 {
				continue
			}
			id, err := newID("ses")
			if err != nil {
				return err
			}
			handle, err := nativeSessionID(tool)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE session SET ended_at=? WHERE agent_id=? AND generation=? AND ended_at IS NULL`, timestamp(), p.id, generation); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,native_session_id,start_mode,started_at) VALUES (?,?,?,?,'fresh_with_history',?)`, id, p.id, generation+1, nullableString(handle), timestamp()); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE agent SET generation=generation+1,desired_rev=desired_rev+1 WHERE id=?`, p.id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM agent_context_clear WHERE agent_id=?`, p.id); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "agent_context_cleared", ProjectID: projectID, RoomID: roomID, AgentID: p.id,
				PayloadJSON: payloadJSON(map[string]string{"actor_id": p.by, "actor_kind": "user"})}); err != nil {
				return err
			}
			if roomID != "" {
				if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: fmt.Sprintf("%s's context was cleared", slug)}); err != nil {
					return err
				}
			}
			if desired != "stopped" {
				if _, err := tx.AppendEvent(Event{Kind: "agent_spawn_requested", ProjectID: projectID, RoomID: roomID, AgentID: p.id,
					PayloadJSON: fmt.Sprintf(`{"generation":%d}`, generation+1)}); err != nil {
					return err
				}
			}
			card, err := tx.agentCardByID(p.id)
			if err != nil {
				return err
			}
			changed = append(changed, card)
		}
		return nil
	})
	return changed, err
}
