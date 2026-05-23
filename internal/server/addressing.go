package server

import (
	"regexp"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/types"
)

var (
	// mentionRe finds @slug and @slug@project mentions anywhere in the body.
	mentionRe = regexp.MustCompile(`(?i)@([a-z][a-z0-9_-]*(?:@[a-z0-9_.-]+)?)`)
	// legacyPrefixRe detects IRC-style "name:" speaker prefixes at the start of a body.
	legacyPrefixRe = regexp.MustCompile(`(?i)^([a-z][a-z0-9]{2,11})\s*:`)
	// inlinePrefixRe detects inline IRC-style "name1, name2 —" or
	// "name1 and name2:" addressing at the start of a sentence/paragraph.
	inlinePrefixRe = regexp.MustCompile(`(?i)(?:^|[.!?\n]\s*)([a-z][a-z0-9]{2,11})\s*(?:,\s*|\s+and\s+)([a-z][a-z0-9]{2,11})\s*(?:[—–\-]|:)`)
)

const hereRecentWindow = 5 * time.Minute

var (
	groupMentionKinds = map[string]string{
		"channel":  "channel",
		"here":     "here",
		"humans":   "humans",
		"ais":      "ais",
		"everyone": "everyone",
		"all":      "everyone",
	}
	reservedAgentNames = map[string]bool{
		"human":    true,
		"humans":   true,
		"ai":       true,
		"ais":      true,
		"everyone": true,
		"all":      true,
		"here":     true,
		"channel":  true,
		"system":   true,
		"_system":  true,
	}
)

var attentionMissPhrases = []string{
	"please review",
	"please approve",
	"please decide",
	"please confirm",
	"please check",
	"please look at",
	"let me know",
	"your call",
	"go-ahead",
	"sign off",
	"sign-off",
}

type maskedView struct {
	masked  string
	escaped string
}

type roomAgentContext struct {
	ID       string
	Name     string
	Kind     string
	LastSeen time.Time
	Waiting  bool
}

type addressingContext struct {
	SenderID   string
	KnownNames map[string]bool
	RoleNames  map[string][]string
	RoomAgents []roomAgentContext
	Now        time.Time
}

// maskCodeForAddressing replaces code-region content with whitespace-preserving
// placeholders so downstream regexes see only live prose. Covered in v1:
// fenced blocks (``` and ~~~), inline backticks, and narrow CommonMark-style
// indented code blocks (preceded by BOF/blank line, then continuing while
// indentation holds). Escaped mentions (\@name) are preserved here and handled
// by stripEscapedMentions after masking.
func maskCodeForAddressing(body string) maskedView {
	lines := strings.Split(body, "\n")
	maskedLines := make([]string, len(lines))
	lineMaps := make([]string, len(lines))

	inFence := false
	fenceDelim := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inFence && isFenceStart(trimmed) {
			inFence = true
			fenceDelim = trimmed[:3]
			maskedLines[i] = maskInlineCode(line)
			lineMaps[i] = line
			continue
		}
		if inFence {
			maskedLines[i] = maskLineContent(line)
			lineMaps[i] = maskLineContent(line)
			if isFenceEnd(trimmed, fenceDelim) {
				inFence = false
				fenceDelim = ""
			}
			continue
		}
		maskedLines[i] = maskInlineCode(line)
		lineMaps[i] = line
	}

	applyIndentedCodeMask(maskedLines, lineMaps)

	masked := strings.Join(maskedLines, "\n")
	return maskedView{
		masked:  masked,
		escaped: stripEscapedMentions(masked),
	}
}

func isFenceStart(trimmed string) bool {
	return isFenceDelimiter(trimmed, "```") || isFenceDelimiter(trimmed, "~~~")
}

func isFenceEnd(trimmed, delim string) bool {
	return isFenceDelimiter(trimmed, delim)
}

