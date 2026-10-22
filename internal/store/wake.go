package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var mentionPattern = regexp.MustCompile(`(?i)@([a-z][a-z0-9_-]*(?:@[a-z0-9_.-]+)?)`)

type roomRecipient struct {
	id, slug, kind, role string
	lead, revoked        bool
	// auto marks a lead that is in the room only because leads are put into every project room: it is addressable, and woken when addressed,
	// but an unaddressed human message does not wake it.
	auto bool
	// implicit marks a project human who can read the room without a
	// room_member row. They are addressable by login, but group tags
	// (@channel, @humans, ...) keep meaning the room's explicit members.
	implicit bool
}

func (tx *Tx) roomRecipients(roomID string) ([]roomRecipient, error) {
	return roomRecipientsFrom(tx.tx.QueryContext, tx.ctx, roomID)
}

func roomRecipientsFrom(query func(context.Context, string, ...any) (*sql.Rows, error), ctx context.Context, roomID string) ([]roomRecipient, error) {
	rows, err := query(ctx, `SELECT a.id,a.slug,r.role_key,r.cap_is_lead,a.revoked_at IS NOT NULL,m.auto=1
		FROM room_member m JOIN agent a ON a.id=m.member_id JOIN role r ON r.id=a.role_id
		WHERE m.room_id=? AND m.member_kind='agent' AND m.left_at IS NULL
		ORDER BY a.id`, roomID)
	if err != nil {
		return nil, err
	}
	var members []roomRecipient
	for rows.Next() {
		var item roomRecipient
		item.kind = "agent"
		if err := rows.Scan(&item.id, &item.slug, &item.role, &item.lead, &item.revoked, &item.auto); err != nil {
			rows.Close()
			return nil, err
		}
		members = append(members, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	users, err := roomUserRows(query, ctx, roomID)
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		members = append(members, roomRecipient{id: user.id, slug: user.login, kind: "user", implicit: user.implicit})
	}
	return members, nil
}

type roomUserRow struct {
	id, login string
	implicit  bool
}

// roomUserRows lists the humans addressable by `@login` in a room: its active
// room_member user rows, plus, outside private chats (kind dm), every other
// active (not disabled) member of the room's project. Project humans can read an ordinary room
// anyway; a DM stays limited to its two peers. Rows are ordered by user id
// (explicit and implicit members interleaved), the same for server addressing
// and for the client's mention-users list.
func roomUserRows(query func(context.Context, string, ...any) (*sql.Rows, error), ctx context.Context, roomID string) ([]roomUserRow, error) {
	rows, err := query(ctx, `SELECT u.id,u.login,0 FROM room_member m JOIN user u ON u.id=m.member_id
		WHERE m.room_id=? AND m.member_kind='user' AND m.left_at IS NULL
		UNION
		SELECT u.id,u.login,1 FROM room r JOIN membership pm ON pm.project_id=r.project_id JOIN user u ON u.id=pm.user_id
		WHERE r.id=? AND r.kind!='dm' AND u.disabled_at IS NULL AND NOT EXISTS (SELECT 1 FROM room_member m WHERE m.room_id=r.id
			AND m.member_kind='user' AND m.member_id=u.id AND m.left_at IS NULL)
		ORDER BY 1`, roomID, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []roomUserRow
	for rows.Next() {
		var row roomUserRow
		if err := rows.Scan(&row.id, &row.login, &row.implicit); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// resolveMessageTargets freezes addressing at insertion time. Targets are
// member IDs, so later role changes or room joins cannot rewrite a message.
func (tx *Tx) resolveMessageTargets(message Message) ([]string, map[string]string, *LoopGuard, error) {
	members, err := tx.roomRecipients(message.RoomID)
	if err != nil {
		return nil, nil, nil, err
	}
	var projectID, projectName string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT p.id,p.name FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, message.RoomID).Scan(&projectID, &projectName); err != nil {
		return nil, nil, nil, err
	}
	byID := make(map[string]roomRecipient, len(members))
	for _, member := range members {
		byID[member.id] = member
	}
	targets := resolveMentionTargets(message.Body, message.AuthorID, members, projectID, projectName)
	add := func(member roomRecipient, reason string) {
		if member.id == message.AuthorID {
			return
		}
		// A reply is the weakest reason: it never replaces the mention, role or group that already targets the member.
		if _, ok := targets[member.id]; ok && reason == "reply" {
			return
		}
		targets[member.id] = reason
	}
	// Replies, DMs, handoffs, and inbox targets extend or replace mentions.
	// The mention resolver is also used to rank a turn's bounded roster.
	// A reply to a person always targets them (room visibility). A reply from
	// one agent to another wakes the parent's author as well, behind a hop
	// guard: after ReplyWakeHops automatic replies in a row with no person's
	// message between them, the next reply still threads but wakes nobody.
	var guard *LoopGuard
	if message.ReplyTo != "" {
		var author string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT author_id FROM message WHERE id=? AND room_id=?`, message.ReplyTo, message.RoomID).Scan(&author)
		if err != nil && err != sql.ErrNoRows {
			return nil, nil, nil, err
		}
		if member, ok := byID[author]; ok {
			switch {
			case message.AuthorKind != "agent" || member.kind == "user":
				add(member, "reply")
			case member.kind == "agent" && member.id != message.AuthorID:
				// A parent the message already targets some other way (a mention, a role, a group) is woken by that, and the
				// guard has nothing to block.
				if _, already := targets[member.id]; already {
					break
				}
				prior, err := tx.recentReplyWakes(message.RoomID, message.ID)
				if err != nil {
					return nil, nil, nil, err
				}
				if prior < ReplyWakeHops {
					add(member, "reply")
				} else if announced, err := tx.loopGuardAnnounced(message.RoomID); err != nil {
					return nil, nil, nil, err
				} else if !announced {
					guard = &LoopGuard{ReplyTo: message.ReplyTo, Hops: prior}
				}
			}
		}
	}
	if message.AuthorKind == "agent" || message.AuthorKind == "user" {
		var kind string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT kind FROM room WHERE id=?`, message.RoomID).Scan(&kind); err != nil {
			return nil, nil, nil, err
		}
		if kind == "dm" {
			for _, member := range members {
				if member.kind == "user" {
					add(member, "dm")
				}
			}
		}
		if message.Handoff {
			for _, member := range members {
				if member.kind == "agent" && member.lead {
					add(member, "handoff")
				}
			}
		}
	}
	if message.OnlyTargets != nil {
		targets = map[string]string{}
		for _, id := range message.OnlyTargets {
			if member, ok := byID[id]; ok {
				add(member, "inbox_answer")
			}
		}
	}
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, targets, guard, nil
}

// resolveMentionTargets applies the same name, role, project, and group
// precedence for wake addressing and turn-roster ranking.
func resolveMentionTargets(body, authorID string, members []roomRecipient, projectID, projectName string) map[string]string {
	return resolveMentions(body, authorID, members, projectID, projectName, nil)
}

// resolveMentions is the one resolver. When trace is non-nil it records, for
// each distinct live token, how the token was read and who it reached; the
// debug inspector uses that to explain an invisible miss. The trace is
// observation only: addressing is identical with or without it.
func resolveMentions(body, authorID string, members []roomRecipient, projectID, projectName string, trace *[]MentionTrace) map[string]string {
	byLowerID := make(map[string]roomRecipient, len(members))
	bySlug := map[string][]roomRecipient{}
	// Revoked agents no longer hold their slug: they must not hide the live
	// holders of a same-named role. They stay addressable by slug only when
	// nothing live answers to the token, so the wake is still recorded as
	// skipped rather than silently dropped.
	revokedBySlug := map[string][]roomRecipient{}
	for _, member := range members {
		byLowerID[strings.ToLower(member.id)] = member
		if member.revoked {
			revokedBySlug[strings.ToLower(member.slug)] = append(revokedBySlug[strings.ToLower(member.slug)], member)
			continue
		}
		bySlug[strings.ToLower(member.slug)] = append(bySlug[strings.ToLower(member.slug)], member)
	}
	targets := map[string]string{}
	add := func(member roomRecipient, reason string) {
		if member.id != authorID {
			if mentionReasonStrength(reason) > mentionReasonStrength(targets[member.id]) {
				targets[member.id] = reason
			}
		}
	}
	// record notes one token's reading. Members are listed even when the
	// sender is among them; SelfExcluded says the sender was dropped.
	record := func(token, kind, outcome, reason string, reached []roomRecipient) {
		if trace == nil {
			return
		}
		entry := MentionTrace{Token: token, Kind: kind, Outcome: outcome, Reason: reason, Reached: []TracePerson{}}
		for _, member := range reached {
			if member.id == authorID {
				entry.SelfExcluded = true
				continue
			}
			entry.Reached = append(entry.Reached, TracePerson{ID: member.id, Name: member.slug, Kind: member.kind})
		}
		if outcome == "addressed" && len(entry.Reached) == 0 {
			if entry.SelfExcluded {
				entry.Outcome = "self"
				entry.Reason += "; it names only the sender, who is never addressed by their own message"
			} else {
				entry.Outcome = "empty"
				entry.Reason += "; nobody is in that group right now"
			}
		}
		*trace = append(*trace, entry)
	}
	tracing := trace != nil
	for _, token := range proseMentions(body) {
		switch groupTag(token) {
		case groupMembers:
			var reached []roomRecipient
			for _, member := range members {
				if !member.implicit {
					add(member, "group")
					if tracing {
						reached = append(reached, member)
					}
				}
			}
			record(token, "group", "addressed", "group tag: every explicit room member (agents and humans in the room), except project humans who are not members", reached)
		case groupAIs:
			var reached []roomRecipient
			for _, member := range members {
				if member.kind == "agent" {
					add(member, "group")
					if tracing {
						reached = append(reached, member)
					}
				}
			}
			record(token, "group", "addressed", "group tag: every AI agent in the room", reached)
		case groupHumans:
			var reached []roomRecipient
			for _, member := range members {
				if member.kind == "user" && !member.implicit {
					add(member, "group")
					if tracing {
						reached = append(reached, member)
					}
				}
			}
			record(token, "group", "addressed", "group tag: every human who is an explicit room member", reached)
		default:
			if member, ok := byLowerID[token]; ok {
				add(member, "mention")
				record(token, "full_id", "addressed", "exact full ID of a room member", []roomRecipient{member})
				continue
			}
			if strings.Contains(token, "@") {
				parts := strings.SplitN(token, "@", 2)
				if parts[1] == strings.ToLower(projectName) || parts[1] == strings.ToLower(projectID) {
					var reached []roomRecipient
					for _, member := range append(append([]roomRecipient(nil), bySlug[parts[0]]...), revokedBySlug[parts[0]]...) {
						if member.kind == "agent" {
							add(member, "mention")
							if tracing {
								reached = append(reached, member)
							}
						}
					}
					if len(reached) == 0 {
						record(token, "project_qualified", "no_match", fmt.Sprintf("the project part matches, but no agent in this room has the slug %q", parts[0]), nil)
					} else {
						record(token, "project_qualified", "addressed", "slug plus project: the agent with that slug in this project", reached)
					}
				} else {
					record(token, "project_qualified", "no_match", fmt.Sprintf("the project part %q is neither this project's name nor its ID, so nothing matches", parts[1]), nil)
				}
				continue
			}
			if matches := bySlug[token]; len(matches) == 1 {
				add(matches[0], "mention") // An exact slug wins over a role.
				record(token, "slug", "addressed", "exact slug of a room member (a slug wins over a role of the same name)", matches)
			} else if len(matches) > 1 {
				record(token, "slug", "ambiguous", fmt.Sprintf("%d room members share the slug %q, so nobody is addressed; write the full form such as @%s@%s to address one", len(matches), token, token, projectName), matches)
			} else {
				roleHit := false
				var reached []roomRecipient
				for _, member := range members {
					if member.kind == "agent" && !member.revoked && strings.EqualFold(member.role, token) {
						add(member, "role")
						if tracing {
							reached = append(reached, member)
						}
						roleHit = true
					}
				}
				if roleHit {
					record(token, "role", "addressed", "assigned role key: the AI agents currently holding that role in this room", reached)
					continue
				}
				var revoked []roomRecipient
				for _, member := range members {
					if member.kind == "agent" && member.revoked && strings.EqualFold(member.role, token) {
						add(member, "role")
						if tracing {
							revoked = append(revoked, member)
						}
					}
				}
				for _, member := range revokedBySlug[token] {
					add(member, "mention")
					if tracing {
						revoked = append(revoked, member)
					}
				}
				if len(revoked) > 0 {
					record(token, "revoked", "addressed", "only revoked agents answer to this name; they are recorded as not woken", revoked)
				} else {
					record(token, "none", "no_match", "no room member, role key, or group tag has this name", nil)
				}
			}
		}
	}
	if trace != nil {
		for _, ignored := range ignoredMentions(body) {
			// Only a token that would have reached someone was a real miss;
			// Java annotations, user@host, and other code noise are left out.
			var probe []MentionTrace
			would := resolveMentions("@"+ignored.Token, authorID, members, projectID, projectName, &probe)
			reachable := len(would) > 0
			for _, entry := range probe {
				reachable = reachable || entry.Outcome == "ambiguous"
				if entry.Outcome == "addressed" || entry.Outcome == "ambiguous" {
					ignored.WouldReach = append(ignored.WouldReach, entry.Reached...)
				}
			}
			if reachable {
				*trace = append(*trace, ignored)
			}
		}
	}
	return targets
}

// ignoredMentions lists @tokens that appear in the raw body but never in live
// prose, which is the invisible miss: they sit in inline code, a fence, an
// indented block, or are escaped with a backslash, so they address nobody.
func ignoredMentions(body string) []MentionTrace {
	live := map[string]bool{}
	for _, token := range proseMentions(body) {
		live[token] = true
	}
	seen := map[string]bool{}
	out := []MentionTrace{}
	for _, match := range mentionPattern.FindAllStringSubmatch(body, -1) {
		name := strings.TrimRight(strings.ToLower(match[1]), "-_")
		if live[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, MentionTrace{Token: name, Kind: "ignored", Outcome: "ignored", Reached: []TracePerson{}, WouldReach: []TracePerson{},
			Reason: "written inside backticks or a code block, or escaped with a backslash, so it is shown literally and addresses nobody"})
	}
	return out
}

func mentionReasonStrength(reason string) int {
	switch reason {
	case "mention":
		return 3
	case "role":
		return 2
	case "group":
		return 1
	default:
		return 0
	}
}

// ReplyWakeHops is how many agent-to-agent reply wakes a room allows in a row, with no person's message between them, inside
// ReplyWakeWindow.
const ReplyWakeHops = 4

// ReplyWakeWindow bounds the count to recent history, so an old exchange does not silence replies hours later.
const ReplyWakeWindow = 30 * time.Minute

// LoopGuard says an agent's reply was threaded but deliberately did not wake its parent's author, because the room had already seen
// ReplyWakeHops such wakes. Hops is the number of wakes that were allowed.
type LoopGuard struct {
	ReplyTo string
	Hops    int
}

// recentReplyWakes counts, in a room, the agent messages that woke another agent by reply since the last message from a person, within
// ReplyWakeWindow. Counting per room rather than up one reply chain means a reply to an older message does not start the count again;
// a person's message does. Only wakes that happened count, so a reply that was blocked, or whose parent a mention woke anyway, adds nothing.
func (tx *Tx) recentReplyWakes(roomID, excludeID string) (int, error) {
	cutoff := time.Now().UTC().Add(-ReplyWakeWindow).Format(timestampLayout)
	var count int
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(DISTINCT m.id) FROM note n JOIN message m ON m.id=n.message_id
		WHERE m.room_id=? AND m.id<>? AND m.author_kind='agent' AND n.wake_reason='reply' AND m.created_at>=?
		AND m.seq>COALESCE((SELECT MAX(seq) FROM message WHERE room_id=? AND author_kind='user'),0)`,
		roomID, excludeID, cutoff, roomID).Scan(&count)
	return count, err
}

// loopGuardAnnounced says the room has already been told about a stopped exchange since the last message from a person, so the notice
// appears once per run of replies however many more are blocked.
func (tx *Tx) loopGuardAnnounced(roomID string) (bool, error) {
	cutoff := time.Now().UTC().Add(-ReplyWakeWindow).Format(timestampLayout)
	var found int
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE kind='loop_guard' AND room_id=? AND created_at>=?
		AND created_at>=COALESCE((SELECT MAX(created_at) FROM message WHERE room_id=? AND author_kind='user'),''))`, roomID, cutoff, roomID).Scan(&found)
	return found == 1, err
}

func (tx *Tx) queueMessageWakes(message Message, targets map[string]string, projectID string) error {
	members, err := tx.roomRecipients(message.RoomID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.kind != "agent" {
			continue
		}
		reason := targets[member.id]
		if message.AuthorKind == "user" && len(targets) == 0 && message.OnlyTargets == nil {
			reason = "human_message"
		}
		if message.AuthorKind == "system" && message.OnlyTargets == nil {
			reason = ""
		}
		skip := ""
		switch {
		case member.id == message.AuthorID:
			skip = "self"
		case member.revoked:
			skip = "revoked"
		case message.AuthorKind == "system" && message.OnlyTargets == nil:
			skip = "system_chatter"
		case reason == "":
			skip = "not_addressed"
		case reason == "human_message" && member.auto:
			skip = "auto_member" // the lead reads this room but was not put in it; an unaddressed message is the worker's
		}
		if skip != "" {
			if _, err := tx.AppendEvent(Event{Kind: "message_not_woken", ProjectID: projectID, RoomID: message.RoomID,
				AgentID: member.id, PayloadJSON: payloadJSON(map[string]string{"message_id": message.ID, "reason": skip})}); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.InsertNote(Note{AgentID: member.id, MessageID: message.ID, RoomID: message.RoomID, WakeReason: reason}); err != nil {
			return err
		}
	}
	return nil
}

// proseMentions ignores fenced, inline and indented code, plus escaped @.
// liveProse returns body with code (fenced, indented, inline) and escaped
// \@ mentions blanked to spaces, so only live prose remains for addressing.
func liveProse(body string) string {
	var live strings.Builder
	inFence, fence := false, ""
	lines := strings.Split(body, "\n")
	previousBlank := true
	indented := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker := trimmed[:3]
			if !inFence {
				inFence, fence = true, marker
			} else if marker == fence {
				inFence = false
			}
			live.WriteByte('\n')
			previousBlank = false
			continue
		}
		if inFence {
			live.WriteByte('\n')
			previousBlank = trimmed == ""
			continue
		}
		if trimmed == "" {
			indented = false
			previousBlank = true
			live.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			if previousBlank || indented {
				indented = true
				live.WriteByte('\n')
				previousBlank = false
				continue
			}
		}
		indented = false
		codeRun := 0
		for i := 0; i < len(line); i++ {
			switch line[i] {
			case '`':
				end := i + 1
				for end < len(line) && line[end] == '`' {
					end++
				}
				run := end - i
				if codeRun == run {
					codeRun = 0
				} else if codeRun == 0 && hasClosingBacktickRun(line, end, run) {
					codeRun = run
				}
				for j := i; j < end; j++ {
					live.WriteByte(' ')
				}
				i = end - 1
			case '\\':
				if i+1 < len(line) && line[i+1] == '@' {
					live.WriteString("  ")
					i++
				} else {
					live.WriteByte(line[i])
				}
			default:
				if codeRun != 0 {
					live.WriteByte(' ')
				} else {
					live.WriteByte(line[i])
				}
			}
		}
		live.WriteByte('\n')
		previousBlank = false
	}
	return live.String()
}

