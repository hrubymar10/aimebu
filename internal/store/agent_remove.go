package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrAgentNotStopped is the refusal to remove an agent that is not stopped: a process may still be running for it.
var ErrAgentNotStopped = errors.New("agent_not_stopped")

// AgentRemoval is what removing an agent needs to know about it before anything is touched.
type AgentRemoval struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	ProjectID string `json:"project_id"`
	Desired   string `json:"desired"`
	Observed  string `json:"observed"`
}

// Stopped says no process of the agent runs and none may start: the state an agent must be in to be removed.
func (a AgentRemoval) Stopped() bool { return a.Desired == "stopped" && a.Observed == "stopped" }

func agentRemovalIn(row interface{ Scan(...any) error }, id string) (AgentRemoval, error) {
	a := AgentRemoval{ID: id}
	var revoked bool
	if err := row.Scan(&a.Slug, &a.ProjectID, &a.Desired, &a.Observed, &revoked); err != nil {
		return AgentRemoval{}, err
	}
	if revoked {
		return AgentRemoval{}, sql.ErrNoRows // an agent that is removed already is not found
	}
	return a, nil
}

const agentRemovalSelect = `SELECT slug,project_id,desired,observed,revoked_at IS NOT NULL FROM agent WHERE id=?`

// AgentRemovalInfo reads the agent for a removal. A removed agent is not found.
func (s *Store) AgentRemovalInfo(ctx context.Context, id string) (AgentRemoval, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentRemoval{}, err
	}
	return agentRemovalIn(s.readDB.QueryRowContext(ctx, agentRemovalSelect, id), id)
}

// RemoveAgent takes a stopped agent out for good, in one transaction: it leaves every room it is in (its queued notes are dropped, and a chat
// between agents left with fewer than two of them is closed), the inbox items that waited on it are withdrawn, and it is revoked, which frees its
// role's slot, its name and its checkout's writer slot and stops its key. Its messages, turns and history stay; they name it as removed. An agent
// that is not stopped is refused (ErrAgentNotStopped), one that is gone or removed already is not found. The worktree of its own, if any, is the
// caller's to deal with before this.
func (s *Store) RemoveAgent(ctx context.Context, id, actorID string) (AgentRemoval, error) {
	var removed AgentRemoval
	err := s.Tx(ctx, func(tx *Tx) error {
		agent, err := agentRemovalIn(tx.tx.QueryRowContext(tx.ctx, agentRemovalSelect, id), id)
		if err != nil {
			return err
		}
		if !agent.Stopped() {
			return ErrAgentNotStopped
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT room_id FROM room_member WHERE member_kind='agent' AND member_id=? AND left_at IS NULL ORDER BY room_id`, id)
		if err != nil {
			return err
		}
		var rooms []string
		for rows.Next() {
			var roomID string
			if err := rows.Scan(&roomID); err != nil {
				rows.Close()
				return err
			}
			rooms = append(rooms, roomID)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, roomID := range rooms {
			if err := tx.RemoveMember(roomID, "agent", id); err != nil {
				return err
			}
			if err := tx.closeDesertedAgentChat(roomID); err != nil {
				return err
			}
		}
		if err := tx.withdrawAgentInbox(id, agent.ProjectID, actorID); err != nil {
			return err
		}
		if err := tx.pauseSchedulesTargeting(id, agent.Slug); err != nil {
			return err
		}
		// The name is free for another agent from here on: the row keeps a tombstone in its place (slug is unique within the project) and the name it had
		// for its messages to be shown under.
		if _, err := tx.Exec(`UPDATE agent SET removed_slug=slug,slug=slug||'~'||id WHERE id=?`, id); err != nil {
			return err
		}
		if err := tx.revokeAgent(id, map[string]string{"slug": agent.Slug, "removed_by": actorID}); err != nil {
			return err
		}
		removed = agent
		return nil
	})
	return removed, err
}

// closeDesertedAgentChat closes a chat between agents that has fewer than two agents in it after one left. A room of a real project is left alone.
func (tx *Tx) closeDesertedAgentChat(roomID string) error {
	var projectID, lifecycle string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,lifecycle FROM room WHERE id=?`, roomID).Scan(&projectID, &lifecycle); err != nil {
		return err
	}
	if projectID != AgentDMProjectID || lifecycle == "closed" {
		return nil
	}
	var left int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND left_at IS NULL`, roomID).Scan(&left); err != nil {
		return err
	}
	if left >= 2 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE room SET lifecycle='closed',closed_at=?,lifecycle_rev=lifecycle_rev+1 WHERE id=?`, timestamp(), roomID); err != nil {
		return err
	}
	_, err := tx.AppendEvent(Event{Kind: "room_closed", ProjectID: AgentDMProjectID, RoomID: roomID, PayloadJSON: payloadJSON(map[string]string{"room_id": roomID})})
	return err
}

// withdrawAgentInbox withdraws what waited on the agent in the Inbox (a recovery decision, a folder approval, a question it asked): nobody is left
// to act on the answer.
func (tx *Tx) withdrawAgentInbox(agentID, projectID, actorID string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,room_id FROM inbox_item WHERE agent_id=? AND state='open' ORDER BY id`, agentID)
	if err != nil {
		return err
	}
	type item struct{ id, roomID string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.roomID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`UPDATE inbox_item SET state='withdrawn',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=? AND state='open'`, "agent removed by "+actorID, timestamp(), it.id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE boundary_pending SET state='withdrawn' WHERE inbox_id=? AND state='open'`, it.id); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: it.roomID, AgentID: agentID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": it.id, "resolution": "withdrawn", "reason": "agent removed"})}); err != nil {
			return err
		}
	}
	return nil
}

// pauseSchedulesTargeting pauses the schedules aimed at an agent that is going away, each with a notice in its room: they would skip every run from
// here on, and a recurring one would never stop.
func (tx *Tx) pauseSchedulesTargeting(agentID, slug string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,room_id FROM schedule WHERE target_agent_id=? AND enabled=1 ORDER BY id`, agentID)
	if err != nil {
		return err
	}
	type item struct{ id, roomID string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.roomID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`UPDATE schedule SET enabled=0,rev=rev+1,updated_at=? WHERE id=?`, timestamp(), it.id); err != nil {
			return err
		}
		if _, err := tx.InsertMessage(Message{RoomID: it.roomID, AuthorKind: "system", AuthorID: "system", Body: fmt.Sprintf("Schedule %s paused: %s was removed", it.id, slug)}); err != nil {
			return err
		}
	}
	return nil
}
