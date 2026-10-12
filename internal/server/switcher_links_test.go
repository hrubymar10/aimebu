package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSwitcherLinkLoopRunsAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var calls atomic.Int64
	ensure := func() error { calls.Add(1); return nil }
	startSwitcherLinkLoop(ctx, &wg, time.Millisecond, ensure, func(string, ...any) {})
	if calls.Load() < 1 {
		t.Fatal("loop should run ensure once immediately")
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < 3 {
		t.Fatalf("loop did not tick: %d calls", calls.Load())
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on ctx cancel")
	}
	after := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != after {
		t.Fatal("ensure ran after the loop stopped")
	}
}

func TestOnceLoggerDedupsByMessage(t *testing.T) {
	var lines []string
	l := newOnceLogger(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	errA, errB := errors.New("claude: boom"), errors.New("codex: bang")
	l.report(errA)
	l.report(errA)
	l.report(errors.New("claude: boom")) // same message, different value
	if len(lines) != 1 {
		t.Fatalf("persistent error logged %d times, want 1: %v", len(lines), lines)
	}
	l.report(errB)
	l.report(errA)
	if len(lines) != 2 {
		t.Fatalf("distinct errors: got %d lines, want 2: %v", len(lines), lines)
	}
	l.report(nil) // recovery forgets, so a recurrence is logged again
	l.report(errA)
	if len(lines) != 3 {
		t.Fatalf("recurrence after recovery: got %d lines, want 3: %v", len(lines), lines)
	}
}
