package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
)

// Message pages use the room/sequence index and never scan the full history.
func (s *Store) ListMessagesLatest(ctx context.Context, roomID string, limit int) ([]Message, error) {
	items, err := s.listMessages(ctx, `WHERE room_id=? ORDER BY seq DESC LIMIT ?`, roomID, limit)
	slices.Reverse(items)
	return items, err
}

func (s *Store) ListMessagesBefore(ctx context.Context, roomID string, before int64, limit int) ([]Message, error) {
	items, err := s.listMessages(ctx, `WHERE room_id=? AND seq<? ORDER BY seq DESC LIMIT ?`, roomID, before, limit)
	slices.Reverse(items)
	return items, err
}

func (s *Store) ListMessagesAfterLimit(ctx context.Context, roomID string, after int64, limit int) ([]Message, error) {
	return s.listMessages(ctx, `WHERE room_id=? AND seq>? ORDER BY seq LIMIT ?`, roomID, after, limit)
}

func (s *Store) ListThreadRepliesAfter(ctx context.Context, roomID, rootID string, after int64, limit int) ([]Message, error) {
	return s.listMessages(ctx, `WHERE room_id=? AND thread_root=? AND seq>? ORDER BY seq LIMIT ?`, roomID, rootID, after, limit)
}

func (s *Store) ListThreadRepliesLatest(ctx context.Context, roomID, rootID string, limit int) ([]Message, error) {
	items, err := s.listMessages(ctx, `WHERE room_id=? AND thread_root=? ORDER BY seq DESC LIMIT ?`, roomID, rootID, limit)
	slices.Reverse(items)
	return items, err
}

func (s *Store) ListThreadRepliesBefore(ctx context.Context, roomID, rootID string, before int64, limit int) ([]Message, error) {
	items, err := s.listMessages(ctx, `WHERE room_id=? AND thread_root=? AND seq<? ORDER BY seq DESC LIMIT ?`, roomID, rootID, before, limit)
	slices.Reverse(items)
	return items, err
}

func (s *Store) ListThreadRepliesAround(ctx context.Context, roomID, rootID string, seq int64, limit int) ([]Message, error) {
	older, err := s.listMessages(ctx, `WHERE room_id=? AND thread_root=? AND seq<=? ORDER BY seq DESC LIMIT ?`, roomID, rootID, seq, (limit+1)/2)
	if err != nil {
		return nil, err
	}
	if len(older) == 0 || older[0].Seq != seq {
		return nil, sql.ErrNoRows
	}
	newer, err := s.ListThreadRepliesAfter(ctx, roomID, rootID, seq, limit-len(older))
	if err != nil {
		return nil, err
	}
	if len(older)+len(newer) < limit && len(older) == (limit+1)/2 {
		older, err = s.listMessages(ctx, `WHERE room_id=? AND thread_root=? AND seq<=? ORDER BY seq DESC LIMIT ?`, roomID, rootID, seq, limit-len(newer))
		if err != nil {
			return nil, err
		}
	}
	slices.Reverse(older)
	return append(older, newer...), nil
}

func (s *Store) MessageSeqInRoom(ctx context.Context, roomID, messageID string) (int64, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return 0, err
	}
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT seq FROM message WHERE room_id=? AND id=?`, roomID, messageID).Scan(&seq)
	return seq, err
}

func (s *Store) ListMessagesAround(ctx context.Context, roomID string, seq int64, limit int) ([]Message, error) {
	older, err := s.listMessages(ctx, `WHERE room_id=? AND seq<=? ORDER BY seq DESC LIMIT ?`, roomID, seq, (limit+1)/2)
	if err != nil {
		return nil, err
	}
	if len(older) == 0 || older[0].Seq != seq {
		return nil, sql.ErrNoRows
	}
	newer, err := s.ListMessagesAfterLimit(ctx, roomID, seq, limit-len(older))
	if err != nil {
		return nil, err
	}
	if len(older)+len(newer) < limit && len(older) == (limit+1)/2 {
		older, err = s.listMessages(ctx, `WHERE room_id=? AND seq<=? ORDER BY seq DESC LIMIT ?`, roomID, seq, limit-len(newer))
		if err != nil {
			return nil, err
		}
	}
	slices.Reverse(older)
	return append(older, newer...), nil
}

// Reactions for a page are read in one query, including sparse thread pages.
func (s *Store) ReactionsForMessages(ctx context.Context, messages []Message) (map[string][]map[string]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	out := make(map[string][]map[string]string)
	if len(messages) == 0 {
		return out, nil
	}
	args := make([]any, len(messages))
	for i, message := range messages {
		args[i] = message.ID
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(messages)), ",")
	rows, err := s.db.QueryContext(ctx, `SELECT r.message_id,r.member_kind,r.member_id,r.emoji,
		COALESCE(u.login,a.slug,r.member_id) FROM reaction r
		LEFT JOIN user u ON r.member_kind='user' AND u.id=r.member_id
		LEFT JOIN agent a ON r.member_kind='agent' AND a.id=r.member_id
		WHERE r.message_id IN (`+marks+`) ORDER BY r.message_id,r.emoji,r.member_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, member, emoji, name string
		if err := rows.Scan(&id, &kind, &member, &emoji, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], map[string]string{"member_kind": kind, "member_id": member, "member_name": name, "emoji": emoji})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// Count every reply to each root on a page, even when replies are outside it.
