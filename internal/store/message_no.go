package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

// MessageByNo finds a message by its number (the "#N" people use). Callers check access to its room.
func (s *Store) MessageByNo(ctx context.Context, no int64) (Message, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Message{}, err
	}
	var m Message
	err := s.db.QueryRowContext(ctx, `SELECT id,seq,no,room_id FROM message WHERE no=? AND no>0`, no).Scan(&m.ID, &m.Seq, &m.No, &m.RoomID)
	return m, err
}

// messageNoRef reads "#12" or "12" as a message number; anything else (an id such as "msg_...") is not one.
func messageNoRef(ref string) (int64, bool) {
	digits := strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if digits == "" || digits[0] == '0' {
		return 0, false
	}
	no, err := strconv.ParseInt(digits, 10, 64)
	return no, err == nil && no > 0
}

// ResolveMessageRef turns a message reference into a message id: an agent may name a message by its number ("#12" or "12") as well as by
// its id. An id, or a number nothing carries, is returned unchanged, so the caller's own checks answer for it.
func (s *Store) ResolveMessageRef(ctx context.Context, ref string) (string, error) {
	no, ok := messageNoRef(ref)
	if !ok {
		return ref, nil
	}
	m, err := s.MessageByNo(ctx, no)
	if err == sql.ErrNoRows {
		return ref, nil
	}
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

// resolveMessageRefs rewrites the reply and thread references of an agent's answer that name a message by number into ids, in place,
// before the answer is validated and committed. The receipt digest is taken from the answer as the agent sent it, so a replay still matches.
// Only a reference the message's own room carries is rewritten: a number another room carries stays as written, so the validation reports it
// with the number the agent used.
func (tx *Tx) resolveMessageRefs(messages []answer.Message) error {
	for i := range messages {
		for _, ref := range []*string{&messages[i].ReplyTo} {
			no, ok := messageNoRef(*ref)
			if !ok {
				continue
			}
			var id string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM message WHERE no=? AND no>0 AND room_id=?`, no, messages[i].Room).Scan(&id)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return err
			}
			*ref = id
		}
	}
	return nil
}

// messageRefRoom finds the room of the message a reference names, by id or by number (as "#N" or "N"); found is false when no message
// carries either.
func (tx *Tx) messageRefRoom(ref string) (room string, found bool, err error) {
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM message WHERE id=?`, ref).Scan(&room)
	if err == nil {
		return room, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	no, ok := messageNoRef(ref)
	if !ok {
		return "", false, nil
	}
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM message WHERE no=? AND no>0`, no).Scan(&room)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return room, true, nil
}
