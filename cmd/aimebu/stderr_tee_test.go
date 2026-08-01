package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTimestampedStderrTeeWritesTerminalAndFile(t *testing.T) {
	var terminal bytes.Buffer
	path := filepath.Join(t.TempDir(), "stderr.log")
	tee := newTimestampedStderrTee(&terminal, &terminal, path, 0o600)
	tee.now = func() time.Time {
		return time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)
	}

	if _, err := tee.Write([]byte("first line\nsecond line\n")); err != nil {
		t.Fatal(err)
	}
	if err := tee.close(); err != nil {
		t.Fatal(err)
	}
	if got := terminal.String(); got != "first line\nsecond line\n" {
		t.Fatalf("terminal = %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-08-01T12:30:00Z first line\n2026-08-01T12:30:00Z second line\n"
	if got := string(data); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestTimestampedStderrTeeOpenFailureFallsBackOnce(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(parent, []byte("block mkdir"), 0o600); err != nil {
		t.Fatal(err)
	}
	var terminal bytes.Buffer
	tee := newTimestampedStderrTee(&terminal, &terminal, filepath.Join(parent, "stderr.log"), 0o600)

	for _, line := range []string{"one\n", "two\n"} {
		if _, err := tee.Write([]byte(line)); err != nil {
			t.Fatalf("terminal-only write failed: %v", err)
		}
	}
	got := terminal.String()
	if strings.Count(got, "stderr file logging disabled") != 1 {
		t.Fatalf("warning count = %d, output %q", strings.Count(got, "stderr file logging disabled"), got)
	}
	if !strings.HasSuffix(got, "one\ntwo\n") {
		t.Fatalf("terminal did not preserve writes: %q", got)
	}
}

func TestTimestampedStderrTeeWriteFailureFallsBackOnce(t *testing.T) {
	var terminal bytes.Buffer
	tee := newTimestampedStderrTee(&terminal, &terminal, filepath.Join(t.TempDir(), "stderr.log"), 0o600)
	if err := tee.file.Close(); err != nil {
		t.Fatal(err)
	}

	for _, line := range []string{"one\n", "two\n"} {
		if _, err := tee.Write([]byte(line)); err != nil {
			t.Fatalf("terminal-only write failed: %v", err)
		}
	}
	got := terminal.String()
	if strings.Count(got, "stderr file logging disabled") != 1 {
		t.Fatalf("warning count = %d, output %q", strings.Count(got, "stderr file logging disabled"), got)
	}
	if !strings.HasPrefix(got, "one\n") || !strings.HasSuffix(got, "two\n") {
		t.Fatalf("terminal did not preserve writes: %q", got)
	}
}

func TestTimestampedStderrTeeConcurrentLinesDoNotInterleave(t *testing.T) {
	var terminal lockedBuffer
	path := filepath.Join(t.TempDir(), "stderr.log")
	tee := newTimestampedStderrTee(&terminal, &terminal, path, 0o600)

	const writers = 100
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = fmt.Fprintf(tee, "writer-%03d\n", i)
		}(i)
	}
	wg.Wait()
	if err := tee.close(); err != nil {
		t.Fatal(err)
	}

	terminalLines := strings.Split(strings.TrimSpace(terminal.String()), "\n")
	if len(terminalLines) != writers {
		t.Fatalf("terminal lines = %d, want %d", len(terminalLines), writers)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fileLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(fileLines) != writers {
		t.Fatalf("file lines = %d, want %d", len(fileLines), writers)
	}
	for _, line := range fileLines {
		if strings.Count(line, "writer-") != 1 {
			t.Fatalf("interleaved file line %q", line)
		}
	}
}

func TestAgentLogRenameKeepsDebugAndStderrTogether(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	t.Setenv("AIMEBU_AGENT_DEBUG", "1")
	spawnTag := "feedbeefcafebabe"
	var terminal bytes.Buffer
	stderrLog := newAgentStderrLog("", spawnTag, &terminal)
	debug := newAgentDebugLog("", spawnTag)
	debug.stderr = stderrLog

	debug.log("wrapper_start", nil)
	_, _ = fmt.Fprintln(stderrLog.tee, "before registration")
	if err := debug.setAgentName("worker@aimebu"); err != nil {
		t.Fatal(err)
	}
	debug.log("register_observed", nil)
	_, _ = fmt.Fprintln(stderrLog.tee, "after registration")
	if err := debug.close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrLog.close(); err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{".log", ".stderr.log"} {
		pre := agentLogPath("", spawnTag, suffix)
		final := agentLogPath("worker@aimebu", spawnTag, suffix)
		if _, err := os.Stat(pre); !os.IsNotExist(err) {
			t.Fatalf("pre-register %s remains: %v", suffix, err)
		}
		if _, err := os.Stat(final); err != nil {
			t.Fatalf("final %s missing: %v", suffix, err)
		}
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
