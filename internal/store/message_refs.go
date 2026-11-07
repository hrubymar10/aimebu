package store

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// MaxMessageRefs bounds how many "#N" references one delivery resolves into its context.
const MaxMessageRefs = 5

// refExcerptRunes bounds the body of a referenced message put into a turn context.
const refExcerptRunes = 1500

var (
	codeFence  = regexp.MustCompile("(?s)```.*?```")
	codeInline = regexp.MustCompile("`[^`\n]*`")
	// A message number: "#" and digits, not part of a word, a URL fragment or another "#".
	messageRef = regexp.MustCompile(`(^|[^\w/&#.\-])#([1-9][0-9]{0,17})\b`)
)

// extractMessageRefs lists the distinct message numbers written as "#N" in the bodies, in order of appearance, at most max, leaving out
// code spans and the numbers in skip.
func extractMessageRefs(bodies []string, skip map[int64]bool, max int) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, body := range bodies {
		text := codeInline.ReplaceAllString(codeFence.ReplaceAllString(body, " "), " ")
		for _, match := range messageRef.FindAllStringSubmatch(text, -1) {
			no, err := strconv.ParseInt(match[2], 10, 64)
			if err != nil || seen[no] || skip[no] {
				continue
			}
			seen[no] = true
			out = append(out, no)
			if len(out) == max {
				return out
			}
		}
	}
	return out
}

type singleRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AgentMessage is a message an agent asked for by number. Only a message of a room in the agent's own project is ever returned (and a
// private chat only to a member of it); anything else is Available:false with nothing else filled in, so a number from another project or a
// number nothing carries cannot be told apart.
type AgentMessage struct {
	No        int64  `json:"no"`
	Available bool   `json:"available"`
	RoomID    string `json:"room_id,omitempty"`
	Room      string `json:"room,omitempty"`
	From      string `json:"from,omitempty"`
	Body      string `json:"body,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

func excerptRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

func agentMessageByNo(ctx context.Context, q singleRowQuerier, agentID, projectID string, no int64, limit int) (AgentMessage, error) {
	unavailable := AgentMessage{No: no}
	var roomID, title, kind, authorKind, authorID, body, createdAt string
	err := q.QueryRowContext(ctx, `SELECT r.id,r.title,r.kind,m.author_kind,m.author_id,m.body,m.created_at
		FROM message m JOIN room r ON r.id=m.room_id
		WHERE m.no=? AND m.no>0 AND r.project_id IN (?,`+agentDMProjectSQL+`) AND (r.kind<>'dm' OR EXISTS (SELECT 1 FROM room_member own
			WHERE own.room_id=r.id AND own.member_kind='agent' AND own.member_id=? AND own.left_at IS NULL))`, no, projectID, agentID).
		Scan(&roomID, &title, &kind, &authorKind, &authorID, &body, &createdAt)
	if err == sql.ErrNoRows {
		return unavailable, nil
	}
	if err != nil {
		return AgentMessage{}, err
	}
	from := authorID
	switch authorKind {
	case "system":
		from = "system"
	case "user":
		var login string
		if err := q.QueryRowContext(ctx, `SELECT login FROM user WHERE id=?`, authorID).Scan(&login); err == nil {
			from = login
		}
	case "agent":
		var slug string
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(removed_slug,''),slug) FROM agent WHERE id=?`, authorID).Scan(&slug); err == nil {
			from = slug
		}
	}
	return AgentMessage{No: no, Available: true, RoomID: roomID, Room: title, From: from, Body: excerptRunes(body, limit), CreatedAt: createdAt}, nil
}

// AgentMessageByNo is what the message_get tool answers: a message by its number, limited to the agent's project (see AgentMessage).
func (s *Store) AgentMessageByNo(ctx context.Context, agentID, projectID string, no int64) (AgentMessage, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentMessage{}, err
	}
	return agentMessageByNo(ctx, s.db, agentID, projectID, no, 20000)
}

// referencedMessages resolves the "#N" references in what a delivery shows the agent into its context, so the agent does not have to ask:
// at most MaxMessageRefs, an excerpt each, the same project rule as AgentMessageByNo. The messages the delivery already carries are left out.
func (tx *Tx) referencedMessages(agentID, projectID string, bodies []string, delivered map[int64]bool) ([]AgentMessage, error) {
	var out []AgentMessage
	for _, no := range extractMessageRefs(bodies, delivered, MaxMessageRefs) {
		message, err := agentMessageByNo(tx.ctx, tx.tx, agentID, projectID, no, refExcerptRunes)
		if err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	return out, nil
}
