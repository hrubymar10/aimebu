package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// AgentDMNewChatsPerHour is how many new chats one lead may open in an hour: the per-chat limit does not stop a fan-out to many agents.
	AgentDMNewChatsPerHour = 10
	// MaxAgentDMBody bounds one dm_agent message, in bytes.
	MaxAgentDMBody = 20000
)

var (
	// ErrAgentUnavailable is a chat with an agent that is revoked, stopped or not accepting work.
	ErrAgentUnavailable = errors.New("agent_unavailable")
	// ErrDMTurnCap is a chat that already used its hourly agent turns.
	ErrDMTurnCap = errors.New("dm_turn_cap")
	// ErrDMFanOutCap is a lead that already opened its hourly share of new chats.
	ErrDMFanOutCap = errors.New("dm_fanout_cap")
)

// AgentDMResult is what dm_agent returns: the chat and the message that went into it.
type AgentDMResult struct {
	RoomID    string `json:"room_id"`
	MessageID string `json:"message_id"`
	No        int64  `json:"no"`
	Created   bool   `json:"created"`
}

// DMAgentAsTool is a lead's dm_agent call: it opens (or finds) the chat with the target agent, whatever project the target is in, and posts
// the message into it, which wakes the target. Everything is decided here, never by the model: only an active lead may start a chat; the
// people who caused the lead's turn must reach the target's project (see leadMayReach); the target must be live and its project not
// archived; and a lead opens only so many new chats an hour and a chat takes only so many turns.
func (tx *Tx) DMAgentAsTool(callerID, targetID, body string) (AgentDMResult, error) {
	var callerProject, callerDesired string
	var lead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.desired,r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=? AND a.revoked_at IS NULL`, callerID).
		Scan(&callerProject, &callerDesired, &lead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentDMResult{}, ErrForbidden
		}
		return AgentDMResult{}, err
	}
	if !lead || callerDesired != "active" {
		return AgentDMResult{}, ErrForbidden
	}
	if targetID == callerID {
		return AgentDMResult{}, ErrInvalidTransition
	}
	var targetProject, targetDesired string
	var targetRevoked, targetArchived bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.desired,a.revoked_at IS NOT NULL,p.archived_at IS NOT NULL FROM agent a JOIN project p ON p.id=a.project_id WHERE a.id=?`, targetID).
		Scan(&targetProject, &targetDesired, &targetRevoked, &targetArchived); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// An agent that does not exist answers like one the lead may not reach, so an ID cannot be probed.
			return AgentDMResult{}, ErrForbidden
		}
		return AgentDMResult{}, err
	}
	allowed, err := tx.leadMayReach(callerID, callerProject, targetProject)
	if err != nil {
		return AgentDMResult{}, err
	}
	if !allowed {
		return AgentDMResult{}, ErrForbidden
	}
	if err := tx.refuseArchived(callerProject); err != nil {
		return AgentDMResult{}, err
	}
	if targetArchived {
		return AgentDMResult{}, ErrProjectArchived
	}
	if targetRevoked || targetDesired != "active" {
		return AgentDMResult{}, ErrAgentUnavailable
	}
	var existing string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE dm_key=?`, agentDMKey(callerID, targetID)).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var opened int
		cutoff := time.Now().UTC().Add(-time.Hour).Format(timestampLayout)
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room WHERE project_id=`+agentDMProjectSQL+` AND created_by=? AND created_at>=?`, callerID, cutoff).Scan(&opened); err != nil {
			return AgentDMResult{}, err
		}
		if opened >= AgentDMNewChatsPerHour {
			return AgentDMResult{}, ErrDMFanOutCap
		}
	case err != nil:
		return AgentDMResult{}, err
	}
	room, created, err := tx.OpenAgentDM(callerID, targetID, callerID)
	if err != nil {
		return AgentDMResult{}, err
	}
	if room.Lifecycle != "open" {
		return AgentDMResult{}, ErrInvalidTransition
	}
	turns, err := tx.agentChatTurns(room.ID)
	if err != nil {
		return AgentDMResult{}, err
	}
	if turns >= AgentDMTurnsPerHour {
		return AgentDMResult{}, ErrDMTurnCap
	}
	message, err := tx.InsertMessage(Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: callerID, Body: body})
	if err != nil {
		return AgentDMResult{}, err
	}
	return AgentDMResult{RoomID: room.ID, MessageID: message.ID, No: message.No, Created: created}, nil
}

// leadMayReach is the authorization of a chat across projects. The people whose messages caused the lead's current turn (and the turns it
// continues, for a correction or a retry) must every one reach the target's project; if no person caused it (an agent or a schedule woke
// the lead), an owner of the lead's own project must. Reach is owner or operator of the target project, and the instance owner reaches
// everything. A viewer does not: a chat makes the target work and spend.
func (tx *Tx) leadMayReach(leadID, leadProject, targetProject string) (bool, error) {
	var turn string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM turn WHERE agent_id=? AND exec IN ('pending','running') ORDER BY id DESC LIMIT 1`, leadID).Scan(&turn); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `WITH RECURSIVE chain(id) AS (
			SELECT ? UNION SELECT t.parent_turn_id FROM turn t JOIN chain c ON t.id=c.id WHERE t.parent_turn_id IS NOT NULL)
		SELECT DISTINCT m.author_id FROM chain JOIN turn_note tn ON tn.turn_id=chain.id JOIN note n ON n.id=tn.note_id
		JOIN message m ON m.id=n.message_id WHERE m.author_kind='user' ORDER BY 1`, turn)
	if err != nil {
		return false, err
	}
	var people []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		people = append(people, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(people) == 0 {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT user_id FROM membership WHERE project_id=? AND role='owner' ORDER BY user_id`, leadProject)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
			people = append(people, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		for _, id := range people {
			if ok, err := userReaches(tx.ctx, tx.tx, id, targetProject); err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	}
	for _, id := range people {
		if ok, err := userReaches(tx.ctx, tx.tx, id, targetProject); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// ValidAgentDMBody checks what a lead sends: some text, within the limit.
func ValidAgentDMBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("body is required")
	}
	if len(body) > MaxAgentDMBody {
		return fmt.Errorf("body is at most %d bytes", MaxAgentDMBody)
	}
	return nil
}
