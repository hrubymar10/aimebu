package store

import (
	"context"
	"regexp"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// Rooms are referenced by id. A `#room_<id>` token in a message body stays as
// the id (agents write it that way); a person's typed `#<title>` is resolved
// to the id on save, by an exact, case-insensitive title match among the rooms
// the author can see — an ambiguous match or a room the author cannot see is
// left as it was written, and so is anything inside code, a pure `#<number>`
// message reference, or an id typed directly. Readers who may not see a room
// are never told its title: the entities table carries the id without a name
// (the web renders `#private-room`), and an id whose room is gone is left out
// entirely (the web renders `#unknown-room`).

var (
	// A stored room reference: "#" and the room's full id (`room_` and its key).
	roomRefPattern = regexp.MustCompile(`(?i)#(room_[a-z0-9]+)\b`)
	// A room title a person may have typed: "#" and a run of title characters
	// (letters, digits and the punctuation titles carry inside them), ending at
	// whitespace or other punctuation. Titles with spaces or a trailing mark are
	// not typeable as one token; the composer's picker inserts their id directly.
	roomTitleTokenPattern = regexp.MustCompile(`(?i)#([a-z0-9][a-z0-9_.+\-/]*)`)
	// A token that already is a stored room id (`#` plus the full id).
	roomIDTokenPattern = regexp.MustCompile(`(?i)^room_[a-z0-9]+$`)
)

// proseRoomRefSpans lists every `#room_<id>` occurrence in the live prose of
// body, each with its span in the raw body (the `#` included) and the id as
// written.
func proseRoomRefSpans(body string) []MentionSpan {
	return proseMarkedSpans(body, roomRefPattern)
}

// proseRoomTitleSpans lists every `#<token>` a person may have typed as a room
// title, in the live prose of body. The token is left as it was written; the
// caller decides what it names.
func proseRoomTitleSpans(body string) []MentionSpan {
	return proseMarkedSpans(body, roomTitleTokenPattern)
}

// storeRoomRefsByID resolves the room titles a person typed to room ids before
// the message is stored. It returns whether the body changed. Agents write the
// id form themselves, so their bodies are never rewritten.
func (tx *Tx) storeRoomRefsByID(message *Message) (bool, error) {
	if message.AuthorKind != "user" || message.Body == "" || !strings.Contains(message.Body, "#") {
		return false, nil
	}
	spans := proseRoomTitleSpans(message.Body)
	if len(spans) == 0 {
		return false, nil
	}
	replacements := map[string]string{}
	for _, span := range spans {
		token := span.Token
		if replacements[token] != "" {
			continue
		}
		lower := strings.ToLower(token)
		// A stored room id and a bare message number are never titles.
		if roomIDTokenPattern.MatchString(lower) || isAllDigits(lower) {
			continue
		}
		var ids []string
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT r.id FROM room r WHERE lower(r.title)=? AND
			((r.kind<>'dm' AND EXISTS (SELECT 1 FROM membership pm WHERE pm.project_id=r.project_id AND pm.user_id=?)
				AND (r.restricted=0 OR EXISTS (SELECT 1 FROM membership oversight WHERE oversight.project_id=r.project_id AND oversight.user_id=? AND oversight.role IN ('owner','operator'))
					OR EXISTS (SELECT 1 FROM room_member um WHERE um.room_id=r.id AND um.member_kind='user' AND um.member_id=? AND um.left_at IS NULL)))
			OR (r.kind='dm' AND r.project_id='`+AgentDMProjectID+`' AND EXISTS (SELECT 1 FROM room_member dm WHERE dm.room_id=r.id AND dm.member_kind='user' AND dm.member_id=? AND dm.left_at IS NULL)))`,
			lower, message.AuthorID, message.AuthorID, message.AuthorID, message.AuthorID)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		// Exactly one visible room holds the title; an ambiguous match stays as written.
		if len(ids) == 1 {
			replacements[token] = "#" + ids[0]
		}
	}
	if len(replacements) == 0 {
		return false, nil
	}
	texts := make([]string, len(spans))
	for i, span := range spans {
		texts[i] = replacements[span.Token]
	}
	message.Body, _ = replaceMentionSpans(message.Body, spans, texts)
	return true, nil
}

// isAllDigits says whether token is a bare message number (`#N`).
func isAllDigits(token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return false
		}
	}
	return true
}

// roomRef is one stored `#room_<id>` reference looked up for a reader.
type roomRef struct {
	id         string
	title      string
	kind       string
	projectID  string
	restricted int
	gone       bool
}

// loadRoomRefs looks the given room-id tokens (lower-cased) up in the room
// table. A token no room matches comes back with gone set.
func loadRoomRefs(ctx context.Context, q rowQuerier, tokens []string) (map[string]roomRef, error) {
	out := map[string]roomRef{}
	if len(tokens) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, roomRefsByIDsSQL(len(tokens)), toAnySlice(tokens)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref roomRef
		var lower string
		if err := rows.Scan(&lower, &ref.id, &ref.title, &ref.kind, &ref.projectID, &ref.restricted); err != nil {
			return nil, err
		}
		out[lower] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, token := range tokens {
		if _, ok := out[token]; !ok {
			out[token] = roomRef{gone: true}
		}
	}
	return out, nil
}

// roomRefsByIDsSQL selects the room rows for lower(id) IN (?,?,...).
func roomRefsByIDsSQL(count int) string {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", count), ",")
	return `SELECT lower(id),id,COALESCE(title,''),kind,COALESCE(project_id,''),restricted FROM room WHERE lower(id) IN (` + placeholders + `)`
}

// roomMembersOf loads the rooms (by lower id) of which memberID is a live
// member of the given kind, among the given tokens.
func roomMembersOf(ctx context.Context, q rowQuerier, memberKind, memberID string, tokens []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(tokens) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tokens)), ",")
	args := append([]any{memberKind, memberID}, toAnySlice(tokens)...)
	rows, err := q.QueryContext(ctx, `SELECT lower(room_id) FROM room_member WHERE member_kind=? AND member_id=? AND left_at IS NULL AND lower(room_id) IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var roomID string
		if err := rows.Scan(&roomID); err != nil {
			return nil, err
		}
		out[roomID] = true
	}
	return out, rows.Err()
}

func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// agentSeesRoom says whether an agent of readerProjectID may read the room: a
// non-chat room of its project (a restricted one only as a member), or a
// private chat it sits in.
func agentSeesRoom(ref roomRef, member bool, readerProjectID string) bool {
	if ref.kind == "dm" {
		return ref.projectID == AgentDMProjectID && member
	}
	return ref.projectID == readerProjectID && (ref.restricted == 0 || member)
}

// roomEntitiesForAgent builds the room entries of the entities table one
// stored body carries for the agent reader: one entry per distinct
// `#room_<id>` the live prose mentions, in body order, named only when the
// reader may see the room. An unknown or deleted id is left out entirely.
func roomEntitiesForAgent(ctx context.Context, q rowQuerier, readerID, readerProjectID, body string) ([]MentionEntity, error) {
	spans := proseRoomRefSpans(body)
	if len(spans) == 0 {
		return nil, nil
	}
	var tokens []string
	seen := map[string]bool{}
	for _, span := range spans {
		token := strings.ToLower(span.Token)
		if seen[token] {
			continue
		}
		seen[token] = true
		tokens = append(tokens, token)
	}
	rooms, err := loadRoomRefs(ctx, q, tokens)
	if err != nil {
		return nil, err
	}
	members, err := roomMembersOf(ctx, q, "agent", readerID, tokens)
	if err != nil {
		return nil, err
	}
	out := []MentionEntity{}
	seen = map[string]bool{}
	for _, span := range spans {
		token := strings.ToLower(span.Token)
		if seen[token] {
			continue
		}
		seen[token] = true
		ref := rooms[token]
		if ref.gone {
			continue
		}
		name := ""
		if agentSeesRoom(ref, members[token], readerProjectID) {
			name = ref.title
		}
		out = append(out, MentionEntity{ID: ref.id, Name: name, Kind: "room"})
	}
	return out, nil
}

// userSeesRoom says whether the person may read the room: a non-chat room of a
// project they belong to (a restricted one with oversight or a seat in it), or
// a private chat they are a member of.
func userSeesRoom(ref roomRef, member bool, memberships map[string]string) bool {
	if ref.kind == "dm" {
		return ref.projectID == AgentDMProjectID && member
	}
	role := memberships[ref.projectID]
	if role == "" {
		return false
	}
	if ref.restricted == 0 {
		return true
	}
	return role == "owner" || role == "operator" || member
}

// RoomRefsForUser fills each message's Entities with the room references its
// body mentions, named only when the user may see the room: one entry per
// distinct `#room_<id>` in body order, the title only for a room the user can
// read. An unknown or deleted id is left out entirely. It reads only; the
// stored bodies keep their ids.
func (s *Store) RoomRefsForUser(ctx context.Context, userID string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	perMessage := make([][]MentionSpan, len(messages))
	var tokens []string
	seen := map[string]bool{}
	for i, message := range messages {
		spans := proseRoomRefSpans(message.Body)
		perMessage[i] = spans
		for _, span := range spans {
			token := strings.ToLower(span.Token)
			if !seen[token] {
				seen[token] = true
				tokens = append(tokens, token)
			}
		}
	}
	if len(tokens) == 0 {
		return nil
	}
	rooms, err := loadRoomRefs(ctx, s.readDB, tokens)
	if err != nil {
		return err
	}
	members, err := roomMembersOf(ctx, s.readDB, "user", userID, tokens)
	if err != nil {
		return err
	}
	memberships := map[string]string{}
	memberRows, err := s.readDB.QueryContext(ctx, `SELECT project_id,role FROM membership WHERE user_id=?`, userID)
	if err != nil {
		return err
	}
	for memberRows.Next() {
		var projectID, role string
		if err := memberRows.Scan(&projectID, &role); err != nil {
			memberRows.Close()
			return err
		}
		memberships[projectID] = role
	}
	if err := memberRows.Err(); err != nil {
		memberRows.Close()
		return err
	}
	memberRows.Close()
	for i := range messages {
		spans := perMessage[i]
		if len(spans) == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, span := range spans {
			token := strings.ToLower(span.Token)
			if seen[token] {
				continue
			}
			seen[token] = true
			ref := rooms[token]
			if ref.gone {
				continue
			}
			name := ""
			if userSeesRoom(ref, members[token], memberships) {
				name = ref.title
			}
			messages[i].Entities = append(messages[i].Entities, protocol.Entity{ID: ref.id, Name: name, Kind: "room"})
		}
	}
	return nil
}
