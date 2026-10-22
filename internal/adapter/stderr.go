package adapter

import (
	"io"
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

// RedactSecrets removes what a harness's output must never carry into storage: agent keys and bearer tokens. It does not bound the
// length; SanitizeStderr (the tail kept as exit evidence) and the per-agent output log both start from it.
func RedactSecrets(raw string) string {
	raw = strings.ToValidUTF8(raw, "�")
	raw = agentKeyPattern.ReplaceAllString(raw, "[redacted]")
	return bearerPattern.ReplaceAllString(raw, "Bearer [redacted]")
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
