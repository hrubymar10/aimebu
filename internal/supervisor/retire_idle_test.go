package supervisor

import (
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// A turn frees its agent's admission just before it sends its process frame, so that a follow-up delivery is not rejected. A supervisor
// retiring at idle must still count that turn as work until the frame is sent: if it closed its socket in between, the server would learn
// the turn's end only from the replacement's journal replay, and the old supervisor would be gone with the end unsaid. processSink runs in
// exactly that window (after the release, before the send), so what the retire check says there is what the retire poller could see.
func TestRetireWaitsForTheProcessFrameOfAReleasedTurn(t *testing.T) {
	h := newWarmHarness(t)
	h.c.Warm = WarmConfig{} // a process of its own that ends with the turn
	h.spawn("agt_a")
	var admitted, blocking, called bool
	h.c.processSink = func(string, adapter.ExitInfo) {
		called = true
		h.c.mu.Lock()
		admitted = len(h.c.active) > 0 || len(h.c.running) > 0
		h.c.mu.Unlock()
		blocking = h.c.hasRetireBlockingWork()
	}
	h.run("agt_a", nil)
	if !called {
		t.Fatal("the turn ended without reaching its process frame")
	}
	if admitted {
		t.Fatal("the agent's admission was still held at the process frame: the window this test is about is gone, rewrite the test")
	}
	if !blocking {
		t.Fatal("a turn whose process frame was not sent yet did not hold a supervisor retiring at idle")
	}
	if h.c.hasRetireBlockingWork() {
		t.Fatal("the turn still holds the supervisor after its process frame")
	}
}
