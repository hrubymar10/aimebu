package store

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// legacyPrefixRe detects an IRC-style "name:" speaker prefix at the start
	// of a body.
	legacyPrefixRe = regexp.MustCompile(`(?i)^([a-z][a-z0-9]{2,11})\s*:`)
	// inlinePrefixRe detects inline "name1, name2 —" or "name1 and name2:" at
	// the start of a sentence or paragraph.
	inlinePrefixRe = regexp.MustCompile(`(?i)(?:^|[.!?\n]\s*)([a-z][a-z0-9]{2,11})\s*(?:,\s*|\s+and\s+)([a-z][a-z0-9]{2,11})\s*(?:[—–\-]|:)`)
)

// parseLegacyPrefix returns the lowercase name when body starts with
// "name:" and name is a known member slug. Ordinary labels such as "Note:" or
// "URL:", code, and escaped text do not match.
func parseLegacyPrefix(body string, known map[string]bool) (string, bool) {
	m := legacyPrefixRe.FindStringSubmatch(liveProse(body))
	if m == nil {
		return "", false
	}
	name := strings.ToLower(m[1])
	return name, known[name]
}

// parseInlineLegacyPrefix returns the names of "a, b —" / "a and b:" style
// addressing, only when both tokens are known member slugs.
func parseInlineLegacyPrefix(body string, known map[string]bool) ([]string, bool) {
	m := inlinePrefixRe.FindStringSubmatch(liveProse(body))
	if m == nil {
		return nil, false
	}
	first, second := strings.ToLower(m[1]), strings.ToLower(m[2])
	if !known[first] || !known[second] {
		return nil, false
	}
	if first == second {
		return []string{first}, true
	}
	return []string{first, second}, true
}

// legacyPrefixWarning explains why an IRC-style prefix did not address anyone.
// It returns "" when body uses neither form.
func legacyPrefixWarning(body string, known map[string]bool) string {
	if name, ok := parseLegacyPrefix(body, known); ok {
		return fmt.Sprintf("'%s:' prefix detected — this does not address %s; write '@%s ...'. Also drop self-labels: the author field already identifies you.", name, name, name)
	}
	if names, ok := parseInlineLegacyPrefix(body, known); ok {
		return fmt.Sprintf("'%s, %s —' inline addressing detected — write '@%s @%s ...' so the message addresses them.", names[0], names[len(names)-1], names[0], names[len(names)-1])
	}
	return ""
}
