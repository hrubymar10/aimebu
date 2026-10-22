package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// lockedBuffer is a bytes.Buffer safe to write from the adapter's goroutine and read from the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type brokenSink struct{}

func (brokenSink) Write([]byte) (int, error) { return 0, errors.New("sink is gone") }

// What the harness prints to stderr reaches the turn's sink as it is written (the per-agent output log), raw: redaction belongs to the
// log. The tail kept as exit evidence is redacted as before, and a sink that fails changes nothing.
func TestStderrSinkReceivesWhatTheHarnessPrints(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	script := "#!/bin/sh\necho 'first warning' >&2\necho 'Bearer aak_secret: second' >&2\nexit 17\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for name, sink := range map[string]io.Writer{"a sink": &lockedBuffer{}, "a broken sink": brokenSink{}} {
		a := &Adapter{Command: path, AllowNative: true}
		turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "sink", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeCorrection, StderrSink: sink})
		if err != nil {
			t.Fatal(err)
		}
		for range turn.Events() {
		}
		exit, err := turn.Wait()
		if err != nil || !strings.Contains(exit.StderrTail, "first warning") || !strings.Contains(exit.StderrTail, "second") || strings.Contains(exit.StderrTail, "aak_secret") {
			t.Fatalf("%s: exit=%+v err=%v", name, exit, err)
		}
		if buffer, ok := sink.(*lockedBuffer); ok && (!strings.Contains(buffer.String(), "first warning") || !strings.Contains(buffer.String(), "second")) {
			t.Fatalf("the sink saw %q", buffer.String())
		}
	}
}
