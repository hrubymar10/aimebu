package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

func warmOrphanStatusJournal(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "supervisor", "journal.sqlite")
	j, err := supervisor.OpenJournal(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	report := true
	if err := j.RecordWarmProcess(t.Context(), supervisor.WarmProcess{PID: 17, AgentID: "agent", Started: "old", SessionEndReport: &report}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.MarkWarmOrphan(t.Context(), 17, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return root, path
}

// A separate process owns the journal and publishes the same status snapshot
// as the running supervisor's orphan sweep.
func TestWarmOrphanStatusHolder(t *testing.T) {
	path := os.Getenv("AIMEBU_TEST_WARM_STATUS_PATH")
	if path == "" {
		return
	}
	j, err := supervisor.OpenJournal(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.PublishWarmOrphanStatus(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestSupervisorStatusShowsRunningWarmOrphanWithoutOpeningJournal(t *testing.T) {
	root, path := warmOrphanStatusJournal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWarmOrphanStatusHolder$")
	cmd.Env = append(os.Environ(), "AIMEBU_TEST_WARM_STATUS_PATH="+path)
	var childErr bytes.Buffer
	cmd.Stderr = &childErr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = io.WriteString(stdin, "done\n")
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("status holder: %v: %s", err, childErr.String())
		}
	}()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("status holder ready: %q, %v: %s", line, err, childErr.String())
	}
	original := openWarmOrphanJournal
	defer func() { openWarmOrphanJournal = original }()
	openWarmOrphanJournal = func(context.Context, string) (*supervisor.Journal, error) {
		t.Fatal("opened a journal held by another process")
		return nil, nil
	}
	var out bytes.Buffer
	printWarmOrphans(&out, root, time.Now())
	if !strings.Contains(out.String(), "warm orphan awaiting proof, age 2h") || !strings.Contains(out.String(), "agent agent") {
		t.Fatalf("running status: %q", out.String())
	}
}

func TestSupervisorStatusReadsFreeWarmOrphanUnderLock(t *testing.T) {
	root, path := warmOrphanStatusJournal(t)
	original := openWarmOrphanJournal
	defer func() { openWarmOrphanJournal = original }()
	called := 0
	openWarmOrphanJournal = func(ctx context.Context, gotPath string) (*supervisor.Journal, error) {
		called++
		if gotPath != path {
			t.Fatalf("journal path = %q", gotPath)
		}
		j, err := original(ctx, gotPath)
		if err != nil {
			return nil, err
		}
		if held, _, _, err := supervisor.JournalLockInfo(path); err != nil || !held {
			t.Fatalf("free-journal read lacks lock: held=%t err=%v", held, err)
		}
		return j, nil
	}
	var out bytes.Buffer
	printWarmOrphans(&out, root, time.Now())
	if called != 1 || !strings.Contains(out.String(), "warm orphan awaiting proof, age 2h") {
		t.Fatalf("free status: opens=%d output=%q", called, out.String())
	}
}
