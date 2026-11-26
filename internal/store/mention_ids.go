package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Mentions are stored by agent ID. When a message is written, every live
// prose mention that names exactly one agent of the room (`@name`,
// `@name@project`, `@name-2`) is rewritten to `@agt_<id>` in the stored body,
// so the mention keeps addressing that agent even after its name is freed and
// reused by another one. Every reader gets the display form back: the read
// paths rewrite `@agt_<id>` to `@<name>` (with a `-2`, `-3`, ... suffix on the
// later of two agents that share a name in one room) and carry a `mentions`
// array naming what each stored mention resolved to. A mention of an unknown
// or removed agent that cannot be named any more is left as the stored
// `@agt_<id>` text. Wake and addressing work off the stored IDs: the resolver
// has always accepted a full member ID as a token.

// MentionRef is one stored agent-ID mention of a message as a reader sees it:
// the agent it was stored with and the display name the reader is shown.
// Resolved is false when the agent could not be named any more (it was
// removed, or its row is gone); the body then still shows the stored ID text.
type MentionRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
}

// MentionSpan is one @token occurrence in the live prose of a body (code,
// fences, indented blocks and escaped \@ are not live prose), with its byte
// span in the raw body.
type MentionSpan struct {
	Start, End int
	Token      string
}

// liveProseSpans returns the live prose of body (code and escaped @ blanked to
// spaces, as liveProse renders them) and, for each byte of that string, the
// byte offset in body it was rendered from (-1 for a newline the prober
// synthesised between kept lines). Within one line the mapping is one-to-one,
// so a span found in the live prose maps back onto a contiguous span of body.
// Which lines are code is decided by the same block rules the web renderer
// applies (see prose_blocks.go).
func liveProseSpans(body string) (string, []int) {
	var out proseOutput
	lines := strings.Split(body, "\n")
	vlines := make([]proseLine, len(lines))
	offset := 0
	for n, line := range lines {
		pos := make([]int, len(line))
		for i := range pos {
			pos[i] = offset + i
		}
		vlines[n] = proseLine{text: line, pos: pos}
		offset += len(line) + 1
	}
	scanProseBlocks(&out, vlines, 0)
	return out.live.String(), out.pos
}

// proseMentionSpans lists every @token occurrence in the live prose of body,
// each with its span in the raw body. Unlike proseMentions it does not dedupe:
// every occurrence of a token is replaced (or displayed) on its own.
func proseMentionSpans(body string) []MentionSpan {
	live, pos := liveProseSpans(body)
	matches := mentionPattern.FindAllStringSubmatchIndex(live, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]MentionSpan, 0, len(matches))
	for _, m := range matches {
		start, tokenStart, tokenEnd, end := m[0], m[2], m[3], m[1]
		if pos[start] < 0 || pos[end-1] < 0 {
			continue // a match across a synthesised newline cannot happen; stay safe
		}
		token := strings.TrimRight(strings.ToLower(live[tokenStart:tokenEnd]), "-_")
		out = append(out, MentionSpan{Start: pos[start], End: pos[end-1] + 1, Token: token})
	}
	return out
}

// proseMarkedSpans lists every match of pattern in the live prose of body,
// where the match begins with a marker character (`#`) and its first capture
// group is the token after it. A match is kept only when the rune before the
// marker is not a word character, "/", "&", ".", "-" or the marker itself (the
// same guard the `#N` message-reference pattern applies), and each span covers
// the marker and the token in the raw body.
func proseMarkedSpans(body string, pattern *regexp.Regexp) []MentionSpan {
	live, pos := liveProseSpans(body)
	matches := pattern.FindAllStringSubmatchIndex(live, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]MentionSpan, 0, len(matches))
	for _, m := range matches {
		start, tokenStart, tokenEnd, end := m[0], m[2], m[3], m[1]
		if pos[start] < 0 || pos[end-1] < 0 {
			continue // a match across a synthesised newline cannot happen; stay safe
		}
		if !markerStartsToken(live, start) {
			continue
		}
		out = append(out, MentionSpan{Start: pos[start], End: pos[end-1] + 1, Token: live[tokenStart:tokenEnd]})
	}
	return out
}

// markerStartsToken says whether the rune before the marker leaves the token
// standalone: a word character or one of / & # . - glued to the marker makes it
// part of something else (a URL fragment, another reference, an identifier).
func markerStartsToken(live string, start int) bool {
	if start == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(live[:start])
	if r >= utf8.RuneSelf {
		return true // a non-ASCII rune is outside the guard's ASCII classes
	}
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		return false
	case r == '/' || r == '&' || r == '#' || r == '.' || r == '-':
		return false
	}
	return true
}

