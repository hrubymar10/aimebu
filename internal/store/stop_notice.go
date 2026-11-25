package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// workerStoppedEvent is the event kind that records a stop notice handed to a lead; the notice's output source and the prefetch read it.
const workerStoppedEvent = "agent_stopped_to_lead"

// NoteLeadOfStop tells the project's active lead that a person stopped a worker, with the end of what the worker was last doing: a note
// (reason worker_stopped) that the server gives the worker's last output tail, like a recovery's, under the same room-read rule. It is for a
// person's stop only (the agent tools' stop_agent and every system stop give none), and it is skipped when there is nothing new to hand over:
// the worker is a lead, there is no other active lead, the worker has had no turn since its last message, or the room the lead would be
// told in is closed. The system line the stop already posts is untouched.
func (tx *Tx) NoteLeadOfStop(workerID string) error {
	var projectID, slug string
	var isLead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,COALESCE(NULLIF(a.removed_slug,''),a.slug),r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, workerID).Scan(&projectID, &slug, &isLead); err != nil {
		return err
	}
	if isLead {
		return nil
	}
	var turnID, roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.id,t.room_id FROM turn t WHERE t.agent_id=? AND t.mode='normal' AND t.started_at IS NOT NULL
		AND t.started_at>COALESCE((SELECT MAX(m.created_at) FROM message m WHERE m.author_kind='agent' AND m.author_id=?),'')
		ORDER BY t.started_at DESC,t.id DESC LIMIT 1`, workerID, workerID).Scan(&turnID, &roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var leadID string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT a.id FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND a.id<>? AND r.cap_is_lead=1 AND a.desired='active' AND a.recovery_block=0 AND a.revoked_at IS NULL ORDER BY a.created_at,a.id LIMIT 1`, projectID, workerID).Scan(&leadID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	leadRoom, err := tx.leadRoomFor(projectID, roomID, leadID)
	if err != nil || leadRoom == "" {
		return err
	}
	var open bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle='open' FROM room WHERE id=?`, leadRoom).Scan(&open); err != nil {
		return err
	}
	if !open {
		return nil
	}
	message, err := tx.InsertMessage(Message{RoomID: leadRoom, AuthorKind: "system", AuthorID: "system", OnlyTargets: []string{},
		Body: fmt.Sprintf("A person stopped %s (agent %s). Its last turn was %s in room %s. [Worker output](/api/rooms/%s/agents/%s/output).", slug, workerID, turnID, roomID, leadRoom, workerID)})
	if err != nil {
		return err
	}
	if _, err := tx.InsertNote(Note{AgentID: leadID, MessageID: message.ID, RoomID: leadRoom, WakeReason: "worker_stopped"}); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: workerStoppedEvent, ProjectID: projectID, RoomID: roomID, AgentID: workerID, TurnID: turnID,
		PayloadJSON: payloadJSON(map[string]string{"lead_id": leadID, "message_id": message.ID})})
	return err
}
