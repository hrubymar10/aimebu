package server

import (
	"regexp"
	"strings"
)

var (
	modelBracketSuffixRE = regexp.MustCompile(`\s*\[[^\]]+\]\s*$`)
	modelDateSuffixRE    = regexp.MustCompile(`[-_](?:\d{8}|\d{4}-\d{2}-\d{2})$`)
	claudeModelIDRE      = regexp.MustCompile(`^claude-([a-z]+)-([0-9]+)-([0-9]+)$`)
)

// canonicalModelSlug folds structurally known provider-specific model IDs
// into the short slugs used for leaderboard grouping. Unknown and unmapped
// values are kept honest instead of guessed.
func canonicalModelSlug(raw, harness string) string {
	model := strings.TrimSpace(strings.ToLower(raw))
	if model == "" {
		return "unknown"
	}
	model = strings.TrimSpace(modelBracketSuffixRE.ReplaceAllString(model, ""))
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = strings.TrimSpace(model[slash+1:])
	}
	model = modelDateSuffixRE.ReplaceAllString(model, "")
	if model == "" {
		return "unknown"
	}
	if model == "unknown" {
		return model
	}
	if matches := claudeModelIDRE.FindStringSubmatch(model); matches != nil {
		return matches[1] + matches[2] + "." + matches[3]
	}
	return model
}
