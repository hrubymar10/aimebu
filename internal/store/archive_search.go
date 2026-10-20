package store

import (
	"context"
	"strings"
	"unicode/utf8"
)

// ArchiveHit is one text match inside a closed room of the caller's project.
type ArchiveHit struct {
	RoomID    string `json:"room_id"`
	RoomName  string `json:"room_name"`
	MessageID string `json:"message_id"`
	Snippet   string `json:"snippet"`
	CreatedAt string `json:"created_at"`
}

// archiveHitLimit caps how many matches one archive search may return.
const archiveHitLimit = 50

// ArchiveSearch finds message bodies in this project's closed rooms for one
// member, keeping other members' private chats out like the room lists do.
func (s *Store) ArchiveSearch(ctx context.Context, projectID, userID, query string, limit int) ([]ArchiveHit, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > archiveHitLimit {
		limit = archiveHitLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.room_id,m.body,m.created_at,r.title
		FROM message m JOIN room r ON r.id=m.room_id
		WHERE r.project_id=? AND r.lifecycle='closed'
		  AND (r.kind!='dm' OR EXISTS (
			SELECT 1 FROM room_member own WHERE own.room_id=r.id AND own.member_kind='user'
			AND own.member_id=? AND own.left_at IS NULL))
		  AND m.body LIKE ? ESCAPE '\'
		ORDER BY m.created_at DESC,m.seq DESC LIMIT ?`, projectID, userID, likePattern(query), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArchiveHit{}
	for rows.Next() {
		var hit ArchiveHit
		var body string
		if err := rows.Scan(&hit.MessageID, &hit.RoomID, &body, &hit.CreatedAt, &hit.RoomName); err != nil {
			return nil, err
		}
		hit.Snippet = snippetAround(body, query)
		out = append(out, hit)
	}
	return out, rows.Err()
}

// likePattern escapes LIKE wildcards so a query cannot widen its own match.
func likePattern(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 2)
	out.WriteByte('%')
	for _, r := range query {
		if r == '\\' || r == '%' || r == '_' {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	out.WriteByte('%')
	return out.String()
}

// snippetAround keeps a rune-safe window around the first case-insensitive hit.
func snippetAround(body, query string) string {
	const window = 60
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	at := runeIndex(strings.ToLower(trimmed), strings.ToLower(query))
	if at < 0 {
		at = 0
	}
	start := at - window
	if start < 0 {
		start = 0
	}
	end := at + utf8.RuneCountInString(query) + window
	if end > len(runes) {
		end = len(runes)
	}
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return snippet
}

// runeIndex maps a byte offset into a rune offset so windows stay UTF-8 safe.
func runeIndex(hay, needle string) int {
	at := strings.Index(hay, needle)
	if at < 0 {
		return -1
	}
	return utf8.RuneCountInString(hay[:at])
}
