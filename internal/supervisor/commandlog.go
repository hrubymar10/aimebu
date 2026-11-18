package supervisor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// commandLog is one job's output log: what the command printed, redacted line by line before it is written, capped, with the end kept for
// the wake note. It lives in the agent's own checkout (.aimebu/commands, git-excluded), so the path the note quotes is one the agent can
// open from its own Bash, native or in a container.
type commandLog struct {
	mu        sync.Mutex
	file      *os.File
	written   int64
	truncated bool
	line      []byte
	tail      []byte
}

func openCommandLog(path string) (*commandLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	return &commandLog{file: file}, nil
}

// Write takes the process's output as it comes, and keeps it whole only in the tail: a line is redacted before it reaches the file.
func (l *commandLog) Write(p []byte) (int, error) {
	total := len(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(p) > 0 {
		if index := bytes.IndexByte(p, '\n'); index >= 0 {
			l.line = append(l.line, p[:index+1]...)
			p = p[index+1:]
			l.flushLine()
			continue
		}
		l.line = append(l.line, p...)
		p = nil
		if len(l.line) > 256<<10 {
			// A line with no newline in it is written in pieces so it cannot hold the writer hostage.
			l.flushLine()
		}
	}
	return total, nil
}

func (l *commandLog) flushLine() {
	if len(l.line) == 0 {
		return
	}
	clean := string(l.line)
	if needsRedaction(clean) {
		clean = adapter.RedactSecrets(clean)
	}
	l.line = l.line[:0]
	l.keepTail(clean)
	if l.written >= commandLogMaxBytes {
		l.truncated = true
		return
	}
	chunk := []byte(clean)
	if rest := commandLogMaxBytes - l.written; int64(len(chunk)) > rest {
		chunk = chunk[:rest]
		l.truncated = true
	}
	if n, err := l.file.Write(chunk); err == nil {
		l.written += int64(n)
	}
}

func (l *commandLog) keepTail(line string) {
	l.tail = append(l.tail, line...)
	if len(l.tail) > commandTailBytes {
		l.tail = l.tail[len(l.tail)-commandTailBytes:]
	}
}

// redactionMarkers are what every pattern RedactSecrets applies needs somewhere in the text: a name that says what the value is, a scheme
// word, or a key prefix. A line with none of them cannot hold a recognised secret, and is written as it came (the scan is not free).
var redactionMarkers = []string{"bearer", "basic", "cookie", "key", "token", "secret", "passw", "aak", "amk", "as_", "sk-", "ghp", "gho", "ghu", "ghs", "ghr", "github_pat", "xox", "akia"}

func needsRedaction(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range redactionMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// CloseTail flushes what is left of a partial line, closes the file and returns the tail the wake note shows.
func (l *commandLog) CloseTail() (tail string, written int64, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLine()
	_ = l.file.Close()
	return strings.ToValidUTF8(string(l.tail), "�"), l.written, l.truncated
}

// tailFile reads the end of one job's log, bounded: what a wake note shows for a job this supervisor found running after a restart, and
// what a command_log read returns. It returns the text, the file's size, and whether the read was cut.
func tailFile(path string, maxBytes int) (string, int64, bool, error) {
	if maxBytes < 1 {
		maxBytes = commandTailBytes
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", 0, false, err
	}
	size := info.Size()
	truncated := false
	if size > int64(maxBytes) {
		truncated = true
		if _, err := file.Seek(size-int64(maxBytes), 0); err != nil {
			return "", size, truncated, err
		}
	}
	data := make([]byte, 0, maxBytes)
	buffer := make([]byte, 32<<10)
	for len(data) < maxBytes {
		n, err := file.Read(buffer)
		if n > 0 {
			room := maxBytes - len(data)
			if n > room {
				n = room
			}
			data = append(data, buffer[:n]...)
		}
		if err != nil {
			break
		}
	}
	// Cut at the first whole line of the read window, so the note never shows half a line as its first one.
	if truncated {
		if index := bytes.IndexByte(data, '\n'); index >= 0 && index+1 < len(data) {
			data = data[index+1:]
		}
	}
	text := strings.ToValidUTF8(string(data), "�")
	text = strings.TrimRight(text, "\n")
	return text, size, truncated, nil
}
