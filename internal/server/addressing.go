package server

import (
	"regexp"
	"slices"
	"strings"

	"github.com/hrubymar10/aimebu/internal/types"
)

var (
	// prefixRe matches a leading address list like "name:", "a, b:", "a and b:".
	prefixRe = regexp.MustCompile(`(?i)^((?:[a-z][a-z0-9]*(?:\s*,\s*|\s+and\s+))*[a-z][a-z0-9]*)\s*:`)
	// separatorRe splits a prefix match like "a, b and c" into individual names.
	separatorRe = regexp.MustCompile(`(?i)\s*,\s*|\s+and\s+`)
	// mentionRe finds @name mentions anywhere in the body.
	mentionRe = regexp.MustCompile(`(?i)@([a-z][a-z0-9]*)`)
)

// parseAddressedTo returns the deduplicated list of short names a message body
// is addressed to. Returns nil for room-wide messages (no prefix, no @mentions).
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
	if m := prefixRe.FindStringSubmatch(body); m != nil {
		for _, part := range separatorRe.Split(m[1], -1) {
			if p := strings.TrimSpace(part); p != "" {
				add(p)
			}
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
//
// should_respond logic mirrors the MCP etiquette:
//   - system messages: never respond.
//   - human sender, room-wide (no addressed_to): respond.
//   - human sender, addressed to others: do not respond.
//   - ai sender or unknown: respond only if addressed_to_me or in a DM room.
func annotate(msgs []types.Message, agentName string) []annotatedMessage {
	out := make([]annotatedMessage, len(msgs))
	for i, m := range msgs {
		addressed := parseAddressedTo(m.Body)
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

// agentShortName extracts the name portion from an agent ID.
// "worker@aimebu" → "worker"; bare "martin" → "martin".
func agentShortName(agentID string) string {
	if i := strings.IndexByte(agentID, '@'); i > 0 {
		return agentID[:i]
	}
	return agentID
}
