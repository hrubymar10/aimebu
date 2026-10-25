package store

import (
	"context"
	"database/sql"
	"errors"
)

// AgentDMOffer is a chat between agents a person could join but has not: its title and activity only, never its text.
type AgentDMOffer struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	LastActivityAt string `json:"last_activity_at"`
	MessageCount   int    `json:"message_count"`
	Lifecycle      string `json:"lifecycle"`
}

// AgentDMOffers lists the open chats between agents the person has a footing in (they belong to a project of one of its agents, or are the
// instance owner) and has not joined. Joining shows them the whole conversation. A chat that was closed (its project went away) is not
// offered and cannot be joined; those who were in it keep reading it.
func (s *Store) AgentDMOffers(ctx context.Context, userID string) ([]AgentDMOffer, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.title,COALESCE((SELECT m.created_at FROM message m WHERE m.room_id=r.id ORDER BY m.seq DESC LIMIT 1),r.created_at),
		(SELECT COUNT(*) FROM message m WHERE m.room_id=r.id),r.lifecycle
		FROM room r WHERE r.project_id=`+agentDMProjectSQL+` AND r.lifecycle='open' AND `+agentDMEligibleSQL+`
		AND NOT EXISTS (SELECT 1 FROM room_member own WHERE own.room_id=r.id AND own.member_kind='user' AND own.member_id=? AND own.left_at IS NULL)
		ORDER BY 3 DESC,r.id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentDMOffer{}
	for rows.Next() {
		var offer AgentDMOffer
		if err := rows.Scan(&offer.ID, &offer.Title, &offer.LastActivityAt, &offer.MessageCount, &offer.Lifecycle); err != nil {
			return nil, err
		}
		out = append(out, offer)
	}
	return out, rows.Err()
}

// JoinAgentDM adds a person to a chat between agents they have a footing in and says so in the chat, so the agents and everyone else in it
// know someone is reading. Joining twice changes nothing.
func (s *Store) JoinAgentDM(ctx context.Context, userID, roomID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var eligible bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS (SELECT 1 FROM room r WHERE r.id=? AND r.project_id=`+agentDMProjectSQL+` AND r.lifecycle='open' AND `+agentDMEligibleSQL+`)`, roomID, userID).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return ErrForbidden
		}
		var member bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS (SELECT 1 FROM room_member WHERE room_id=? AND member_kind='user' AND member_id=? AND left_at IS NULL)`, roomID, userID).Scan(&member); err != nil || member {
			return err
		}
		if err := tx.AddMember(roomID, "user", userID); err != nil {
			return err
		}
		return tx.agentChatPresence(roomID, userID, "joined")
	})
}

// LeaveAgentDM takes a person out of a chat between agents.
func (s *Store) LeaveAgentDM(ctx context.Context, userID, roomID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var member bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS (SELECT 1 FROM room r JOIN room_member own ON own.room_id=r.id AND own.member_kind='user' AND own.member_id=? AND own.left_at IS NULL
			WHERE r.id=? AND r.project_id=`+agentDMProjectSQL+`)`, userID, roomID).Scan(&member); err != nil {
			return err
		}
		if !member {
			return ErrForbidden
		}
		if err := tx.agentChatPresence(roomID, userID, "left"); err != nil {
			return err
		}
		return tx.RemoveMember(roomID, "user", userID)
	})
}

func (tx *Tx) agentChatPresence(roomID, userID, what string) error {
	var login string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT login FROM user WHERE id=?`, userID).Scan(&login); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if login == "" {
		login = userID
	}
	_, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: login + " " + what + " the chat."})
	return err
}