// roomAgentNameContext is what rewriting mentions needs about one room: the
// room's agents (live and revoked) with their names and creation order.
type roomAgentNameContext struct {
	projectID, projectName string
	members                []roomRecipient
}

func (tx *Tx) roomAgentNames(roomID string) (roomAgentNameContext, error) {
	members, err := tx.roomRecipients(roomID)
	if err != nil {
		return roomAgentNameContext{}, err
	}
	var projectID, projectName string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT p.id,p.name FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, roomID).Scan(&projectID, &projectName); err != nil {
		return roomAgentNameContext{}, err
	}
	return roomAgentNameContext{projectID: projectID, projectName: projectName, members: members}, nil
}

// liveAgentsByName indexes the room's live (not revoked) agents by lower slug,
// in creation order.
func (c roomAgentNameContext) liveAgentsByName() map[string][]roomRecipient {
	byName := map[string][]roomRecipient{}
	for _, member := range c.members {
		if member.kind != "agent" || member.revoked {
			continue
		}
		name := strings.ToLower(member.slug)
		byName[name] = append(byName[name], member)
	}
	for _, agents := range byName {
		sort.Slice(agents, func(i, j int) bool { return agentOrder(agents[i], agents[j]) })
	}
	return byName
}

func agentOrder(a, b roomRecipient) bool {
	if a.createdAt != b.createdAt {
		return a.createdAt < b.createdAt
	}
	return a.id < b.id
}

// byID finds a room member by its exact full ID (agents and people).
func (c roomAgentNameContext) byID(id string) (roomRecipient, bool) {
	for _, member := range c.members {
		if strings.ToLower(member.id) == strings.ToLower(id) {
			return member, true
		}
	}
	return roomRecipient{}, false
}

// resolveNameToken decides what one typed token names: the single live agent
// to store it as, or nobody (empty). It mirrors resolveMentions' precedence
// (exact ID, slug@project, exact slug, then a `-N` duplicate suffix) but only
// ever resolves to one live agent of the room, so ambiguous or non-agent
// tokens stay as they were written.
func (c roomAgentNameContext) resolveNameToken(token string) (roomRecipient, bool) {
	if groupTag(token) != "" {
		return roomRecipient{}, false
	}
	if member, ok := c.byID(token); ok {
		return member, member.kind == "agent"
	}
	if at := strings.Index(token, "@"); at >= 0 {
		parts := strings.SplitN(token, "@", 2)
		inProject := func(member roomRecipient) bool {
			return parts[1] == strings.ToLower(c.projectName) || parts[1] == strings.ToLower(c.projectID) ||
				c.projectID == AgentDMProjectID && (parts[1] == strings.ToLower(member.projectName) || parts[1] == strings.ToLower(member.projectID))
		}
		var found []roomRecipient
		for _, member := range c.liveAgentsByName()[parts[0]] {
			if inProject(member) {
				found = append(found, member)
			}
		}
		if len(found) == 1 {
			return found[0], true
		}
		return roomRecipient{}, false
	}
	if agents := c.liveAgentsByName()[token]; len(agents) == 1 {
		// A name one agent shares with a person's login addresses both (a live mention of an agent must always wake it),
		// so such a token keeps its text: storing only the agent's ID would drop the people.
		for _, member := range c.members {
			if member.kind == "user" && strings.ToLower(member.slug) == token {
				return roomRecipient{}, false
			}
		}
		return agents[0], true
	} else if len(agents) > 1 {
		return roomRecipient{}, false // ambiguous: two live agents share the name
	}
	if base, n, ok := splitDuplicateSuffix(token); ok {
		if agents := c.liveAgentsByName()[base]; len(agents) >= n && n >= 2 {
			return agents[n-1], true
		}
	}
	return roomRecipient{}, false
}

var duplicateSuffixPattern = regexp.MustCompile(`^(.+)-([0-9]+)$`)

// splitDuplicateSuffix reads a `name-2` style token as its base name and
// ordinal. A plain `name-2` that is somebody's exact slug is handled by the
// caller before this runs, so the ordinal reading never shadows a real name.
func splitDuplicateSuffix(token string) (string, int, bool) {
	m := duplicateSuffixPattern.FindStringSubmatch(token)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n < 2 {
		return "", 0, false
	}
	return m[1], n, true
}

