package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/hrubymar10/aimebu/internal/protocol"
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
	// System citation grants come from server-created missed-mention pointers.
	// Their persisted targets_json records the sole agent allowed to follow one.
	err := q.QueryRowContext(ctx, `SELECT r.id,r.title,r.kind,m.author_kind,m.author_id,m.body,m.created_at
		FROM message m JOIN room r ON r.id=m.room_id
		WHERE m.no=? AND m.no>0 AND r.project_id IN (?,`+agentDMProjectSQL+`) AND (
			(r.kind<>'dm' AND r.restricted=0)
			OR EXISTS (SELECT 1 FROM room_member own WHERE own.room_id=r.id AND own.member_kind='agent' AND own.member_id=? AND own.left_at IS NULL)
			OR EXISTS (SELECT 1 FROM message_reference_grant g JOIN message source ON source.id=g.source_message_id
				JOIN room_member own ON own.room_id=source.room_id AND own.member_kind='agent' AND own.member_id=? AND own.left_at IS NULL
				WHERE g.target_no=m.no AND r.kind<>'dm' AND source.room_id<>r.id AND source.room_id IN (SELECT id FROM room WHERE project_id=r.project_id)
				AND (source.author_kind<>'system' OR EXISTS (SELECT 1 FROM json_each(source.targets_json) WHERE value=?))))`, no, projectID, agentID, agentID, agentID).
		Scan(&roomID, &title, &kind, &authorKind, &authorID, &body, &createdAt)
	if err == sql.ErrNoRows {
		return unavailable, nil
	}
	if err != nil {
		return AgentMessage{}, err
	}
	return AgentMessage{No: no, Available: true, RoomID: roomID, Room: title, From: authorLabel(ctx, q, authorKind, authorID), Body: excerptRunes(body, limit), CreatedAt: createdAt}, nil
}

// authorLabel is how an agent is shown who wrote a message: a person's login, an agent's name (its old one when it was removed), "system".
func authorLabel(ctx context.Context, q singleRowQuerier, authorKind, authorID string) string {
	switch authorKind {
	case "system":
		return "system"
	case "user":
		var login string
		if err := q.QueryRowContext(ctx, `SELECT login FROM user WHERE id=?`, authorID).Scan(&login); err == nil {
			return login
		}
	case "agent":
		var slug string
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(removed_slug,''),slug) FROM agent WHERE id=?`, authorID).Scan(&slug); err == nil {
			return slug
		}
	}
	return authorID
}

// AgentMessageByNo is what the message_get tool answers: a message by its number, limited to the agent's project (see AgentMessage).
func (s *Store) AgentMessageByNo(ctx context.Context, agentID, projectID string, no int64) (AgentMessage, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentMessage{}, err
	}
	return agentMessageByNo(ctx, s.db, agentID, projectID, no, 20000)
}

// ReferencedMessageForUser returns only a cited target of a source message the
// user can read. An unavailable result deliberately does not reveal the target.
func (s *Store) ReferencedMessageForUser(ctx context.Context, userID, sourceID string, no int64) (AgentMessage, error) {
	out := AgentMessage{No: no}
	var sourceRoom, targetRoom string
	// A system pointer is visible in the shared room, but its cited message is
	// available to people only through project owner/operator oversight.
	err := s.db.QueryRowContext(ctx, `SELECT source.room_id,target.room_id FROM message_reference_grant g
		JOIN message source ON source.id=g.source_message_id
		JOIN message target ON target.no=g.target_no
		JOIN room sr ON sr.id=source.room_id JOIN room tr ON tr.id=target.room_id
		WHERE g.source_message_id=? AND g.target_no=? AND sr.project_id=tr.project_id AND source.room_id<>target.room_id AND tr.kind<>'dm'
		AND (source.author_kind<>'system' OR EXISTS (SELECT 1 FROM membership oversight WHERE oversight.project_id=sr.project_id
			AND oversight.user_id=? AND oversight.role IN ('owner','operator')))`, sourceID, no, userID).Scan(&sourceRoom, &targetRoom)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := s.UserRoomAccess(ctx, userID, sourceRoom); err != nil {
		if errors.Is(err, ErrForbidden) {
			return out, nil
		}
		return out, err
	}
	var title, authorKind, authorID, body, created string
	err = s.db.QueryRowContext(ctx, `SELECT r.title,m.author_kind,m.author_id,m.body,m.created_at FROM message m JOIN room r ON r.id=m.room_id WHERE m.no=? AND m.room_id=?`, no, targetRoom).Scan(&title, &authorKind, &authorID, &body, &created)
	if err != nil {
		return out, err
	}
	return AgentMessage{No: no, Available: true, RoomID: targetRoom, Room: title, From: authorLabel(ctx, s.db, authorKind, authorID), Body: body, CreatedAt: created}, nil
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

// parentExcerptRunes bounds the body of a note's reply or thread parent in a turn.
const parentExcerptRunes = 500

// noteWhy says why a message reached an agent, from its note's wake reason; parent is the message it replies to, if any.
func noteWhy(reason string, parent *protocol.NoteParent) string {
	switch reason {
	case "reply":
		if parent != nil && parent.No > 0 {
			return fmt.Sprintf("a reply to your message #%d", parent.No)
		}
		return "a reply to your message"
	case "mention":
		return "you are mentioned"
	case "role":
		return "your role is mentioned"
	case "group":
		return "a group tag that includes you"
	case "broadcast":
		return "an agent's message to the whole room, not addressed to you: answer only if you have to act, otherwise end the turn without writing a message"
	case "dm":
		return "a private message to you"
	case "agent_dm":
		return "a message in your chat with another agent"
	case "handoff":
		return "work handed to you"
	case "inbox_answer":
		return "an answer to your question"
	case "human_message":
		return "a person's message to the room"
	case "conversation":
		return "a person's reply in a conversation you started"
	}
	return reason
}

// noteContext fills in each delivered note's reason, the message it replies to and the first message of its conversation, so a reply is never an
// opaque id: reasons[i] is notes[i]'s wake reason.
func (tx *Tx) noteContext(agentID string, notes []protocol.DeliveryNote, reasons []string) error {
	parents := map[string]*protocol.NoteParent{}
	parent := func(messageID, roomID string) (*protocol.NoteParent, error) {
		if messageID == "" {
			return nil, nil
		}
		if found, ok := parents[messageID]; ok {
			return found, nil
		}
		var no int64
		var authorKind, authorID, body string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT no,author_kind,author_id,body FROM message WHERE id=? AND room_id=?`, messageID, roomID).Scan(&no, &authorKind, &authorID, &body)
		if err == sql.ErrNoRows {
			parents[messageID] = nil
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		found := &protocol.NoteParent{No: no, From: authorLabel(tx.ctx, tx.tx, authorKind, authorID), Body: excerptRunes(body, parentExcerptRunes), Yours: authorKind == "agent" && authorID == agentID}
		parents[messageID] = found
		return found, nil
	}
	for i := range notes {
		note := &notes[i]
		replyParent, err := parent(note.ReplyTo, note.RoomID)
		if err != nil {
			return err
		}
		note.InReplyTo = replyParent
		if note.ThreadRoot != "" && note.ThreadRoot != note.ReplyTo {
			if note.Conversation, err = parent(note.ThreadRoot, note.RoomID); err != nil {
				return err
			}
		}
		if i < len(reasons) {
			note.Why = noteWhy(reasons[i], replyParent)
			note.FYI = reasons[i] == "broadcast"
		}
	}
	return nil
}
