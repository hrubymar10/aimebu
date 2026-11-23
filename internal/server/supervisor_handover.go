package server

import (
	"context"
	"log"
	"syscall"
	"time"
)

// The bounded handover. Once a local supervisor has been asked to exit (the drain is over, the owner chose Restart now, or the manager
// found it overdue), the server does not wait for it without limit, whatever build it is: it gives it HandoverExitWait to go by itself,
// then sends SIGTERM, and after HandoverTermWait more it kills it (with what it started, see the manager's kill). Every signal goes to the
// process the request went to and to no other: the pid must still hold the journal and still have the start time it had when the exit was
// requested, so a pid the system has given to another process since is never signalled.
const (
	HandoverExitWait = 30 * time.Second
	HandoverTermWait = 15 * time.Second
	handoverPoll     = 250 * time.Millisecond
)

// HandoverClock is the time a handover waits by; tests give it a fake one.
type HandoverClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// SupervisorHandover ends one old supervisor after its exit was requested. The functions are the system's: who holds the journal, when a
// process started, how a signal is sent and how the supervisor is killed with its child processes.
type SupervisorHandover struct {
	// ExitWait and TermWait default to HandoverExitWait and HandoverTermWait; Poll to a quarter of a second.
	ExitWait, TermWait, Poll time.Duration
	Clock                    HandoverClock
	// Holds says whether pid still holds the supervisor journal. An error is "unknown": the handover keeps waiting, and signals nothing.
	Holds func(pid int) (bool, error)
	// StartOf is the system's record of when pid started ("" when it cannot be read).
	StartOf func(pid int) string
	Signal  func(pid int, sig syscall.Signal) error
	// Kill sends SIGKILL to the supervisor and to what it started.
	Kill func(pid int) error
	Logf func(format string, args ...any)
}

// HandoverOutcome is how a handover ended.
type HandoverOutcome string

const (
	HandoverExited     HandoverOutcome = "exited"     // the process went by itself (after the request or the SIGTERM)
	HandoverKilled     HandoverOutcome = "killed"     // it was killed
	HandoverPidReused  HandoverOutcome = "pid_reused" // the pid now belongs to another process: nothing was signalled
	HandoverUnverified HandoverOutcome = "unverified" // the process could not be verified: nothing was signalled
	HandoverCancelled  HandoverOutcome = "cancelled"  // the server stopped meanwhile
)

func (h SupervisorHandover) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Run waits for the supervisor pid, whose exit was requested when it had the start time start, and ends it if it does not go.
func (h SupervisorHandover) Run(ctx context.Context, pid int, start string) HandoverOutcome {
	if h.ExitWait <= 0 {
		h.ExitWait = HandoverExitWait
	}
	if h.TermWait <= 0 {
		h.TermWait = HandoverTermWait
	}
	if h.Poll <= 0 {
		h.Poll = handoverPoll
	}
	if h.Clock == nil {
		h.Clock = realClock{}
	}
	began := h.Clock.Now()
	h.logf("supervisor swap: waiting for supervisor pid %d to exit (%s)", pid, h.ExitWait)
	if outcome, done := h.waitGone(ctx, pid, start, h.ExitWait, began); done {
		return outcome
	}
	if outcome, ok := h.verify(pid, start, "SIGTERM"); !ok {
		return outcome
	}
	if err := h.Signal(pid, syscall.SIGTERM); err != nil {
		h.logf("supervisor swap: SIGTERM to supervisor pid %d failed: %v", pid, err)
	} else {
		h.logf("supervisor swap: supervisor pid %d did not exit within %s; sent SIGTERM, waiting %s", pid, h.ExitWait, h.TermWait)
	}
	if outcome, done := h.waitGone(ctx, pid, start, h.TermWait, began); done {
		return outcome
	}
	if outcome, ok := h.verify(pid, start, "SIGKILL"); !ok {
		return outcome
	}
	h.logf("supervisor swap: supervisor pid %d did not exit %s after SIGTERM; sending SIGKILL to it and its child processes", pid, h.TermWait)
	if err := h.Kill(pid); err != nil {
		h.logf("supervisor swap: killing supervisor pid %d: %v", pid, err)
	}
	return HandoverKilled
}

// gone says the requested process no longer holds the journal, or is no longer that process. An unknown answer is not gone.
func (h SupervisorHandover) gone(pid int, start string) bool {
	held, err := h.Holds(pid)
	if err != nil {
		return false
	}
	if !held {
		return true
	}
	now := h.StartOf(pid)
	return start != "" && now != "" && now != start
}

// waitGone waits up to wait for the process to go. done is true when the handover is over: the process went, or the server stops.
func (h SupervisorHandover) waitGone(ctx context.Context, pid int, start string, wait time.Duration, began time.Time) (HandoverOutcome, bool) {
	deadline := h.Clock.Now().Add(wait)
	for {
		if h.gone(pid, start) {
			h.logf("supervisor swap: old supervisor pid %d exited %s after it was told to stop", pid, h.Clock.Now().Sub(began).Round(time.Millisecond))
			return HandoverExited, true
		}
		left := deadline.Sub(h.Clock.Now())
		if left <= 0 {
			return "", false
		}
		select {
		case <-ctx.Done():
			return HandoverCancelled, true
		case <-h.Clock.After(min(h.Poll, left)):
		}
	}
}

// verify says whether pid may be sent sig: it still holds the journal and is the very process the exit was requested of.
func (h SupervisorHandover) verify(pid int, start, sig string) (HandoverOutcome, bool) {
	held, err := h.Holds(pid)
	if err != nil {
		h.logf("supervisor swap: cannot tell whether supervisor pid %d still holds the journal (%v); not sending %s", pid, err, sig)
		return HandoverUnverified, false
	}
	if !held {
		h.logf("supervisor swap: old supervisor pid %d exited", pid)
		return HandoverExited, false
	}
	if start == "" {
		h.logf("supervisor swap: the start time of supervisor pid %d was not known when its exit was requested; not sending %s", pid, sig)
		return HandoverUnverified, false
	}
	if now := h.StartOf(pid); now != start {
		if now == "" {
			h.logf("supervisor swap: the start time of supervisor pid %d cannot be read; not sending %s", pid, sig)
			return HandoverUnverified, false
		}
		h.logf("supervisor swap: pid %d now belongs to another process (its start time changed); not sending %s", pid, sig)
		return HandoverPidReused, false
	}
	return "", true
}
