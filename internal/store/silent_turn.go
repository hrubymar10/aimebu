package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
)

// silentTurnEvent is the event kind that records a silent-turn notice; the debounce reads it.
const silentTurnEvent = "silent_turn"

// silentTurnNotice runs after a normal turn committed an answer with no message. A worker that was addressed (a note from an agent or a person
// that is not a broadcast or a group tag) and ended its turn without a word may have left work behind (a harness background job that will
// never wake it), so each room the turn was addressed in gets the system line "<agent> ended its turn without a message", and every agent
// that addressed it is woken (reason silent_turn) to check whether it still owes something. A turn that added a reaction has answered (the
// prompt asks for a reaction instead of an acknowledgement message), so it is not announced. A lead has nobody above it to tell, and a turn
// woken only by system notes (a command's end, a scheduled note), a broadcast or a group tag owes nobody an answer. It is said once until the
// agent next posts a message, so a worker that keeps answering nothing does not wake its author again and again; the notice itself is a system
// note for the author, so the author's own silent answer to it never raises another one.
func (tx *Tx) silentTurnNotice(turn answerTurn) error {
	var isLead bool
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.cap_is_lead,a.project_id FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, turn.agentID).Scan(&isLead, &projectID); err != nil {
		return err
	}
	if isLead {
		return nil
	}
	// A reaction is how the prompt asks agents to acknowledge, so a turn that reacted to anything has answered: it is not silent. The turn's
	// window starts when it started running, or (for a turn never marked running) when its first note was queued.
	var reacted bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM reaction WHERE member_kind='agent' AND member_id=? AND created_at>=
		COALESCE((SELECT started_at FROM turn WHERE id=?),(SELECT MIN(n.created_at) FROM turn_note tn JOIN note n ON n.id=tn.note_id WHERE tn.turn_id=?),''))`,
		turn.agentID, turn.id, turn.id).Scan(&reacted); err != nil {
		return err
	}
	if reacted {
		return nil
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT n.room_id,m.author_kind,m.author_id FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id
		WHERE tn.turn_id=? AND n.wake_reason NOT IN ('broadcast','group') AND m.author_kind IN ('agent','user') AND NOT (m.author_kind='agent' AND m.author_id=?)
		ORDER BY m.seq`, turn.id, turn.agentID)
	if err != nil {
		return err
	}
	type addressed struct{ roomID, authorKind, authorID string }
	var notes []addressed
	for rows.Next() {
		var item addressed
		if err := rows.Scan(&item.roomID, &item.authorKind, &item.authorID); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(notes) == 0 {
		return nil
	}
	// Debounce: a notice already given since the agent's last message is not repeated. Messages and events share one sequence.
	var lastSeq int64
	var lastAt sql.NullString
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(MAX(seq),0),MAX(created_at) FROM message WHERE author_kind='agent' AND author_id=?`, turn.agentID).Scan(&lastSeq, &lastAt); err != nil {
		return err
	}
	var already bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE agent_id=? AND created_at>=? AND kind=? AND seq>?)`, turn.agentID, lastAt.String, silentTurnEvent, lastSeq).Scan(&already); err != nil {
		return err
	}
	if already {
		return nil
	}
	lines := map[string]string{} // room → the notice's message ID
	woken := map[string]bool{}
	var authors []string
	for _, item := range notes {
		var open bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle='open' FROM room WHERE id=?`, item.roomID).Scan(&open); err != nil {
			return err
		}
		if !open {
			continue
		}
		messageID, ok := lines[item.roomID]
		if !ok {
			// Shown to the room, waking nobody by itself (OnlyTargets empty): only the agents that addressed the worker are woken, below.
			// The notice names the worker by its stored @agt_<id> token (names are reusable); the human view translates it.
			message, err := tx.InsertMessage(Message{RoomID: item.roomID, AuthorKind: "system", AuthorID: "system", Body: "@" + turn.agentID + " ended its turn without a message", OnlyTargets: []string{}})
			if err != nil {
				return err
			}
			messageID = message.ID
			lines[item.roomID] = messageID
		}
		if item.authorKind != "agent" || woken[item.authorID] {
			continue
		}
		var live bool
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.revoked_at IS NULL AND EXISTS(SELECT 1 FROM room_member rm WHERE rm.room_id=? AND rm.member_kind='agent' AND rm.member_id=a.id AND rm.left_at IS NULL)
			FROM agent a WHERE a.id=?`, item.roomID, item.authorID).Scan(&live)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if !live {
			continue
		}
		if _, err := tx.InsertNote(Note{AgentID: item.authorID, MessageID: messageID, RoomID: item.roomID, WakeReason: silentTurnEvent}); err != nil {
			return err
		}
		woken[item.authorID] = true
		authors = append(authors, item.authorID)
	}
	if len(lines) == 0 {
		return nil
	}
	// notices names the notice messages, so a delivery of one to an agent that addressed the worker can add the end of this turn's output.
	notices := make([]string, 0, len(lines))
	for _, messageID := range lines {
		notices = append(notices, messageID)
	}
	slices.Sort(notices)
	_, err = tx.AppendEvent(Event{Kind: silentTurnEvent, ProjectID: projectID, RoomID: turn.roomID, AgentID: turn.agentID, TurnID: turn.id, PayloadJSON: payloadJSON(map[string]string{"woken": strings.Join(authors, ","), "notices": strings.Join(notices, ",")})})
	return err
}
