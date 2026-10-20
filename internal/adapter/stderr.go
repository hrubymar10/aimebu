package adapter

import (
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

const stderrLimit = 4096

var agentKeyPattern = regexp.MustCompile(`aak_[A-Za-z0-9_-]+`)
var bearerPattern = regexp.MustCompile(`(?i)Bearer[[:space:]]+[^[:space:]"']+`)

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

// SanitizeStderr bounds evidence and removes agent credentials before storage.
func SanitizeStderr(raw string) string {
	if len(raw) > stderrLimit {
		raw = raw[len(raw)-stderrLimit:]
	}
	raw = strings.ToValidUTF8(raw, "�")
	raw = agentKeyPattern.ReplaceAllString(raw, "[redacted]")
	raw = bearerPattern.ReplaceAllString(raw, "Bearer [redacted]")
	raw = strings.TrimSpace(raw)
	for len(raw) > stderrLimit {
		_, size := utf8.DecodeRuneInString(raw)
		raw = raw[size:]
	}
	return raw
}
