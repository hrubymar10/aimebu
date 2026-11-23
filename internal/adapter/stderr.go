package adapter

import (
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

const stderrLimit = 4096

// aimebu's own credentials (agent key aak_, machine key amk_, session token as_), the API keys and tokens of providers and hosts (sk-ant-..., sk-...,
// ghp_..., github_pat_..., Slack xox..., AWS AKIA...). The short prefixes (as_, sk-, ghp_) need a run of key characters after them, and a
// non-word character (or the start) before, so an identifier such as has_value or task-list-item-for-the-test is not one.
var agentKeyPattern = regexp.MustCompile(`aak_[A-Za-z0-9_-]+|amk_[A-Za-z0-9_-]+`)
var prefixedKeyPattern = regexp.MustCompile(`(^|[^A-Za-z0-9])(as_[A-Za-z0-9_-]{16,}|sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16})`)
var bearerPattern = regexp.MustCompile(`(?i)Bearer[[:space:]]+[^[:space:]"']+`)

// An Authorization header with the Basic scheme (a base64 user:password) keeps its scheme word and loses the credential.
var basicPattern = regexp.MustCompile(`(?i)(authorization["']?[[:space:]]*[:=][[:space:]]*["']?)basic[[:space:]]+[A-Za-z0-9+/=_-]+`)

// A header or setting that carries a secret as its value, whatever the secret looks like: a cookie or set-cookie header, and a name that says
// what it holds (x-api-key; anything ending in api_key, token, secret or password, which takes in ANTHROPIC_API_KEY, GH_TOKEN, refresh_token,
// client_secret; a bare token) followed by : or = (in JSON too). The name stays so the output still says what was there; the value, to the end
// of the line for a cookie (it holds several pairs) and to the next space or quote otherwise, goes.
var cookieHeaderPattern = regexp.MustCompile(`(?i)\b((?:set-)?cookie)[[:space:]]*[:=][[:space:]]*[^\r\n]+`)
var secretValuePattern = regexp.MustCompile(`(?i)(x-api-key|[a-z0-9_-]*api[_-]?key|[a-z0-9_-]*token|[a-z0-9_-]*secret|[a-z0-9_-]*passw(?:or)?d)(["']?[[:space:]]*[:=][[:space:]]*)["']?[^[:space:]"',;]+`)

// A private key in PEM armour (RSA, EC, OpenSSH, PKCS#8, a PGP private key block): from its BEGIN line through the matching END line,
// across the lines between them, whether those are real line breaks or the \n of a JSON string. One whose END is not in the text (the
// text was cut) goes to the end of the text. privateKeyTailPattern is the other cut: the END line and the base64 lines before it, when
// the BEGIN line is not in the text.
var privateKeyPattern = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)
var privateKeyTailPattern = regexp.MustCompile(`(?:[A-Za-z0-9+/=]{16,}(?:\\[nr]|[[:space:]])+)*-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
var privateKeyOpenPattern = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
var privateKeyClosePattern = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)

const privateKeyMark = "[redacted private key]"

// OpensPrivateKey says whether text begins a PEM private key that it does not end: a reader that redacts a stream line by line drops the
// lines that follow, up to the one ClosesPrivateKey names.
func OpensPrivateKey(text string) bool {
	open := privateKeyOpenPattern.FindAllStringIndex(text, -1)
	return len(open) > 0 && !privateKeyClosePattern.MatchString(text[open[len(open)-1][1]:])
}

// ClosesPrivateKey says whether text holds the END line of a PEM private key.
func ClosesPrivateKey(text string) bool { return privateKeyClosePattern.MatchString(text) }

// StderrTail keeps a bounded launcher error tail without retaining full output.
type StderrTail struct {
	mu   sync.Mutex
	data []byte
}

func (t *StderrTail) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(data)
	if n >= stderrLimit {
		t.data = append(t.data[:0], data[n-stderrLimit:]...)
	} else {
		excess := len(t.data) + n - stderrLimit
		if excess > 0 {
			t.data = append(t.data[:0], t.data[excess:]...)
		}
		t.data = append(t.data, data...)
	}
	return n, nil
}

func (t *StderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return SanitizeStderr(string(t.data))
}

// RedactSecrets removes what a harness's output must never carry into storage: aimebu's keys and tokens, provider and host API keys and tokens
// by their prefixes, PEM private keys, bearer and basic credentials, cookies, and the value of anything named like a key, token, secret or password. It is a list
// of known shapes, not a promise: a secret with a shape not listed here and a name that does not say what it is is not recognised. It does not bound the
// length; SanitizeStderr (the tail kept as exit evidence) and the per-agent output log both start from it.
func RedactSecrets(raw string) string {
	raw = strings.ToValidUTF8(raw, "�")
	raw = privateKeyPattern.ReplaceAllString(raw, privateKeyMark)
	raw = privateKeyTailPattern.ReplaceAllString(raw, privateKeyMark)
	raw = agentKeyPattern.ReplaceAllString(raw, "[redacted]")
	raw = prefixedKeyPattern.ReplaceAllString(raw, "${1}[redacted]")
	raw = bearerPattern.ReplaceAllString(raw, "Bearer [redacted]")
	raw = basicPattern.ReplaceAllString(raw, "${1}Basic [redacted]")
	raw = cookieHeaderPattern.ReplaceAllString(raw, "$1: [redacted]")
	return secretValuePattern.ReplaceAllString(raw, "$1$2[redacted]")
}

// JoinWriters is one writer that writes to every non-nil writer given, or nil when there is none. A failing writer never fails the
// others or the copy that feeds it: the harness's stderr pipe is read for the tail kept as exit evidence, and a capture that cannot
// write must not stop that read (a harness blocks once its stderr pipe is full).
func JoinWriters(writers ...io.Writer) io.Writer {
	var kept []io.Writer
	for _, writer := range writers {
		if writer != nil {
			kept = append(kept, tolerant{writer})
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return io.MultiWriter(kept...)
}

type tolerant struct{ w io.Writer }

func (t tolerant) Write(p []byte) (int, error) {
	_, _ = t.w.Write(p)
	return len(p), nil
}

// SanitizeStderr bounds evidence and removes agent credentials before storage.
func SanitizeStderr(raw string) string {
	if len(raw) > stderrLimit {
		raw = raw[len(raw)-stderrLimit:]
	}
	raw = RedactSecrets(raw)
	raw = strings.TrimSpace(raw)
	for len(raw) > stderrLimit {
		_, size := utf8.DecodeRuneInString(raw)
		raw = raw[size:]
	}
	return raw
}
