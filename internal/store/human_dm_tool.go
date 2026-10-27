package store

import (
	"database/sql"
	"errors"
	"time"
)

const (
	// HumanDMNewChatsPerHour is how many new chats one agent may open with people in an hour.
	HumanDMNewChatsPerHour = 10
	// HumanDMMessagesPerHour is how many messages one agent may write into one chat with a person in an hour: a person is not a queue.
	HumanDMMessagesPerHour = 20
)

// ErrHumanDMCap is a chat with a person that already took the agent's messages for the hour.
var ErrHumanDMCap = errors.New("human_dm_cap")

// ErrHumanDMFanOutCap is an agent that already opened its hourly share of new chats with people.
var ErrHumanDMFanOutCap = errors.New("human_dm_fanout_cap")

// HumanDMResult is what dm_human returns: the chat and the message that went into it.
type HumanDMResult struct {
	RoomID    string `json:"room_id"`
	MessageID string `json:"message_id"`
	No        int64  `json:"no"`
	Created   bool   `json:"created"`
}

// DMHumanAsTool is an agent's dm_human call: it opens (or finds) the private chat between the agent and a person and posts the message into
// it. Any active agent may, a worker as much as a lead, and the server decides everything here, never the model: the person must be an active
// member of the agent's own project (owner, operator or viewer: a chat with them is only talk), the project must not be archived, and an agent
// writes only so many messages a chat and opens only so many new chats an hour. Agent to agent stays dm_agent, and lead only. A person who does
// not exist, is disabled or is not in the project answers alike (ErrForbidden), so a login cannot be probed. target is the person's login or id.
func (tx *Tx) DMHumanAsTool(callerID, target, body string) (HumanDMResult, error) {
	var project, desired string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,desired FROM agent WHERE id=? AND revoked_at IS NULL`, callerID).Scan(&project, &desired); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HumanDMResult{}, ErrForbidden
		}
		return HumanDMResult{}, err
	}
	if desired != "active" {
		return HumanDMResult{}, ErrForbidden
	}
	var userID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT u.id FROM user u JOIN membership m ON m.user_id=u.id AND m.project_id=?
		WHERE (u.id=? OR u.login=?) AND u.disabled_at IS NULL ORDER BY u.id LIMIT 1`, project, target, target).Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HumanDMResult{}, ErrForbidden
		}
		return HumanDMResult{}, err
	}
	if err := tx.refuseArchived(project); err != nil {
		return HumanDMResult{}, err
	}
	cutoff := time.Now().UTC().Add(-time.Hour).Format(timestampLayout)
	var existing string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE dm_key=?`, userAgentDMKey(userID, callerID)).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Only the chats with people count: a lead's chats with other agents (dm_agent) have their own cap.
		var opened int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room WHERE kind='dm' AND dm_key LIKE 'user:%' AND created_by=? AND created_at>=?`, callerID, cutoff).Scan(&opened); err != nil {
			return HumanDMResult{}, err
		}
		if opened >= HumanDMNewChatsPerHour {
			return HumanDMResult{}, ErrHumanDMFanOutCap
		}
	case err != nil:
		return HumanDMResult{}, err
	}
	roomID, created, err := tx.openUserAgentDM(userID, callerID, callerID)
	if err != nil {
		return HumanDMResult{}, err
	}
	var lifecycle string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle FROM room WHERE id=?`, roomID).Scan(&lifecycle); err != nil {
		return HumanDMResult{}, err
	}
	if lifecycle != "open" {
		return HumanDMResult{}, ErrInvalidTransition
	}
	var written int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='agent' AND author_id=? AND created_at>=?`, roomID, callerID, cutoff).Scan(&written); err != nil {
		return HumanDMResult{}, err
	}
	if written >= HumanDMMessagesPerHour {
		return HumanDMResult{}, ErrHumanDMCap
	}
	message, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "agent", AuthorID: callerID, Body: body})
	if err != nil {
		return HumanDMResult{}, err
	}
	return HumanDMResult{RoomID: roomID, MessageID: message.ID, No: message.No, Created: created}, nil
}
