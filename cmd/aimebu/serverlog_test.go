package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStampedWriterPutsTheTimeAtTheStartOfEveryLine(t *testing.T) {
	var out bytes.Buffer
	now := time.Date(2026, 10, 22, 7, 8, 9, 0, time.UTC)
	stamped := &stampedWriter{w: &out, now: func() time.Time { return now }}
	fmt.Fprintln(stamped, "first")
	fmt.Fprint(stamped, "second, ")
	now = now.Add(time.Minute)
	fmt.Fprint(stamped, "still second\nthird\n")
	want := "2026-10-22T07:08:09Z first\n2026-10-22T07:08:09Z second, still second\n2026-10-22T07:09:09Z third\n"
	if out.String() != want {
		t.Fatalf("stamped = %q\nwant      %q", out.String(), want)
	}
}

// What the caller wrote is what is reported back, not the longer text with the timestamps added: io.MultiWriter treats a different count
// as a short write.
func TestStampedWriterReportsTheLengthItWasGiven(t *testing.T) {
	var out bytes.Buffer
	stamped := &stampedWriter{w: &out, now: time.Now}
	for _, text := range []string{"four\n", "no newline", "two\nlines\n", ""} {
		if n, err := stamped.Write([]byte(text)); n != len(text) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", text, n, err)
		}
	}
	if _, err := fmt.Fprintln(io.MultiWriter(io.Discard, stamped), "through a multiwriter"); err != nil {
		t.Fatalf("a multiwriter took the count for a short write: %v", err)
	}
}

func TestAttachServerLogTeesOutputAndTheStandardLogger(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	before := log.Writer()
	logs := attachServerLog(root, &stdout, &stderr)
	fmt.Fprintln(logs.stdout, "to stdout")
	fmt.Fprintln(logs.stderr, "to stderr")
	log.Print("from the standard logger")
	logs.close()
	if log.Writer() != before {
		t.Fatal("the standard logger was not restored")
	}
	if stdout.String() != "to stdout\n" || stderr.String() != "to stderr\n" {
		t.Fatalf("the original destinations changed: %q %q", stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(serverLogPath(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`Z to stdout`, `Z to stderr`, `from the standard logger`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the log lacks %q:\n%s", want, data)
		}
	}
	if !regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ to stdout$`).Match(data) {
		t.Errorf("printed lines are not stamped:\n%s", data)
	}
	if info, err := os.Stat(serverLogPath(root)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("log file mode: %v %v", info, err)
	}
	// After detach nothing more reaches the file.
	before2, _ := os.ReadFile(serverLogPath(root))
	log.Print("after detach")
	after, _ := os.ReadFile(serverLogPath(root))
	if !bytes.Equal(before2, after) {
		t.Fatal("the log was written after detach")
	}
}

func TestAttachServerLogRunsWithoutALogFileItCannotMake(t *testing.T) {
	parent := t.TempDir()
	// The config root is a file, so server/ cannot be created under it.
	root := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(root, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	logs := attachServerLog(root, &stdout, &stderr)
	defer logs.close()
	logs.enableRotation()
	fmt.Fprintln(logs.stdout, "still printed")
	fmt.Fprintln(logs.stderr, "still reported")
	if !strings.Contains(stderr.String(), "warning: no log file") || !strings.Contains(stdout.String(), "still printed") || !strings.Contains(stderr.String(), "still reported") {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestServerWritesItsLogAndKeepsItAcrossPrune(t *testing.T) {
	t.Setenv("AIMEBU_USAGES", "0")
	root := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "correct horse battery staple")
	t.Setenv("AIMEBU_EMBEDDED_SUPERVISOR", "0")
	port := holdPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	ready := watchServing(t)
	go func() { done <- serverCommand(ctx, &stdout, &stderr) }()
	waitServerListening(t, fmt.Sprintf("http://127.0.0.1:%d/api/me", port), ready, done, &stderr)
	cancel()
	if code := receive(t, "the server stopping", done, serverStopLimit); code != 0 {
		t.Fatalf("serve code=%d stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(serverLogPath(root))
	if err != nil {
		t.Fatalf("the server wrote no log: %v", err)
	}
	if !strings.Contains(string(data), "Owner initialized") {
		t.Fatalf("the log does not carry what the server printed:\n%s", data)
	}
	// The log is a diagnostic, not data: prune -a keeps it and its backups.
	writePruneFixture(t, serverLogPath(root)+".1", "older log")
	var out, errOut bytes.Buffer
	if got := pruneCommand(context.Background(), []string{"-a", "-y"}, strings.NewReader(""), &out, &errOut); got != 0 {
		t.Fatalf("prune -a = %d, stderr=%q", got, errOut.String())
	}
	for _, path := range []string{serverLogPath(root), serverLogPath(root) + ".1"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("prune -a removed %s: %v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "server", "aimebu.sqlite")); !os.IsNotExist(err) {
		t.Errorf("prune -a kept the database: %v", err)
	}
}

// A server that is about to lose the race to run (another already holds the writer lock) adds its first lines to the log but must not
// rotate the file the winner is writing to: rotation starts only once the server holds the lock.
func TestServerLogRotatesOnlyOnceTheServerOwnsIt(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(serverLogPath(root)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverLogPath(root), bytes.Repeat([]byte("o"), 11<<20), 0600); err != nil { // past the 10 MiB cap
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	logs := attachServerLog(root, &stdout, &stderr)
	defer logs.close()
	fmt.Fprintln(logs.stderr, "a start that loses the race says why")
	if _, err := os.Stat(serverLogPath(root) + ".1"); !os.IsNotExist(err) {
		t.Fatalf("the log was rotated before the server owned it: %v", err)
	}
	logs.enableRotation()
	if info, err := os.Stat(serverLogPath(root) + ".1"); err != nil || info.Size() < 11<<20 {
		t.Fatalf("an oversized log was not rotated once the server owned it: %v %v", info, err)
	}
}

// The one-time setup token and link are for the person at the terminal: they are on the terminal and in the token file, never in the log.
func TestSetupTokenIsNotWrittenToTheLog(t *testing.T) {
	t.Setenv("AIMEBU_USAGES", "0")
	t.Setenv("AIMEBU_EMBEDDED_SUPERVISOR", "0")
	t.Setenv("AIMEBU_OWNER_LOGIN", "")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "")
	root := t.TempDir()
	useMigratedConfigDir(t, root)
	port := holdPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout, stderr syncBuffer
	ready := watchServing(t)
	go func() { done <- serverCommand(ctx, &stdout, &stderr) }()
	waitServerListening(t, fmt.Sprintf("http://127.0.0.1:%d/api/me", port), ready, done, &bytes.Buffer{})
	token, err := os.ReadFile(filepath.Join(root, setupTokenFile))
	if err != nil || strings.TrimSpace(string(token)) == "" {
		t.Fatalf("no setup token file: %v", err)
	}
	cancel()
	if code := receive(t, "the server stopping", done, serverStopLimit); code != 0 {
		t.Fatalf("serve code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), strings.TrimSpace(string(token))) {
		t.Fatalf("the terminal was not given the token:\n%s", stderr.String())
	}
	logged, err := os.ReadFile(serverLogPath(root))
	if err != nil || len(logged) == 0 {
		t.Fatalf("no log: %v", err)
	}
	if strings.Contains(string(logged), strings.TrimSpace(string(token))) || strings.Contains(string(logged), "setup token") {
		t.Fatalf("the setup token or its instructions are in the log:\n%s", logged)
	}
}

// syncBuffer is a bytes.Buffer safe to read while the server writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