func isFenceDelimiter(trimmed, delim string) bool {
	if !strings.HasPrefix(trimmed, delim) {
		return false
	}
	rest := trimmed[len(delim):]
	if rest == "" {
		return true
	}
	for _, r := range rest {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func maskInlineCode(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	inCode := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '`' {
			inCode = !inCode
			b.WriteByte('`')
			continue
		}
		if inCode {
			if ch == '\t' {
				b.WriteByte('\t')
			} else {
				b.WriteByte(' ')
			}
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func maskLineContent(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); i++ {
		if line[i] == '\t' {
			b.WriteByte('\t')
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func applyIndentedCodeMask(maskedLines, lineMaps []string) {
	inIndented := false
	for i, line := range lineMaps {
		blank := strings.TrimSpace(line) == ""
		if inIndented {
			if blank {
				maskedLines[i] = line
				inIndented = false
				continue
			}
			if hasIndentedCodePrefix(line) {
				maskedLines[i] = maskIndentedCodeLine(line)
				continue
			}
			inIndented = false
		}
		if blank {
			maskedLines[i] = line
			continue
		}
		if !hasIndentedCodePrefix(line) {
			continue
		}
		if i > 0 && strings.TrimSpace(lineMaps[i-1]) != "" {
			continue
		}
		maskedLines[i] = maskIndentedCodeLine(line)
		inIndented = true
	}
}

func hasIndentedCodePrefix(line string) bool {
	if strings.HasPrefix(line, "\t") {
		return true
	}
	return strings.HasPrefix(line, "    ")
}

func maskIndentedCodeLine(line string) string {
	if strings.HasPrefix(line, "\t") {
		return "\t" + maskLineContent(line[1:])
	}
	if strings.HasPrefix(line, "    ") {
		return "    " + maskLineContent(line[4:])
	}
	return maskLineContent(line)
}

func stripEscapedMentions(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) && body[i+1] == '@' {
			b.WriteString("  ")
			i++
			continue
		}
		b.WriteByte(body[i])
	}
	return b.String()
}

// parseAddressedTo returns the deduplicated list of raw @tokens captured from
// live prose (outside code, outside escaped \@literal forms). The returned
// tokens are lowercased and may include group tags such as "here" or
// "everyone"; room-aware resolution happens later.
func parseAddressedTo(body string) []string {
	view := maskCodeForAddressing(body)
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		n = strings.ToLower(n)
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, m := range mentionRe.FindAllStringSubmatch(view.escaped, -1) {
		add(m[1])
	}
	return names
}

func isReservedAgentName(name string) bool {
	return reservedAgentNames[strings.ToLower(name)]
}

func resolveAddressedTo(body string, ctx addressingContext) []string {
	return resolveAddressedTokens(parseAddressedTo(body), ctx)
}

func resolveAddressedTokens(raw []string, ctx addressingContext) []string {
	if len(raw) == 0 {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	senderSlug := strings.ToLower(agentShortName(ctx.SenderID))
	senderID := strings.ToLower(ctx.SenderID)
	slugCounts := make(map[string]int, len(ctx.RoomAgents))
	for _, agent := range ctx.RoomAgents {
		slugCounts[strings.ToLower(agent.Name)]++
	}
	add := func(name string) {
		name = strings.ToLower(name)
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	addAgent := func(agent roomAgentContext) {
		if slugCounts[strings.ToLower(agent.Name)] > 1 {
			add(agent.ID)
			return
		}
		add(agent.Name)
	}

	for _, token := range raw {
		switch groupMentionKinds[token] {
		case "channel", "everyone":
			for _, agent := range ctx.RoomAgents {
				if strings.EqualFold(agent.ID, senderID) {
					continue
				}
				addAgent(agent)
			}
		case "humans":
			for _, agent := range ctx.RoomAgents {
				if strings.EqualFold(agent.ID, senderID) || agent.Kind != "human" {
					continue
				}
				addAgent(agent)
			}
		case "ais":
			for _, agent := range ctx.RoomAgents {
				if strings.EqualFold(agent.ID, senderID) || agent.Kind != "ai" {
					continue
				}
				addAgent(agent)
			}
		case "here":
			for _, agent := range ctx.RoomAgents {
				if strings.EqualFold(agent.ID, senderID) {
					continue
				}
				if agent.Waiting || (!agent.LastSeen.IsZero() && ctx.Now.Sub(agent.LastSeen) < hereRecentWindow) {
					addAgent(agent)
				}
			}
		default:
			if strings.Contains(token, "@") {
				for _, agent := range ctx.RoomAgents {
					if strings.EqualFold(agent.ID, token) {
						add(agent.ID)
						break
					}
				}
			} else if matches := roomAgentsBySlug(ctx.RoomAgents, token); len(matches) == 1 {
				add(matches[0].Name)
			} else if len(matches) > 1 {
				continue
			} else if len(ctx.KnownNames) == 0 || ctx.KnownNames[token] {
				add(token)
			} else if len(ctx.RoleNames) > 0 {
				for _, name := range ctx.RoleNames[token] {
					if strings.EqualFold(name, senderSlug) {
						continue
					}
					add(name)
				}
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

func roomAgentsBySlug(agents []roomAgentContext, slug string) []roomAgentContext {
	var matches []roomAgentContext
	for _, agent := range agents {
		if strings.EqualFold(agent.Name, slug) {
			matches = append(matches, agent)
		}
	}
	return matches
}

func ambiguousMentionTokens(raw []string, ctx addressingContext) []string {
	if len(raw) == 0 || len(ctx.RoomAgents) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, token := range raw {
		if strings.Contains(token, "@") || groupMentionKinds[token] != "" {
			continue
		}
		if len(ctx.RoleNames) > 0 && len(ctx.RoleNames[token]) > 0 {
			continue
		}
		if len(roomAgentsBySlug(ctx.RoomAgents, token)) < 2 {
			continue
		}
		if !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

func addressedMatchesAgent(target, agentID string) bool {
	if strings.EqualFold(target, agentID) {
		return true
	}
	if !strings.Contains(target, "@") && strings.EqualFold(target, agentShortName(agentID)) {
		return true
	}
	return false
}

// annotatedMessage is a types.Message with per-agent addressing guidance
// attached. Fields are computed at read time so agents can act on structured
// data instead of re-parsing etiquette prose.
type annotatedMessage struct {
	types.Message
	AddressedTo   []string `json:"addressed_to"`
	AddressedToMe bool     `json:"addressed_to_me"`
	ShouldRespond bool     `json:"should_respond"`
}

// annotate attaches addressing metadata to msgs as seen by agentID. The
// viewer may be a full AI ID (worker@aimebu) or a legacy short name
// (worker). isDM is derived per-message from m.RoomID.
// contextFor supplies the room-aware resolver context for each message.
//
// should_respond logic mirrors the MCP etiquette:
//   - system messages: respond only when explicitly targeted.
//   - human sender, room-wide (no addressed_to): respond.
//   - human sender, addressed to others: do not respond.
//   - ai sender or unknown: respond only if addressed_to_me or in a DM room.
func annotate(msgs []types.Message, agentID string, contextFor func(types.Message) addressingContext) []annotatedMessage {
	out := make([]annotatedMessage, len(msgs))
	for i, m := range msgs {
		ctx := addressingContext{}
		if contextFor != nil {
			ctx = contextFor(m)
		}
		addressed := m.Targets
		if addressed == nil {
			addressed = resolveAddressedTo(m.Body, ctx)
		}
		addrToMe := false
		for _, target := range addressed {
			if addressedMatchesAgent(target, agentID) {
				addrToMe = true
				break
			}
		}
		isDM := strings.HasPrefix(m.RoomID, "dm:")
		var shouldRespond bool
		switch m.FromKind {
		case "system":
			shouldRespond = addrToMe
		case "human":
			if len(addressed) == 0 {
				shouldRespond = true
			} else {
				shouldRespond = addrToMe
			}
		default: // "ai" or legacy empty
			shouldRespond = addrToMe || isDM
		}
		out[i] = annotatedMessage{
			Message:       m,
			AddressedTo:   addressed,
			AddressedToMe: addrToMe,
			ShouldRespond: shouldRespond,
		}
	}
	return out
}

// parseLegacyPrefix detects a legacy IRC-style "name:" speaker prefix at the
// start of body. Returns (matchedName, true) when the prefix matches a name in
// knownNames (lowercase short names of registered agents). Returns ("", false)
// for @-addressed messages, ordinary prose labels like "Note:" or "URL:", and
// any body that does not start with the pattern.
func parseLegacyPrefix(body string, knownNames map[string]bool) (string, bool) {
	view := maskCodeForAddressing(body)
	m := legacyPrefixRe.FindStringSubmatch(view.escaped)
	if m == nil {
		return "", false
	}
	name := strings.ToLower(m[1])
	if !knownNames[name] {
		return "", false
	}
	return name, true
}

// parseInlineLegacyPrefix detects inline IRC-style multi-addressee addressing
// such as "worker, reviewer —" or "worker and reviewer:" at the start of a
// sentence or paragraph. Returns the matched names in order only when both
// tokens resolve to real registered agent short names.
func parseInlineLegacyPrefix(body string, knownNames map[string]bool) ([]string, bool) {
	view := maskCodeForAddressing(body)
	m := inlinePrefixRe.FindStringSubmatch(view.escaped)
	if m == nil {
		return nil, false
	}
	first := strings.ToLower(m[1])
	second := strings.ToLower(m[2])
	if !knownNames[first] || !knownNames[second] {
		return nil, false
	}
	if first == second {
		return []string{first}, true
	}
	return []string{first, second}, true
}

// parseAttentionMiss returns the first high-signal handoff phrase found in the
// body. The phrase list is intentionally conservative. Known limitation: this
// simple substring match does not try to distinguish quoted prose from a live
// request (e.g. `matin said: "please approve"`).
func parseAttentionMiss(body string) (string, bool) {
	view := maskCodeForAddressing(body)
	lower := strings.ToLower(view.escaped)
	for _, phrase := range attentionMissPhrases {
		if strings.Contains(lower, phrase) {
			return phrase, true
		}
	}
	return "", false
}

// agentShortName extracts the name portion from an agent ID.
// "worker@aimebu" → "worker"; bare "martin" → "martin".
func agentShortName(agentID string) string {
	if i := strings.IndexByte(agentID, '@'); i > 0 {
		return agentID[:i]
	}
	return agentID
}
