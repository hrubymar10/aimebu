package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
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

// A retiring instance leaves only when it owes nothing: no job (a native one that runs on its own included, which a replacement used to
// adopt), no warm process, no exit the server has not acknowledged, no stop acknowledgement waiting for a process.
func TestRetireOwesUntilNothingIsOutstanding(t *testing.T) {
	h := newWarmHarness(t)
	c := h.c
	if c.retireOwes() {
		t.Fatal("a fresh instance with nothing to finish owes something")
	}

	c.runner().mu.Lock()
	c.runner().jobs["job_native"] = &commandJob{detachable: true}
	c.runner().mu.Unlock()
	if c.hasRetireBlockingWork() {
		t.Fatal("the premise changed: a leavable job no longer lets the old check pass")
	}
	if !c.retireOwes() {
		t.Fatal("a native job that could be left behind did not hold the retiring instance")
	}
	c.runner().mu.Lock()
	delete(c.runner().jobs, "job_native")
	c.runner().mu.Unlock()

	c.mu.Lock()
	c.pendingStopAck = map[string]protocol.Frame{"agt_a": {Type: protocol.Ack}}
	c.mu.Unlock()
	if !c.retireOwes() {
		t.Fatal("a deferred stop acknowledgement did not hold the retiring instance")
	}
	c.mu.Lock()
	c.pendingStopAck = nil
	c.mu.Unlock()

	// A warm process after a turn; the turn's exit is not acknowledged either.
	h.spawn("agt_a")
	h.run("agt_a", nil)
	exits, err := c.Journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 {
		t.Fatalf("pending exits = %v, %v", exits, err)
	}
	if !c.retireOwes() {
		t.Fatal("an unacknowledged exit and a warm process did not hold the retiring instance")
	}
	if err := c.Journal.AcknowledgeExit(context.Background(), exits[0].TurnID); err != nil {
		t.Fatal(err)
	}
	if !c.retireOwes() {
		t.Fatal("a warm process did not hold the retiring instance")
	}
	if !c.RetireAllWarm("test") {
		t.Fatal("the warm process was not confirmed gone")
	}
	// The turn's answer is not receipted yet either.
	answers, err := c.Journal.PendingAnswers(context.Background())
	if err != nil || len(answers) != 1 {
		t.Fatalf("pending answers = %v, %v", answers, err)
	}
	if !c.retireOwes() {
		t.Fatal("an answer the server has not receipted did not hold the retiring instance")
	}
	if err := c.Journal.RecordReceipt(context.Background(), protocol.ReceiptBody{TurnID: answers[0].TurnID, AnswerRev: answers[0].AnswerRev, Digest: answers[0].Digest, Result: "committed"}); err != nil {
		t.Fatal(err)
	}
	if c.retireOwes() {
		t.Fatal("an instance with everything acknowledged and nothing alive still owes something")
	}
}

func TestFinishRetireRemovesTheJournalOnlyWhenNothingIsOwed(t *testing.T) {
	h := newWarmHarness(t)
	c := h.c
	path := c.Journal.path
	if removed, err := c.FinishRetire(); removed || err != nil {
		t.Fatalf("an instance that did not retire removed its journal: %v, %v", removed, err)
	}
	c.retired.Store(true)
	c.runner().mu.Lock()
	c.runner().jobs["job_x"] = &commandJob{detachable: true}
	c.runner().mu.Unlock()
	if removed, err := c.FinishRetire(); removed || err != nil {
		t.Fatalf("an instance that owes a job removed its journal: %v, %v", removed, err)
	}
	c.runner().mu.Lock()
	delete(c.runner().jobs, "job_x")
	c.runner().mu.Unlock()
	if removed, err := c.FinishRetire(); !removed || err != nil {
		t.Fatalf("a clean empty instance did not remove its journal: %v, %v", removed, err)
	}
	for _, suffix := range []string{"", ".lock", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("%s%s is still there (%v)", path, suffix, err)
		}
	}
	if held, _, err := JournalOwner(path); held || err != nil {
		t.Fatalf("the removed journal's lock is held: %v, %v", held, err)
	}
}

type sweepCountingAdapter struct {
	*warmFake
	sweeps int
}