// The live group tags, once: who a group-tagged message reaches is decided by groupTag.
const (
	groupMembers = "members" // @channel, @here, @everyone, @all: every explicit room member
	groupAIs     = "ais"     // @ais: every AI agent in the room
	groupHumans  = "humans"  // @humans: every explicit human member
)

// groupTag classifies a lower-cased @token: one of the group constants, or "" when it is a name (a slug, a role, a full ID).
func groupTag(token string) string {
	switch token {
	case "channel", "everyone", "all", "here":
		return groupMembers
	case "ais":
		return groupAIs
	case "humans":
		return groupHumans
	}
	return ""
}

func proseMentions(body string) []string {
	seen := map[string]bool{}
	var mentions []string
	for _, match := range mentionPattern.FindAllStringSubmatch(liveProse(body), -1) {
		name := strings.TrimRight(strings.ToLower(match[1]), "-_")
		if !seen[name] {
			seen[name] = true
			mentions = append(mentions, name)
		}
	}
	return mentions
}

func hasClosingBacktickRun(line string, from, length int) bool {
	for i := from; i < len(line); i++ {
		if line[i] != '`' {
			continue
		}
		end := i + 1
		for end < len(line) && line[end] == '`' {
			end++
		}
		if end-i == length {
			return true
		}
		i = end - 1
	}
	return false
}
