package store

import (
	"context"
)

// ReadState is one user's read position in one room.
type ReadState struct {
	ReadCursor  int64 `json:"read_cursor"`
	UnreadCount int   `json:"unread_count"`
}

// MarkRoomRead advances the user's read cursor to seq (the room's newest
// message when seq is 0). The cursor never moves backwards, so a stale
// device cannot resurrect messages another device already read. A real
// advance emits room_read_changed so the user's other sessions refresh.
func (s *Store) MarkRoomRead(ctx context.Context, userID, roomID string, seq int64) (ReadState, error) {
	if err := s.UserRoomAccess(ctx, userID, roomID); err != nil {
		return ReadState{}, err
	}
	var state ReadState
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		var newest int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,COALESCE((SELECT MAX(seq) FROM message WHERE room_id=room.id),0) FROM room WHERE id=?`, roomID).Scan(&projectID, &newest); err != nil {
			return err
		}
		if seq <= 0 || seq > newest {
			seq = newest
		}
		var current int64
		_ = tx.tx.QueryRowContext(tx.ctx, `SELECT last_read_seq FROM room_read_cursor WHERE user_id=? AND room_id=?`, userID, roomID).Scan(&current)
		state.ReadCursor = current
		if seq > current {
			if _, err := tx.Exec(`INSERT INTO room_read_cursor(user_id,room_id,last_read_seq) VALUES (?,?,?)
				ON CONFLICT(user_id,room_id) DO UPDATE SET last_read_seq=excluded.last_read_seq`, userID, roomID, seq); err != nil {
				return err
			}
			state.ReadCursor = seq
			if _, err := tx.AppendEvent(Event{Kind: "room_read_changed", ProjectID: projectID, RoomID: roomID,
				PayloadJSON: payloadJSON(map[string]string{"user_id": userID})}); err != nil {
				return err
			}
		}
		// A room set to mentions only counts the messages that address this person, as the room list does.
		return tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM message WHERE room_id=? AND seq>?
			AND (COALESCE((SELECT notify FROM room_view_preference WHERE user_id=? AND room_id=?),'default')<>'mentions'
				OR EXISTS (SELECT 1 FROM json_each(message.targets_json) WHERE value=?))`,
			roomID, state.ReadCursor, userID, roomID, userID).Scan(&state.UnreadCount)
	})
	return state, err
}

// attentionByRoom counts the open inbox entries the user can see per room,
// reusing the inbox filter so the badge always matches the inbox panel.
func (s *Store) attentionByRoom(ctx context.Context, userID string) (map[string]int, error) {
	// A count per room, not the whole inbox: every project's room list asks for this, and building each item (diagnostic reports,
	// receipts, options) is wasted work when only the number is shown.
	rows, err := s.db.QueryContext(ctx, `SELECT i.room_id, COUNT(*) `+inboxFromSQL+` `+inboxWhereSQL+` GROUP BY i.room_id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var room string
		var n int
		if err := rows.Scan(&room, &n); err != nil {
			return nil, err
		}
		counts[room] = n
	}
	return counts, rows.Err()
}

// ReadReceipt is where one room member's read position lands. Humans report
// their stored cursor; an agent counts as having read the newest message it
// took into a turn, since agents only ever see the notes delivered to them.
type ReadReceipt struct {
	MemberKind string `json:"member_kind"`
	MemberID   string `json:"member_id"`
	Label      string `json:"label"`
	Seq        int64  `json:"seq"`
}

// RoomReadReceipts lists the read positions of everyone who can read the
// room. Members that have read nothing yet are omitted. A human's position is
// their stored read cursor. An agent's is the newest message it took into a
// turn or wrote itself: its own reply has been seen by definition, so the
// marker must not stay behind the message it answered.
func (s *Store) RoomReadReceipts(ctx context.Context, roomID string) ([]ReadReceipt, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT 'user',u.id,u.login,c.last_read_seq
		FROM room_read_cursor c JOIN user u ON u.id=c.user_id JOIN room r ON r.id=c.room_id
		WHERE c.room_id=? AND c.last_read_seq>0 AND u.disabled_at IS NULL AND (
			(r.kind!='dm' AND EXISTS (SELECT 1 FROM membership mb WHERE mb.project_id=r.project_id AND mb.user_id=u.id))
			OR (r.kind='dm' AND EXISTS (SELECT 1 FROM room_member rm WHERE rm.room_id=r.id AND rm.member_kind='user' AND rm.member_id=u.id AND rm.left_at IS NULL)))
		UNION ALL
		SELECT 'agent',a.id,a.slug,MAX(x.seq)
		FROM agent a JOIN (
			SELECT n.agent_id AS agent_id, m.seq AS seq
			FROM note n JOIN message m ON m.id=n.message_id
			WHERE n.room_id=? AND n.state IN ('in_turn','done')
			UNION ALL
			SELECT m.author_id, m.seq FROM message m WHERE m.room_id=? AND m.author_kind='agent'
		) x ON x.agent_id=a.id
		WHERE a.revoked_at IS NULL
		GROUP BY a.id,a.slug
		ORDER BY 1,3`, roomID, roomID, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReadReceipt{}
	for rows.Next() {
		var r ReadReceipt
		if err := rows.Scan(&r.MemberKind, &r.MemberID, &r.Label, &r.Seq); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
