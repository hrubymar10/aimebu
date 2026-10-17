package usages

import (
	"os"
	"regexp"
	"strings"
)

var (
	// A JWT: id and access tokens are three dot-separated base64url parts.
	redactJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)
	// Provider key shapes (Anthropic keys, OpenAI-style sk- keys).
	redactKey = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`)
	// An Authorization header value.
	redactBearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	// A token-bearing field in JSON, a query string or a key=value line. The
	// separator allows the quotes and backslashes of JSON that is itself
	// embedded in a JSON string; the value runs to the next delimiter.
	redactField = regexp.MustCompile(`(?i)((?:access|refresh|id|session)[_-]?token|api[_-]?key|client[_-]?secret|password)(\\*["']?\s*[:=]\s*\\*["']?)([^\s"'\\,}&;]+)`)
	// Any other long opaque run (32+, with a digit): base64url, hex digests, cookie values.
	redactLong = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
)

// redactDetail makes a free-form error or CLI output fragment safe to persist,
// log, broadcast or post to a room: it strips anything that looks like a token
// or key and shortens the home directory to "~". CLI output is untrusted and
// there is no proof a failing CLI never echoes a credential, so every sink that
// is not the owner's own terminal passes its text through here.
func redactDetail(s string) string {
	return redactDetailHome(s, homeForRedaction())
}

// RedactDetail is redactDetail for callers outside the package.
func RedactDetail(s string) string { return redactDetail(s) }

func homeForRedaction() string {
	home, err := os.UserHomeDir()
	if err != nil || len(home) < 2 {
		return ""
	}
	return strings.TrimRight(home, `/\`)
}

func redactDetailHome(s, home string) string {
	if home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	s = redactJWT.ReplaceAllString(s, redacted)
	s = redactKey.ReplaceAllString(s, redacted)
	s = redactBearer.ReplaceAllString(s, "${1} "+redacted)
	s = redactField.ReplaceAllString(s, "${1}${2}"+redacted)
	return redactLong.ReplaceAllStringFunc(s, func(run string) string {
		// A long run of letters only (a path segment, a word) is not a secret;
		// tokens, digests and cookies mix in digits.
		if strings.ContainsAny(run, "0123456789") {
			return redacted
		}
		return run
	})
}
