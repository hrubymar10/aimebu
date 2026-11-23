package supervisor

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

const removingMessage = "Error: Harness container is removing. Run: bin/harness-docker-ctrl start"

// restartHarness runs turns as a delivery to a container does, with short waits between the starts of one turn.
func restartHarness(t *testing.T, limit time.Duration) (*warmHarness, func() []adapter.ExitInfo) {
	t.Helper()
	oldLimit, oldEvery := containerRestartLimit, containerRestartEvery
	containerRestartLimit, containerRestartEvery = limit, 10*time.Millisecond
	t.Cleanup(func() { containerRestartLimit, containerRestartEvery = oldLimit, oldEvery })
	h := newWarmHarness(t)
	h.restartable = true
	h.f.failStderr = removingMessage
	var mu sync.Mutex
	var exits []adapter.ExitInfo
	h.c.processSink = func(_ string, exit adapter.ExitInfo) {
		mu.Lock()
		exits = append(exits, exit)
		mu.Unlock()
	}
	h.spawn("agt_a")
	return h, func() []adapter.ExitInfo {
		mu.Lock()
		defer mu.Unlock()
		return append([]adapter.ExitInfo(nil), exits...)
	}
}

// A delivery whose launcher finds the harness container being removed waits and is started again: the turn ends with the answer of the start
// that found a container, and its earlier failures are not reported.
func TestTurnIsStartedAgainWhileItsContainerIsRemoving(t *testing.T) {
	h, exits := restartHarness(t, 5*time.Second)
	h.c.Warm = WarmConfig{} // every start is a process of its own
	h.f.coldFailures = 2
	h.run("agt_a", containerTurn)
	if h.f.cold != 3 {
		t.Fatalf("the turn was started %d times, want 3", h.f.cold)
	}
	got := exits()
	if len(got) != 1 || !got[0].TerminalEvent || got[0].ExitCode != 0 {
		t.Fatalf("the turn's exits: %+v", got)
	}
}

// The wait is bounded: a container that does not come back fails the turn with the launcher's own message, as it did before.
func TestTurnFailsWhenItsContainerStaysAway(t *testing.T) {
	h, exits := restartHarness(t, 80*time.Millisecond)
	h.c.Warm = WarmConfig{}
	h.f.coldFailures = 1000
	started := time.Now()
	// The turn is over when its retry window is: the window is measured from the first failed start, and the logic is bounded by that
	// window alone. How long the turn takes in wall-clock time also holds the machine's load (delivery setup, scheduling), so only a hang
	// is caught here, not a slow run; the lower bound cannot be made by load.
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.run("agt_a", containerTurn)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("the turn went on waiting for a container after its window was over")
	}
	if took := time.Since(started); took < 80*time.Millisecond {
		t.Fatalf("the turn gave up after %v, before its window of 80ms was over", took)
	}
	if h.f.cold < 2 {
		t.Fatalf("the turn was started %d times", h.f.cold)
	}
	got := exits()
	if len(got) != 1 || got[0].TerminalEvent || got[0].ExitCode != 1 || !strings.Contains(got[0].StderrTail, "Harness container is removing") {
		t.Fatalf("the turn's exits: %+v", got)
	}
}

// Only a launcher that found its container going is started again: any other failure, and a turn nothing can start again, fail at once.
func TestOtherFailuresAreNotStartedAgain(t *testing.T) {
	t.Run("another failure", func(t *testing.T) {
		h, exits := restartHarness(t, 5*time.Second)
		h.c.Warm = WarmConfig{}
		h.f.failStderr = "Error: Docker is not running."
		h.f.coldFailures = 1
		h.run("agt_a", containerTurn)
		if got := exits(); h.f.cold != 1 || len(got) != 1 || got[0].ExitCode != 1 {
			t.Fatalf("starts=%d exits=%+v", h.f.cold, got)
		}
	})
	t.Run("a turn outside a container", func(t *testing.T) {
		h, exits := restartHarness(t, 5*time.Second)
		h.restartable = false
		h.c.Warm = WarmConfig{}
		h.f.coldFailures = 1
		h.run("agt_a", nil)
		if got := exits(); h.f.cold != 1 || len(got) != 1 || got[0].ExitCode != 1 {
			t.Fatalf("starts=%d exits=%+v", h.f.cold, got)
		}
	})
}

// A stop of the agent during the wait wins: nothing is started again, and the turn ends as the stop's cancel.
func TestStopDuringTheWaitForAContainerEndsTheTurn(t *testing.T) {
	h, exits := restartHarness(t, time.Minute)
	containerRestartEvery = 100 * time.Millisecond
	h.c.Warm = WarmConfig{}
	h.f.coldFailures = 1000
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.run("agt_a", containerTurn)
	}()
	eventually(t, "the first failed start", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return h.f.cold >= 1
	})
	h.c.mu.Lock()
	desired := h.c.desired["agt_a"]
	desired.Desired = "stopped"
	h.c.desired["agt_a"] = desired
	h.c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn went on waiting after its agent was stopped")
	}
	got := exits()
	if len(got) != 1 || got[0].ExitCode != 130 || !got[0].CancelIntent {
		t.Fatalf("the turn's exits: %+v", got)
	}
}

// A warm process whose launcher found the container being removed started nothing there, so its slot is free at once: the turn is started
// again in a new warm process without a quarantine in between.
func TestWarmTurnIsStartedAgainWithoutAQuarantine(t *testing.T) {
	oldRetry, oldQuarantine := warmRetryEvery, warmStuckQuarantine
	warmRetryEvery, warmStuckQuarantine = time.Hour, time.Hour
	defer func() { warmRetryEvery, warmStuckQuarantine = oldRetry, oldQuarantine }()
	h, exits := restartHarness(t, 5*time.Second)
	defer func() { // the watchers read the intervals: they end before the intervals go back
		pool := h.c.pool()
		h.c.shutdownWarm()
		pool.watchers.Wait()
	}()
	h.c.ContainerState = func(string) (string, error) { return "running", nil }
	h.f.neverStartedNext = true
	h.f.next = "fail"
	started := time.Now()
	h.run("agt_a", containerTurn)
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the turn took %v", took)
	}
	got := exits()
	if h.f.started != 2 || h.f.cold != 0 || len(got) != 1 || !got[0].TerminalEvent || !got[0].ProcessAlive {
		t.Fatalf("warm starts=%d cold starts=%d exits=%+v", h.f.started, h.f.cold, got)
	}
	if !h.idle("agt_a") {
		t.Fatal("the process that answered was not kept")
	}
}
