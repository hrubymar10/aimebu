package server

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeClock moves only when the handover waits on it: each wait advances it by what was asked, after a moment of real time, so that a
// child process the test signalled has time to be reaped.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	time.Sleep(time.Millisecond)
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	fired := make(chan time.Time, 1)
	fired <- now
	return fired
}

func (c *fakeClock) elapsed(since time.Time) time.Duration { return c.Now().Sub(since) }

// stubbornChild is the test's own stand-in for an old supervisor: a child process that ignores SIGTERM (and so any exit request). Only
// this child is ever signalled by the tests.
type stubbornChild struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	exited bool
	state  chan *exec.Cmd
}

func startStubbornChild(t *testing.T, script string) *stubbornChild {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &stubbornChild{cmd: cmd, state: make(chan *exec.Cmd, 1)}
	go func() {
		_ = cmd.Wait()
		child.mu.Lock()
		child.exited = true
		child.mu.Unlock()
		child.state <- cmd
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		child.mu.Lock()
		done := child.exited
		child.mu.Unlock()
		if !done {
			<-child.state
		}
	})
	// The shell replaces itself with sleep after installing the trap: wait until it has, so that its start time is the one read below.
	time.Sleep(100 * time.Millisecond)
	return child
}

func (c *stubbornChild) holds(pid int) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return pid == c.cmd.Process.Pid && !c.exited, nil
}

func testProcessStart(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type handoverLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *handoverLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *handoverLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// An old supervisor that ignores the exit request and SIGTERM is waited for 30 seconds, sent SIGTERM, waited for 15 more and killed, on
// the handover's clock, and every step is in the log.
func TestHandoverSignalsAndKillsASupervisorThatIgnoresTheExitRequest(t *testing.T) {
	t.Parallel()
	child := startStubbornChild(t, `trap "" TERM; exec sleep 600`)
	pid := child.cmd.Process.Pid
	start := testProcessStart(pid)
	if start == "" {
		t.Fatal("the child's start time cannot be read")
	}
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	began := clock.Now()
	var logs handoverLog
	var mu sync.Mutex
	var steps []string
	h := SupervisorHandover{Clock: clock, Holds: child.holds, StartOf: testProcessStart, Logf: logs.logf,
		Signal: func(p int, sig syscall.Signal) error {
			mu.Lock()
			steps = append(steps, fmt.Sprintf("%s@%s", sig, clock.elapsed(began)))
			mu.Unlock()
			return syscall.Kill(p, sig)
		},
		Kill: func(p int) error {
			mu.Lock()
			steps = append(steps, fmt.Sprintf("kill@%s", clock.elapsed(began)))
			mu.Unlock()
			return syscall.Kill(p, syscall.SIGKILL)
		}}
	if outcome := h.Run(t.Context(), pid, start); outcome != HandoverKilled {
		t.Fatalf("outcome %q; log:\n%s", outcome, logs.text())
	}
	mu.Lock()
	got := strings.Join(steps, " ")
	mu.Unlock()
	if want := fmt.Sprintf("%s@30s kill@45s", syscall.SIGTERM); got != want {
		t.Fatalf("steps %q, want %q", got, want)
	}
	select {
	case cmd := <-child.state:
		child.state <- cmd
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("the child did not end on SIGKILL: %v", cmd.ProcessState)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the killed child did not exit")
	}
	text := logs.text()
	for _, line := range []string{
		fmt.Sprintf("supervisor swap: waiting for supervisor pid %d to exit (30s)", pid),
		fmt.Sprintf("supervisor swap: supervisor pid %d did not exit within 30s; sent SIGTERM, waiting 15s", pid),
		fmt.Sprintf("supervisor swap: supervisor pid %d did not exit 15s after SIGTERM; sending SIGKILL to it and its child processes", pid),
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("the log lacks %q:\n%s", line, text)
		}
	}
}

// A supervisor that goes on SIGTERM is not killed.
func TestHandoverStopsAtSIGTERMWhenTheSupervisorGoes(t *testing.T) {
	t.Parallel()
	child := startStubbornChild(t, `exec sleep 600`)
	pid := child.cmd.Process.Pid
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	var logs handoverLog
	killed := false
	h := SupervisorHandover{Clock: clock, Holds: child.holds, StartOf: testProcessStart, Logf: logs.logf, TermWait: time.Hour,
		Signal: func(p int, sig syscall.Signal) error { return syscall.Kill(p, sig) },
		Kill:   func(int) error { killed = true; return nil }}
	if outcome := h.Run(t.Context(), pid, testProcessStart(pid)); outcome != HandoverExited || killed {
		t.Fatalf("outcome %q killed %t; log:\n%s", outcome, killed, logs.text())
	}
	if !strings.Contains(logs.text(), fmt.Sprintf("old supervisor pid %d exited", pid)) {
		t.Fatalf("the exit is not logged:\n%s", logs.text())
	}
}

// A supervisor that exits by itself after the request is never signalled.
func TestHandoverLeavesASupervisorThatExitsByItself(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	polls := 0
	h := SupervisorHandover{Clock: clock, Logf: (&handoverLog{}).logf,
		Holds:   func(int) (bool, error) { polls++; return polls < 5, nil },
		StartOf: func(int) string { return "start" },
		Signal:  func(int, syscall.Signal) error { t.Error("signalled"); return nil },
		Kill:    func(int) error { t.Error("killed"); return nil }}
	if outcome := h.Run(t.Context(), 4242, "start"); outcome != HandoverExited {
		t.Fatalf("outcome %q", outcome)
	}
}

// A pid the system has given to another process is never signalled: not when its start time differs from the one the exit request was
// sent with, not when that start time was never known, and not when it can no longer be read. The test's own child stands in for the
// other process and must survive.
func TestHandoverNeverSignalsAReusedPid(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]struct {
		requested string // the start time the exit request was sent with
		now       func(int) string
		want      HandoverOutcome
	}{
		"its start time changed":     {"Thu Jan  1 00:00:00 2026", testProcessStart, HandoverExited},
		"its start time was unknown": {"", testProcessStart, HandoverUnverified},
		"it cannot be read":          {"Thu Jan  1 00:00:00 2026", func(int) string { return "" }, HandoverUnverified},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			child := startStubbornChild(t, `exec sleep 600`)
			pid := child.cmd.Process.Pid
			clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
			var logs handoverLog
			h := SupervisorHandover{Clock: clock, Holds: child.holds, StartOf: item.now, Logf: logs.logf,
				Signal: func(p int, sig syscall.Signal) error { t.Errorf("pid %d sent %s", p, sig); return nil },
				Kill:   func(p int) error { t.Errorf("pid %d killed", p); return nil }}
			if outcome := h.Run(context.Background(), pid, item.requested); outcome != item.want {
				t.Fatalf("outcome %q, want %q; log:\n%s", outcome, item.want, logs.text())
			}
			if held, _ := child.holds(pid); !held || syscall.Kill(pid, 0) != nil {
				t.Fatal("the other process does not run any more")
			}
		})
	}
}
