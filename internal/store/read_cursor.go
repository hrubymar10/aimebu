package store

import (
	"context"
)

// ReadState is one user's read position in one room.
type ReadState struct {
	ReadCursor  int64 `json:"read_cursor"`
	UnreadCount int   `json:"unread_count"`
	// UnreadAddressed is the part of the unread messages that name this person or reply to them.
	UnreadAddressed int `json:"unread_addressed"`
	AttentionCount  int `json:"attention_count"`
}

// MarkRoomRead advances the user's read cursor to seq (the room's newest
// message when seq is 0). The cursor never moves backwards, so a stale
// device cannot resurrect messages another device already read. Visible
// attention items through the read cursor are marked seen for this person.
// A cursor or seen-state change emits room_read_changed for other sessions.
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
		changed := false
		if seq > current {
			if _, err := tx.Exec(`INSERT INTO room_read_cursor(user_id,room_id,last_read_seq) VALUES (?,?,?)
				ON CONFLICT(user_id,room_id) DO UPDATE SET last_read_seq=excluded.last_read_seq`, userID, roomID, seq); err != nil {
				return err
			}
			state.ReadCursor = seq
			changed = true
		}
		// Opening the latest page acknowledges only requests already present in that
		// view. Items without a message are visible in the room's Inbox context.
		marked, err := tx.Exec(`INSERT OR IGNORE INTO inbox_item_seen(item_id,user_id,seen_at)
			SELECT i.id,?,? `+inboxFromSQL+` LEFT JOIN message msg ON msg.id=i.message_id `+inboxCountWhereSQL+`
			AND i.room_id=? AND (i.message_id IS NULL OR msg.seq<=?)`,
			userID, timestamp(), userID, userID, roomID, state.ReadCursor)
		if err != nil {
			return err
		}
		if n, err := marked.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			changed = true
		}
		if changed {
			if err := tx.appendReadEvents(projectID, roomID, userID); err != nil {
				return err
			}
		}
		return tx.readCounts(userID, roomID, &state)
	})
	return state, err
}

// MarkRoomUnread moves the user's own read cursor back to just before the
// message with seq (the room's newest message when seq is 0 or above the
// newest), so that message and everything after it count as unread again.
// It only ever moves the cursor backwards: a cursor already before seq stays
// where it is, so marking unread never marks anything read. Other people's
// cursors, agents' seen markers and the Inbox's seen state are left alone. A
// change emits room_read_changed for the user's other sessions.
func (s *Store) MarkRoomUnread(ctx context.Context, userID, roomID string, seq int64) (ReadState, error) {
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
		if seq > 0 && seq-1 < current {
			if _, err := tx.Exec(`UPDATE room_read_cursor SET last_read_seq=? WHERE user_id=? AND room_id=?`, seq-1, userID, roomID); err != nil {
				return err
			}
			state.ReadCursor = seq - 1
			if err := tx.appendReadEvents(projectID, roomID, userID); err != nil {
				return err
			}
		}
		return tx.readCounts(userID, roomID, &state)
	})
	return state, err
}

// appendReadEvents announces a move of userID's read cursor: room_read_changed
// for everybody in the room (the read receipts follow it) and read_cursor_moved,
// which only that person's own sessions are sent, so their other tabs and
// devices update their counts live.
func (tx *Tx) appendReadEvents(projectID, roomID, userID string) error {
	for _, kind := range []string{"room_read_changed", "read_cursor_moved"} {
		if _, err := tx.AppendEvent(Event{Kind: kind, ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"user_id": userID})}); err != nil {
			return err
		}
	}
	return nil
}

