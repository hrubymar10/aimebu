package adapter

import (
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSignalGroupTreatsAGoneGroupAsDone(t *testing.T) {
	defer func(original func(int, syscall.Signal) error) { kill = original }(kill)
	var gotPID int
	var gotSignal syscall.Signal
	for name, answer := range map[string]error{"delivered": nil, "no such process": syscall.ESRCH} {
		kill = func(pid int, sig syscall.Signal) error { gotPID, gotSignal = pid, sig; return answer }
		if err := SignalGroup(4242, syscall.SIGKILL, func() bool { return false }); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if gotPID != -4242 || gotSignal != syscall.SIGKILL {
			t.Errorf("%s: signalled %d with %v", name, gotPID, gotSignal)
		}
	}
}

func TestSignalGroupNeverSignalsAReapedProcess(t *testing.T) {
	defer func(original func(int, syscall.Signal) error) { kill = original }(kill)
	called := false
	kill = func(int, syscall.Signal) error { called = true; return nil }
	if err := SignalGroup(7, syscall.SIGKILL, func() bool { return true }); err != nil || called {
		t.Fatalf("a reaped process was signalled: err=%v called=%v", err, called)
	}
}

func TestReaperRunsTheHoldHookOnceMarked(t *testing.T) {
	var reaper Reaper
	if reaper.Reaped() {
		t.Fatal("a new reaper is not reaped")
	}
	ran := 0
	restore := HoldAfterReap(func() { ran++ })
	reaper.MarkReaped()
	restore()
	reaper.MarkReaped()
	if !reaper.Reaped() || ran != 1 {
		t.Fatalf("reaped=%v hook ran %d times", reaper.Reaped(), ran)
	}
}

func TestSignalGroupTrustsPermissionErrorsOnlyOnceReaped(t *testing.T) {
	defer func(original func(int, syscall.Signal) error) { kill = original }(kill)
	kill = func(int, syscall.Signal) error { return syscall.EPERM }
	// Reaped a moment after the signal failed (the exit had not been waited for yet): still a finished group.
	var polls atomic.Int32
	if err := SignalGroup(7, syscall.SIGTERM, func() bool { return polls.Add(1) > 3 }); err != nil {
		t.Fatalf("reaped after a moment: %v", err)
	}
	// A live process: the permission problem is real and is returned, after the grace.
	started := time.Now()
	err := SignalGroup(7, syscall.SIGTERM, func() bool { return false })
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("live process: %v", err)
	}
	if elapsed := time.Since(started); elapsed < reapGrace-100*time.Millisecond || elapsed > reapGrace+10*time.Second {
		t.Fatalf("the grace was %v", elapsed)
	}
}

func TestSignalGroupReturnsOtherErrors(t *testing.T) {
	defer func(original func(int, syscall.Signal) error) { kill = original }(kill)
	kill = func(int, syscall.Signal) error { return syscall.EINVAL }
	if err := SignalGroup(7, syscall.Signal(999), func() bool { return false }); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("got %v", err)
	}
}

func TestKillAfterEscalatesOnlyWhileTheProcessIsLive(t *testing.T) {
	defer func(original func(int, syscall.Signal) error) { kill = original }(kill)
	var calls atomic.Int32
	var lastPID atomic.Int64
	var lastSignal atomic.Int32
	kill = func(pid int, sig syscall.Signal) error {
		calls.Add(1)
		lastPID.Store(int64(pid))
		lastSignal.Store(int32(sig))
		return nil
	}
	const grace = 20 * time.Millisecond
	// The turn finishes inside the grace: nothing is killed.
	done := make(chan struct{})
	close(done)
	KillAfter(31, grace, done, func() bool { return false })
	if calls.Load() != 0 {
		t.Fatalf("killed a turn that had finished")
	}
	// The grace runs out and the process is reaped (its turn is still reporting the exit): nothing is signalled either.
	KillAfter(31, grace, make(chan struct{}), func() bool { return true })
	if calls.Load() != 0 {
		t.Fatalf("signalled a reaped process")
	}
	// A live process after the grace gets SIGKILL on its group.
	KillAfter(31, grace, make(chan struct{}), func() bool { return false })
	if calls.Load() != 1 || lastPID.Load() != -31 || syscall.Signal(lastSignal.Load()) != syscall.SIGKILL {
		t.Fatalf("calls=%d pid=%d signal=%d", calls.Load(), lastPID.Load(), lastSignal.Load())
	}
}