func (s *Store) ThreadCountsForMessages(ctx context.Context, roomID string, messages []Message) (map[string]int, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]int)
	args := []any{roomID}
	seen := make(map[string]bool)
	for _, message := range messages {
		rootID := message.ThreadRoot
		if rootID == "" {
			rootID = message.ID
		}
		if !seen[rootID] {
			seen[rootID] = true
			args = append(args, rootID)
		}
	}
	if len(args) == 1 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)-1), ",")
	rows, err := s.db.QueryContext(ctx, `SELECT thread_root,COUNT(*) FROM message
		WHERE room_id=? AND thread_root IN (`+marks+`) GROUP BY thread_root`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		out[id] = count
	}
	return out, rows.Err()
}

// ReplyPreview is what a client needs to quote the message a reply answers when that message is not in the page it has loaded: its number,
// who wrote it (as the message carries it, so the client names the author the way it names any author) and one line of its text.
type ReplyPreview struct {
	No         int64  `json:"no"`
	AuthorKind string `json:"author_kind"`
	AuthorID   string `json:"author_id"`
	AuthorName string `json:"author_name,omitempty"`
	Snippet    string `json:"snippet"`
}

// MessageSnippet is one line of a message: whitespace collapsed and cut to 96 characters, "[attachment]" for a message that is only files.
func MessageSnippet(body string, attachments int) string {
	text := strings.Join(strings.Fields(body), " ")
	if text == "" && attachments > 0 {
		return "[attachment]"
	}
	if runes := []rune(text); len(runes) > 96 {
		return strings.TrimRight(string(runes[:95]), " ") + "…"
	}
	return text
}

// ReplyPreviewsForMessages reads, in one query for the page, the message each reply on it answers, keyed by that message's ID. Only a
// message of the same room is ever returned, so a reply link that points elsewhere (an import can leave one) previews nothing. Only the
// start of each body is read (after leading whitespace), which is all a one-line snippet needs.
func (s *Store) ReplyPreviewsForMessages(ctx context.Context, roomID string, messages []Message) (map[string]ReplyPreview, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]ReplyPreview)
	seen := map[string]bool{}
	args := []any{roomID}
	for _, message := range messages {
		if message.ReplyTo != "" && !seen[message.ReplyTo] {
			seen[message.ReplyTo] = true
			args = append(args, message.ReplyTo)
		}
	}
	if len(args) == 1 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)-1), ",")
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.no,m.author_kind,m.author_id,COALESCE(u.login,''),substr(ltrim(m.body, ' '||char(9)||char(10)||char(13)),1,600)
		FROM message m LEFT JOIN user u ON m.author_kind='user' AND u.id=m.author_id
		WHERE m.room_id=? AND m.id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var textless []any // parents with no text of their own: the attachments are looked at only for these
	for rows.Next() {
		var id, body string
		var preview ReplyPreview
		if err := rows.Scan(&id, &preview.No, &preview.AuthorKind, &preview.AuthorID, &preview.AuthorName, &body); err != nil {
			return nil, err
		}
		if preview.Snippet = MessageSnippet(body, 0); preview.Snippet == "" {
			textless = append(textless, id)
		}
		out[id] = preview
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(textless) == 0 {
		return out, nil
	}
	files, err := s.db.QueryContext(ctx, `SELECT DISTINCT message_id FROM attachment WHERE message_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(textless)), ",")+`)`, textless...)
	if err != nil {
		return nil, err
	}
	defer files.Close()
	for files.Next() {
		var id string
		if err := files.Scan(&id); err != nil {
			return nil, err
		}
		preview := out[id]
		preview.Snippet = MessageSnippet("", 1)
		out[id] = preview
	}
	return out, files.Err()
}