// readCounts fills the unread and attention counts of state from its ReadCursor.
func (tx *Tx) readCounts(userID, roomID string, state *ReadState) error {
	// A room set to mentions only counts the messages that address this person, as the room list does.
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM message WHERE room_id=? AND seq>?
		AND (COALESCE((SELECT notify FROM room_view_preference WHERE user_id=? AND room_id=?),'default')<>'mentions'
		OR EXISTS (SELECT 1 FROM json_each(message.targets_json) WHERE value=?))`,
		roomID, state.ReadCursor, userID, roomID, userID).Scan(&state.UnreadCount); err != nil {
		return err
	}
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM message WHERE room_id=? AND seq>?
		AND EXISTS (SELECT 1 FROM json_each(message.targets_json) WHERE value=?)`,
		roomID, state.ReadCursor, userID).Scan(&state.UnreadAddressed); err != nil {
		return err
	}
	return tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) `+inboxFromSQL+` `+inboxCountWhereSQL+`
		AND i.room_id=? AND NOT EXISTS (SELECT 1 FROM inbox_item_seen seen WHERE seen.item_id=i.id AND seen.user_id=?)`,
		userID, userID, roomID, userID).Scan(&state.AttentionCount)
}

// messageForPersonSQL narrows a message row aliased fm to the ones meant for one person, whose user id is its single placeholder: the message
// names them or replies to them (their id is in its targets) or asks for a person's attention. A worker's completion hands off to its
// coordinator, so it targets an agent and never matches. "Finished, not yet seen" is such a message from an agent that is not in a turn,
// after the person's read cursor: the state is derived from that cursor and stored nowhere.
const messageForPersonSQL = `(fm.needs_attention=1 OR EXISTS (SELECT 1 FROM json_each(fm.targets_json) WHERE value=?))`

// openAttentionByRoom counts every open inbox entry the user can see per room, seen or not: a room still waits for a person while an
// entry is open, whether or not they have looked at it.
func (s *Store) openAttentionByRoom(ctx context.Context, userID string) (map[string]int, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT i.room_id, COUNT(*) `+inboxFromSQL+` `+inboxCountWhereSQL+` GROUP BY i.room_id`, userID, userID)
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

// attentionByRoom counts the unseen open inbox entries the user can see per room,
// reusing the inbox visibility filter so the badge is the unseen subset.
func (s *Store) attentionByRoom(ctx context.Context, userID string) (map[string]int, error) {
	// A count per room, not the whole inbox: every project's room list asks for this, and building each item (diagnostic reports,
	// receipts, options) is wasted work when only the number is shown.
	rows, err := s.readDB.QueryContext(ctx, `SELECT i.room_id, COUNT(*) `+inboxFromSQL+` `+inboxCountWhereSQL+`
		AND NOT EXISTS (SELECT 1 FROM inbox_item_seen seen WHERE seen.item_id=i.id AND seen.user_id=?)
		GROUP BY i.room_id`, userID, userID, userID)
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
// their stored cursor; an agent reports the newest message it has dealt with
// (see RoomReadReceipts).
type ReadReceipt struct {
	MemberKind string `json:"member_kind"`
	MemberID   string `json:"member_id"`
	Label      string `json:"label"`
	Seq        int64  `json:"seq"`
}

// roomReadReceiptsSQL takes the room id four times. The last arm is a key lookup in agent_room_seen, which queueMessageWakes keeps.
const roomReadReceiptsSQL = `SELECT 'user',u.id,u.login,c.last_read_seq
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
			UNION ALL
			SELECT agent_id, seq FROM (
				SELECT s.agent_id AS agent_id, MIN(s.seq, COALESCE((SELECT MIN(q.seq)-1 FROM note n2 JOIN message q ON q.id=n2.message_id
					WHERE n2.room_id=s.room_id AND n2.agent_id=s.agent_id AND n2.state IN ('queued','dispatched')), s.seq)) AS seq
				FROM agent_room_seen s WHERE s.room_id=?
			) WHERE seq>0
		) x ON x.agent_id=a.id
		WHERE a.revoked_at IS NULL
		GROUP BY a.id,a.slug
		ORDER BY 1,3`

// RoomReadReceipts lists the read positions of everyone who can read the
// room. Members that have read nothing yet are omitted. A human's position is
// their stored read cursor. An agent's is the newest message it has dealt
// with, which is v1's rule (an agent's wait handed it every room message, the
// ones not for it marked "no need to respond", and its cursor moved as it took
// them) in v2's terms:
//   - a message a turn took (its note is in_turn or done);
//   - a message it wrote: its own reply has been seen by definition, so the
//     marker never stays behind the message it answered;
//   - a message the router delivered to it as nothing for it to do (not
//     addressed to it, system chatter, or an unaddressed message for the
//     worker while it is the lead put in the room): it counts the moment it
//     is posted, as an unaddressed message did when a waiting v1 agent
//     received it. queueMessageWakes keeps the newest one per room and agent
//     in agent_room_seen. That marker never passes a message still waiting for the agent: it is read as at most the message before the
//     agent's oldest queued or dispatched note in the room, so chatter after a queued note does not move it past that note (and
//     this holds for the rows the migration's backfill wrote too).
//
// A queued note does not count: a message meant for an agent that is busy or
// stopped stays unread until a turn takes it. Neither does a skip for a
// revoked agent, a person who cannot reach it, or a full agent chat: those
// were meant for it and did not get through.
func (s *Store) RoomReadReceipts(ctx context.Context, roomID string) ([]ReadReceipt, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, roomReadReceiptsSQL, roomID, roomID, roomID, roomID)
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
