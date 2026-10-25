package adapter

import (
	"errors"
	"sync/atomic"
	"syscall"
	"time"
)

// kill is syscall.Kill, replaceable in tests.
var kill = syscall.Kill

// reapGrace bounds how long a permission error is given to turn out to be a finished process: see SignalGroup.
const reapGrace = 2 * time.Second

// SignalGroup delivers sig to the process group led by pid, for a turn's Cancel or Kill, and treats a group that is already gone as success.
//
// A harness runs in its own group, and a cancel may land after the process has exited, for instance a second Cancel while the first one's
// signal is still taking effect, or a deadline firing as a turn ends. Linux then answers ESRCH. macOS can answer EPERM for a group whose
// members have all exited. Both mean "nothing left to cancel". An EPERM is only trusted once the process is known to have been reaped
// (reaped reports it, and is given a short grace, because the exit may not have been waited for yet); on a live process it is a real
// permission problem and is returned.
func SignalGroup(pid int, sig syscall.Signal, reaped func() bool) error {
	// A process that has been waited for has nothing to signal, and its group id may already belong to someone else.
	if reaped() {
		return nil
	}
	err := kill(-pid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) {
		deadline := time.Now().Add(reapGrace)
		for {
			if reaped() {
				return nil
			}
			if !time.Now().Before(deadline) {
				return err
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return err
}

// KillAfter is the escalation after a polite cancel: it waits up to grace for the turn to finish (done) and, if the process is still not
// reaped by then, sends SIGKILL to its group, through SignalGroup like every other signal. Turns run it in a goroutine.
func KillAfter(pid int, grace time.Duration, done <-chan struct{}, reaped func() bool) {
	select {
	case <-done:
	case <-time.After(grace):
		_ = SignalGroup(pid, syscall.SIGKILL, reaped)
	}
}

// Reaper records that a turn's process has been waited for. A turn calls MarkReaped right after cmd.Wait returns, before it reports the
// exit, and its Cancel and Kill check Reaped (through SignalGroup), so the moment between the reap and the end of the turn's own
// bookkeeping is treated like the time after it: nothing is signalled and no cancel intent is recorded.
type Reaper struct{ reaped atomic.Bool }

// Reaped says whether the process has been waited for.
func (r *Reaper) Reaped() bool { return r.reaped.Load() }

// MarkReaped records the reap, then runs the test hook, if any.
func (r *Reaper) MarkReaped() {
	r.reaped.Store(true)
	if hook := reapHook.Load(); hook != nil {
		(*hook)()
	}
}

var reapHook atomic.Pointer[func()]

// HoldAfterReap makes every turn run hook right after its process is reaped, until the returned function is called. It is for tests that
// need to act in the gap between the reap and the end of the turn; production code never sets it.
func HoldAfterReap(hook func()) (restore func()) {
	reapHook.Store(&hook)
	return func() { reapHook.Store(nil) }
}