func (s *sweepCountingAdapter) SweepKeys() { s.sweeps++ }

// The start-up sweep deletes the generated adapter configs of the runtime roots all instances share: it must not run while another instance,
// whose turns still read them, holds its journal.
func TestSweepLegacyKeysSkippedWhileAnotherInstanceHoldsItsLock(t *testing.T) {
	dir := t.TempDir()
	open := func(parts ...string) *Journal {
		j, err := OpenJournal(context.Background(), filepath.Join(append([]string{dir}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	mine := open("journals", "new.sqlite")
	defer mine.Close()
	sweeper := &sweepCountingAdapter{warmFake: &warmFake{}}
	c := NewClient("machine", "key", "http://127.0.0.1:9897", mine, sweeper)

	c.SweepLegacyKeys()
	if sweeper.sweeps != 1 {
		t.Fatalf("a lone instance did not sweep (%d)", sweeper.sweeps)
	}
	for name, parts := range map[string][]string{"legacy": {"journal.sqlite"}, "sibling": {"journals", "old.sqlite"}} {
		other := open(parts...)
		before := sweeper.sweeps
		c.SweepLegacyKeys()
		if sweeper.sweeps != before {
			t.Fatalf("%s: swept while another instance held its lock", name)
		}
		other.Close()
		c.SweepLegacyKeys()
		if sweeper.sweeps != before+1 {
			t.Fatalf("%s: did not sweep once the other instance was gone", name)
		}
	}
}

func runWithPeer(t *testing.T, journal *Journal, peer func(ctx context.Context, conn *websocket.Conn)) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, err := readFrame(r.Context(), conn); err != nil {
			return
		}
		ack, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.HelloAck})
		if err := conn.Write(r.Context(), websocket.MessageText, ack); err != nil {
			return
		}
		peer(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	client := NewClient("machine", "key", server.URL, journal, adapter.NewFake())
	client.ReconnectDelay = 10 * time.Millisecond
	return client
}

func writeFrame(ctx context.Context, conn *websocket.Conn, frame protocol.Frame) {
	data, _ := json.Marshal(frame)
	_ = conn.Write(ctx, websocket.MessageText, data)
}

// The server grants the key of an agent a retiring instance held once it is handed over, in an agent_keys frame after hello_ack.
func TestAgentKeysFrameGrantsAKeyAfterHello(t *testing.T) {
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	key := "aak_" + strings.Repeat("k", 43)
	client := runWithPeer(t, journal, func(ctx context.Context, conn *websocket.Conn) {
		writeFrame(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.AgentKeys, AgentKeys: []protocol.AgentKeyGrant{{AgentID: "agt_a", Key: key}}})
		for {
			if _, err := readFrame(ctx, conn); err != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go client.RunOnce(ctx)
	deadline := time.Now().Add(waitGuard)
	for client.keyOf("agt_a") != key {
		if time.Now().After(deadline) {
			t.Fatal("the agent_keys frame did not give the supervisor the key")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A retire request keeps the connection (it carries the acknowledgements) while a job runs, even a native one a replacement could adopt; when the
// job ends and nothing is owed the instance leaves, and its journal can be removed.
func TestRetireAtIdleWaitsForJobsThenLeaves(t *testing.T) {
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := runWithPeer(t, journal, func(ctx context.Context, conn *websocket.Conn) {
		writeFrame(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.RetireAtIdle})
		for {
			if _, err := readFrame(ctx, conn); err != nil {
				return
			}
		}
	})
	client.runner().mu.Lock()
	client.runner().jobs["job_native"] = &commandJob{detachable: true}
	client.runner().mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- client.Run(t.Context()) }()
	select {
	case <-done:
		t.Fatal("the instance left while a native job still ran")
	case <-time.After(1500 * time.Millisecond):
	}
	client.runner().mu.Lock()
	delete(client.runner().jobs, "job_native")
	client.runner().mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(waitGuard):
		t.Fatal("the instance did not leave after its job ended")
	}
	if removed, err := client.FinishRetire(); !removed || err != nil {
		t.Fatalf("FinishRetire = %v, %v", removed, err)
	}
}
