package server

import (
	"regexp"
	"slices"
	"strings"

	"github.com/hrubymar10/aimebu/internal/types"
)

var (
	// mentionRe finds @name mentions anywhere in the body.
	mentionRe = regexp.MustCompile(`(?i)@([a-z][a-z0-9]*)`)
	// legacyPrefixRe detects IRC-style "name:" speaker prefixes at the start of a body.
	legacyPrefixRe = regexp.MustCompile(`(?i)^([a-z][a-z0-9]{2,11})\s*:`)
)

// parseAddressedTo returns the deduplicated list of short names a message body
// is addressed to via @name mentions. Returns nil for room-wide messages.
func parseAddressedTo(body string) []string {
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		n = strings.ToLower(n)
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	return names
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

// annotate attaches addressing metadata to msgs as seen by agentName (the
// short name, e.g. "worker"). isDM is derived per-message from m.RoomID.
// knownAgents filters @mention captures to real agent names (pass nil to skip
// filtering — useful in tests or when the agent list is unavailable).
//
// should_respond logic mirrors the MCP etiquette:
//   - system messages: never respond.
//   - human sender, room-wide (no addressed_to): respond.
//   - human sender, addressed to others: do not respond.
//   - ai sender or unknown: respond only if addressed_to_me or in a DM room.
func annotate(msgs []types.Message, agentName string, knownAgents map[string]bool) []annotatedMessage {
	out := make([]annotatedMessage, len(msgs))
	for i, m := range msgs {
		addressed := parseAddressedTo(m.Body)
		if len(knownAgents) > 0 {
			filtered := make([]string, 0, len(addressed))
			for _, n := range addressed {
				if knownAgents[n] {
					filtered = append(filtered, n)
				}
			}
			addressed = filtered
		}
		addrToMe := slices.Contains(addressed, agentName)
		isDM := strings.HasPrefix(m.RoomID, "dm:")
		var shouldRespond bool
		switch m.FromKind {
		case "system":
			shouldRespond = false
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
	m := legacyPrefixRe.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	name := strings.ToLower(m[1])
	if !knownNames[name] {
		return "", false
	}
	return name, true
}

// agentShortName extracts the name portion from an agent ID.
// "worker@aimebu" → "worker"; bare "martin" → "martin".
func agentShortName(agentID string) string {
	if i := strings.IndexByte(agentID, '@'); i > 0 {
		return agentID[:i]
	}
	return agentID
}