// storeMentionsByID rewrites the agent mentions of a message body to agent IDs
// before the message is stored. It returns whether the body changed.
func (tx *Tx) storeMentionsByID(message *Message) (bool, error) {
	if message.AuthorKind == "system" || message.Body == "" {
		return false, nil
	}
	names, err := tx.roomAgentNames(message.RoomID)
	if err != nil {
		return false, err
	}
	spans := proseMentionSpans(message.Body)
	if len(spans) == 0 {
		return false, nil
	}
	replacements := map[string]string{}
	for _, span := range spans {
		if replacements[span.Token] != "" {
			continue
		}
		if _, ok := names.byID(span.Token); ok {
			continue // already stored as an ID
		}
		if member, ok := names.resolveNameToken(span.Token); ok {
			replacements[span.Token] = "@" + member.id
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

// replaceMentionSpans rewrites the given spans of body (from the last one back
// so the offsets stay valid). texts names what each span becomes; an empty
// text keeps the span as it is.
func replaceMentionSpans(body string, spans []MentionSpan, texts []string) (string, bool) {
	changed := false
	for i := len(spans) - 1; i >= 0; i-- {
		if texts[i] == "" {
			continue
		}
		span := spans[i]
		body = body[:span.Start] + texts[i] + body[span.End:]
		changed = true
	}
	return body, changed
}

// nameFreeClock is the clock the 24-hour part of the name-free rule reads. Tests replace it.
var nameFreeClock = time.Now

// nameFreeCutoff is the earliest stopped_at that still blocks a name: an agent
// stopped before it has been stopped for at least 24 hours.
func nameFreeCutoff() string {
	return nameFreeClock().UTC().Add(-24 * time.Hour).Format(timestampLayout)
}

// liveAgentCondition is the SQL that says an agent still holds its name: it is
// not removed, and it is not (stopped for at least 24 hours with its home room
// closed). The condition is a string, not a view, because liveness depends on
// the clock: no view or index can hold it. The caller must supply the cutoff
// parameter where the ? stands.
const liveAgentCondition = `(a.revoked_at IS NULL AND NOT (a.desired='stopped' AND a.stopped_at IS NOT NULL AND a.stopped_at<=? AND COALESCE((SELECT r.lifecycle='closed' FROM room r WHERE r.id=a.home_room_id),0)))`

// agentName is one looked-up agent: its canonical ID (as stored) and the name a reader sees.
type agentName struct {
	ID, Name string
}

// agentNamesByIDs names the given agent IDs the way a reader sees them: a
// removed agent by its kept removed_slug, a live one by its slug. The map is
// keyed by the lower-cased ID (mention tokens are lower-cased) and carries the
// canonical ID.
func agentNamesByIDs(ctx context.Context, q rowQuerier, ids []string) (map[string]agentName, error) {
	if len(ids) == 0 {
		return map[string]agentName{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = strings.ToLower(id)
	}
	rows, err := q.QueryContext(ctx, `SELECT lower(id),id,COALESCE(NULLIF(removed_slug,''),slug) FROM agent WHERE lower(id) IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]agentName{}
	for rows.Next() {
		var lower, id, slug string
		if err := rows.Scan(&lower, &id, &slug); err != nil {
			return nil, err
		}
		names[lower] = agentName{ID: id, Name: slug}
	}
	return names, rows.Err()
}

// roomDisplayNames gives the display name of every agent of one room: the
// agent's slug, with a `-2`, `-3`, ... suffix on every later agent that shares
// the slug with an earlier one among the room's current (not removed, still
// joined) agents. The earliest agent of a name keeps the plain name.
func roomDisplayNames(ctx context.Context, q rowQuerier, roomID string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.slug FROM room_member m JOIN agent a ON a.id=m.member_id
		WHERE m.room_id=? AND m.member_kind='agent' AND m.left_at IS NULL AND a.revoked_at IS NULL
		ORDER BY a.created_at,a.id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type member struct{ id, slug string }
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.id, &m.slug); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, m := range members {
		counts[strings.ToLower(m.slug)]++
	}
	seen := map[string]int{}
	out := map[string]string{}
	for _, m := range members {
		name := strings.ToLower(m.slug)
		if counts[name] > 1 {
			seen[name]++
			if seen[name] > 1 {
				out[m.id] = fmt.Sprintf("%s-%d", m.slug, seen[name])
				continue
			}
		}
		out[m.id] = m.slug
	}
	return out, nil
}

// displayMentions rewrites the stored agent-ID mentions of the given messages
// back to display names, room by room, and fills each message's Mentions
// array. It reads only; bodies in the database keep their IDs.
func displayMentions(ctx context.Context, q rowQuerier, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	rooms := map[string]bool{}
	ids := map[string]bool{}
	type preparedMessage struct {
		spans []MentionSpan
	}
	prepared := make([]preparedMessage, len(messages))
	for i, message := range messages {
		if message.Body == "" {
			continue
		}
		spans := proseMentionSpans(message.Body)
		for _, span := range spans {
			if strings.HasPrefix(span.Token, "agt_") {
				ids[span.Token] = true
				rooms[message.RoomID] = true
			}
		}
		prepared[i].spans = spans
	}
	if len(ids) == 0 {
		return nil
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	names, err := agentNamesByIDs(ctx, q, idList)
	if err != nil {
		return err
	}
	suffixes := map[string]map[string]string{}
	for roomID := range rooms {
		byRoom, err := roomDisplayNames(ctx, q, roomID)
		if err != nil {
			return err
		}
		suffixes[roomID] = byRoom
	}
	for i := range messages {
		message := &messages[i]
		if message.Body == "" {
			continue
		}
		spans := prepared[i].spans
		display := suffixes[message.RoomID]
		// Decide every span first, so the mentions array follows the body's own order.
		type shown struct {
			replacement string
			ok          bool
			ref         MentionRef
		}
		decided := make([]shown, len(spans))
		seen := map[string]bool{}
		for j, span := range spans {
			if !strings.HasPrefix(span.Token, "agt_") {
				continue
			}
			agent, ok := names[span.Token]
			if !ok {
				if !seen[span.Token] {
					seen[span.Token] = true
					decided[j].ref = MentionRef{ID: span.Token, Name: "", Resolved: false}
				}
				continue // unknown or deleted agent: the stored ID text stays
			}
			name := agent.Name
			if suffix, ok := display[agent.ID]; ok {
				name = suffix
			}
			decided[j].replacement, decided[j].ok = "@"+name, true
			decided[j].ref = MentionRef{ID: agent.ID, Name: name, Resolved: true}
		}
		var refs []MentionRef
		texts := make([]string, len(spans))
		for j, d := range decided {
			texts[j] = d.replacement
		}
		message.Body, _ = replaceMentionSpans(message.Body, spans, texts)
		for _, d := range decided {
			if d.ref.ID != "" && !d.ref.Resolved {
				refs = append(refs, d.ref)
			} else if d.ok {
				refs = append(refs, d.ref)
			}
		}
		if len(refs) > 0 {
			message.Mentions = refs
		}
	}
	return nil
}

// MentionEntity is one entry of an agent-facing entities table: an id the
// stored body mentions and the name the reading agent is shown for it (empty
// when the agent may not see one). Kind is "agent" or "person".
type MentionEntity struct {
	ID   string
	Name string
	Kind string
}

// mentionEntities builds the entities table of one stored body as the agent
// readerID (of readerProjectID) sees it, for the message that lives in roomID.
// Mentions are stored by agent ID, so the table covers the @agt_<id> tokens of
// the live prose in body order, one entry per distinct id; an @usr_<id> an
// agent wrote is covered as a person. The no-leak rule: a name is present only
// when the reader may see it — an agent of the reader's project (its slug, or
// the kept removed_slug of a removed agent), a member agent of a private chat
// between agents the reader sits in, or a person addressable in the message's
// room (its explicit members plus, outside private chats, the project's active
// humans, the same set @login addressing reads). An id whose holder is gone is
// left out entirely: the body keeps its stored text.
func mentionEntities(ctx context.Context, q rowQuerier, readerID, readerProjectID, roomID, body string) ([]MentionEntity, error) {
	mentionSpans := proseMentionSpans(body)
	roomSpans := proseRoomRefSpans(body)
	if len(mentionSpans) == 0 && len(roomSpans) == 0 {
		return nil, nil
	}
	// One pass over both kinds of reference in body order: a mention span and a
	// room span never overlap, so merging the two ordered lists by position is
	// enough.
	type scanned struct {
		span MentionSpan
		room bool
	}
	merged := make([]scanned, 0, len(mentionSpans)+len(roomSpans))
	mi, ri := 0, 0
	for mi < len(mentionSpans) || ri < len(roomSpans) {
		switch {
		case ri >= len(roomSpans) || (mi < len(mentionSpans) && mentionSpans[mi].Start < roomSpans[ri].Start):
			merged = append(merged, scanned{mentionSpans[mi], false})
			mi++
		default:
			merged = append(merged, scanned{roomSpans[ri], true})
			ri++
		}
	}
	var agentTokens, userTokens, roomTokens []string
	seenAgent, seenUser, seenRoom := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, item := range merged {
		token := strings.ToLower(item.span.Token)
		switch {
		case item.room:
			if !seenRoom[token] {
				seenRoom[token] = true
				roomTokens = append(roomTokens, token)
			}
		case strings.HasPrefix(token, "agt_") && !seenAgent[token]:
			seenAgent[token] = true
			agentTokens = append(agentTokens, token)
		case strings.HasPrefix(token, "usr_") && !seenUser[token]:
			seenUser[token] = true
			userTokens = append(userTokens, token)
		}
	}
	type holder struct {
		id, name, projectID, kind string
	}
	load := func(tokens []string, person bool) (map[string]holder, error) {
		out := map[string]holder{}
		if len(tokens) == 0 {
			return out, nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tokens)), ",")
		args := make([]any, len(tokens))
		for i, token := range tokens {
			args[i] = token
		}
		table, column, nameColumn, projectColumn := "agent a", "a.id", "COALESCE(NULLIF(a.removed_slug,''),a.slug)", "a.project_id"
		if person {
			table, column, nameColumn, projectColumn = "user u", "u.id", "u.login", "''"
		}
		rows, err := q.QueryContext(ctx, "SELECT "+column+","+nameColumn+","+projectColumn+" FROM "+table+
			" WHERE lower("+column+") IN ("+placeholders+")", args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var h holder
			if err := rows.Scan(&h.id, &h.name, &h.projectID); err != nil {
				return nil, err
			}
			out[strings.ToLower(h.id)] = h
		}
		return out, rows.Err()
	}
	agents, err := load(agentTokens, false)
	if err != nil {
		return nil, err
	}
	users, err := load(userTokens, true)
	if err != nil {
		return nil, err
	}
	rooms, roomMembers := map[string]roomRef{}, map[string]bool{}
	if len(roomTokens) > 0 {
		if rooms, err = loadRoomRefs(ctx, q, roomTokens); err != nil {
			return nil, err
		}
		if roomMembers, err = roomMembersOf(ctx, q, "agent", readerID, roomTokens); err != nil {
			return nil, err
		}
	}
	// Names the reader may see: an agent of its own project, a member agent of
	// its private chat (when the message is in one), a person addressable in
	// the room.
	nameable := map[string]bool{}
	if len(agents) > 0 {
		var kind, projectID string
		if err := q.QueryRowContext(ctx, `SELECT kind,COALESCE(project_id,'') FROM room WHERE id=?`, roomID).Scan(&kind, &projectID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		} else if err == nil && kind == "dm" && projectID == AgentDMProjectID {
			rows, err := q.QueryContext(ctx, `SELECT member_id FROM room_member WHERE room_id=? AND member_kind='agent' AND left_at IS NULL`, roomID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var memberID string
				if err := rows.Scan(&memberID); err != nil {
					rows.Close()
					return nil, err
				}
				nameable[memberID] = true
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
	}
	addressable := map[string]bool{}
	if len(users) > 0 {
		userRows, err := roomUserRows(q.QueryContext, ctx, roomID)
		if err != nil {
			return nil, err
		}
		for _, row := range userRows {
			addressable[row.id] = true
		}
	}
	out := []MentionEntity{}
	seen := map[string]bool{}
	for _, item := range merged {
		span := item.span
		var entity MentionEntity
		token := strings.ToLower(span.Token)
		switch {
		case item.room:
			ref := rooms[token]
			if ref.gone {
				continue // unknown or deleted: the body keeps the stored id text
			}
			name := ""
			if agentSeesRoom(ref, roomMembers[token], readerProjectID) {
				name = ref.title
			}
			entity = MentionEntity{ID: ref.id, Name: name, Kind: "room"}
		case strings.HasPrefix(token, "agt_"):
			h, ok := agents[token] // the map is keyed by lower(id); tokens are lower-cased
			if !ok {
				continue // unknown or deleted: the body keeps the stored text
			}
			name := h.name
			if h.projectID != readerProjectID && !nameable[h.id] {
				name = ""
			}
			entity = MentionEntity{ID: h.id, Name: name, Kind: "agent"}
		case strings.HasPrefix(token, "usr_"):
			h, ok := users[token]
			if !ok || !addressable[h.id] {
				continue
			}
			entity = MentionEntity{ID: h.id, Name: h.name, Kind: "person"}
		default:
			continue
		}
		if seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, entity)
	}
	return out, nil
}
