package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// backgroundJobNoteBody is the system note an agent gets when its harness ran a query of its own between turns because a background job the
// agent had started ended (Claude Code's `task-notification`). The answer of such a query reaches nobody (it belongs to no turn), so the agent
// is given a real turn to look and report.
const backgroundJobNoteBody = "a background job of yours ended; check and report"

// eventBackgroundJobEnded records each such report: the wake (with the message it wrote) or why there was none.
const eventBackgroundJobEnded = "background_job_ended"

// RecordBackgroundJobEnded handles a supervisor's report that an agent's harness answered a background job's end between turns. It queues the
// system note backgroundJobNoteBody for the agent, in the room of its last ended normal turn, and says whether it did. At most one such note
// per turn: an agent whose jobs keep ending (or that keeps arming new ones) is woken once after each turn it took, not in a loop. Like a
// command's end, a stopped agent, a closed room or one the agent left gets no note, and the event records why (not_woken). A stale
// generation is refused with ErrStaleGeneration; an agent that has no ended turn yet (nothing it started can have ended) gets nothing.
func (s *Store) RecordBackgroundJobEnded(ctx context.Context, agentID string, generation int) (bool, error) {
	queued := false
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, desired string
		var agentGeneration int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,desired,generation FROM agent WHERE id=?`, agentID).
			Scan(&projectID, &desired, &agentGeneration); err != nil {
			return err
		}
		if agentGeneration != generation {
			return ErrStaleGeneration
		}
		var turnID, roomID string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,room_id FROM turn WHERE agent_id=? AND generation=? AND mode='normal'
			AND exec IN ('exited','killed') ORDER BY COALESCE(ended_at,'') DESC, id DESC LIMIT 1`, agentID, generation).Scan(&turnID, &roomID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var already bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE kind=? AND turn_id=?
			AND json_extract(payload_json,'$.not_woken') IS NULL)`, eventBackgroundJobEnded, turnID).Scan(&already); err != nil {
			return err
		}
		if already {
			return nil // once per turn
		}
		notWoken := ""
		var roomLifecycle string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle FROM room WHERE id=?`, roomID).Scan(&roomLifecycle); err != nil {
			return err
		}
		switch {
		case desired != "active":
			notWoken = "agent_" + desired
		case roomLifecycle != "open":
			notWoken = "room_closed"
		default:
			var member int
			if err := tx.tx.QueryRowContext(tx.ctx,
				`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`,
				roomID, agentID).Scan(&member); err != nil {
				return err
			}
			if member == 0 {
				notWoken = "not_member"
			}
		}
		payload := map[string]string{}
		if notWoken != "" {
			payload["not_woken"] = notWoken
			_, err := tx.AppendEvent(Event{Kind: eventBackgroundJobEnded, ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
				PayloadJSON: payloadJSON(payload)})
			return err
		}
		message, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: backgroundJobNoteBody})
		if err != nil {
			return err
		}
		if _, err := tx.InsertNote(Note{AgentID: agentID, MessageID: message.ID, RoomID: roomID, WakeReason: "background_job"}); err != nil {
			return err
		}
		payload["message_no"] = fmt.Sprint(message.No)
		if _, err := tx.AppendEvent(Event{Kind: eventBackgroundJobEnded, ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
			PayloadJSON: payloadJSON(payload)}); err != nil {
			return err
		}
		queued = true
		return nil
	})
	return queued, err
}
