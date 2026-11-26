package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"database/sql"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestRunOnceCancellationDoesNotWaitForCloseHandshake(t *testing.T) {
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	releasePeer := make(chan struct{})
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
		<-releasePeer // Deliberately leave the client's close frame unanswered.
	}))
	defer server.Close()
	defer close(releasePeer)
	client := NewClient("machine", "key", server.URL, journal, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not connect")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled supervisor waited for WebSocket close handshake")
	}
}

func TestMatchingReconnectClearsRetireState(t *testing.T) {
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		for {
			if _, err := readFrame(r.Context(), conn); err != nil {
				return
			}
		}
	}))
	defer peer.Close()
	client := NewClient("machine", "key", peer.URL, journal, adapter.NewFake())
	client.retireRequested.Store(true)
	client.stopping = true
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not reconnect")
	}
	client.mu.Lock()
	stopping := client.stopping
	client.mu.Unlock()
	if stopping || client.retireRequested.Load() {
		t.Fatal("matching reconnect kept the supervisor retiring")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not stop")
	}
}

// waitGuard is how long a test waits for a state that stays once it is reached (a connection made, an ack sent, a run stopped). It only stops a
// test that would otherwise hang for good: none of them depends on how soon the state arrives, so a loaded machine makes them slower, not red.
// The one wait that is itself the property (a close that must not wait for the peer's handshake) keeps its own short limit.
const waitGuard = 30 * time.Second

func TestLongConnectionDoesNotCountTowardIdleDisconnect(t *testing.T) {
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	firstRelease := make(chan struct{})
	var connections atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if connections.Add(1) == 1 {
			<-firstRelease
			_ = conn.CloseNow()
			return
		}
		for {
			if _, err := readFrame(r.Context(), conn); err != nil {
				return
			}
		}
	}))
	defer peer.Close()
	defer func() {
		select {
		case <-firstRelease:
		default:
			close(firstRelease)
		}
	}()
	client := NewClient("machine", "key", peer.URL, journal, adapter.NewFake())
	client.IdleDisconnectLimit = 40 * time.Millisecond
	client.ReconnectDelay = 5 * time.Millisecond
	ready := make(chan struct{}, 2)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(waitGuard):
		t.Fatal("first connection was not ready")
	}
	time.Sleep(120 * time.Millisecond)
	close(firstRelease)
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("supervisor retired on a brief disconnect after a long connection: %v", err)
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not reconnect")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not stop")
	}
}

func TestReconnectKeepsPausedWarmRefusal(t *testing.T) {
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, err := readFrame(r.Context(), conn); err != nil {
			return
		}
		ack, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.HelloAck, Desired: []protocol.DesiredAgent{
			{AgentID: "active", Desired: "active"},
			{AgentID: "paused", Desired: "paused"},
			{AgentID: "stopped", Desired: "stopped"},
		}})
		if err := conn.Write(r.Context(), websocket.MessageText, ack); err != nil {
			return
		}
		for {
			if _, err := readFrame(r.Context(), conn); err != nil {
				return
			}
		}
	}))
	defer peer.Close()
	client := NewClient("machine", "key", peer.URL, journal, adapter.NewFake())
	client.warmRefusals = make(map[string]protocol.WarmRefusal)
	for _, id := range []string{"active", "paused", "stopped", "absent"} {
		client.warmRefusals[id] = protocol.WarmRefusal{AgentID: id, Reason: "warm unavailable"}
	}
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not connect")
	}
	client.mu.Lock()
	_, active := client.warmRefusals["active"]
	_, paused := client.warmRefusals["paused"]
	_, stopped := client.warmRefusals["stopped"]
	_, absent := client.warmRefusals["absent"]
	client.mu.Unlock()
	if !active || !paused || stopped || absent {
		t.Fatalf("warm refusals after reconnect: active=%t paused=%t stopped=%t absent=%t", active, paused, stopped, absent)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not stop")
	}
}

func TestReconnectDoesNotReplaySettledNativeSessionForRemovedAgent(t *testing.T) {
	ctx := t.Context()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "old-turn", AgentID: "removed", SpecJSON: json.RawMessage(`{}`), Mode: "normal"}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(adapter.Event{Kind: adapter.EventNativeSessionStarted, SourceEventID: "old-session", NativeSessionHandle: "thread-1"})
	if err := journal.RecordEvent(ctx, "old-turn", "old-session", string(adapter.EventNativeSessionStarted), "", payload); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordExit(ctx, "old-turn", 1, false, false); err != nil {
		t.Fatal(err)
	}
	if err := journal.AcknowledgeExit(ctx, "old-turn"); err != nil {
		t.Fatal(err)
	}
	seen := make(chan []string, 1)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		hello, err := readFrame(r.Context(), conn)
		if err != nil {
			return
		}
		if len(hello.Agents) != 0 {
			seen <- []string{"unexpected hello inventory"}
			return
		}
		for _, frame := range []protocol.Frame{{V: protocol.Version, Type: protocol.HelloAck}, {V: protocol.Version, Type: protocol.Ping}} {
			raw, _ := json.Marshal(frame)
			if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
				return
			}
		}
		var kinds []string
		for {
			frame, err := readFrame(r.Context(), conn)
			if err != nil {
				return
			}
			kinds = append(kinds, frame.Type)
			if frame.Type == protocol.Pong {
				seen <- kinds
				return
			}
		}
	}))
	defer peer.Close()
	client := NewClient("machine", "key", peer.URL, journal, adapter.NewFake())
	client.PingInterval = time.Hour
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(runCtx) }()
	select {
	case kinds := <-seen:
		if len(kinds) != 1 || kinds[0] != protocol.Pong {
			t.Fatalf("reconnect sent frames for settled removed agent: %v", kinds)
		}
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not answer reconnect ping")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not stop")
	}
}

func TestStopAcceptingAndDrainJournalsInterruptedTurn(t *testing.T) {
	testStopAcceptingAndDrainJournalsTurn(t, false)
}

func TestOwnerRestartJournalsOneTimeContinuation(t *testing.T) {
	testStopAcceptingAndDrainJournalsTurn(t, true)
}

func testStopAcceptingAndDrainJournalsTurn(t *testing.T, restart bool) {
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	fake.Script("turn-drain", adapter.Step{Kind: adapter.StepHang})
	turn, err := fake.Start(ctx, adapter.TurnSpec{TurnID: "turn-drain", Mode: adapter.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "turn-drain", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, fake)
	client.active["agent"] = "turn-drain"
	client.running["agent"] = turn
	client.turns.Add(1)
	runCtx, stopRun := context.WithCancel(ctx)
	go client.runTurn(runCtx, protocol.Delivery{AgentID: "agent", TurnID: "turn-drain", Mode: adapter.ModeNormal}, turn)
	if restart {
		client.MarkRestartInterruption()
	}
	stopRun() // SIGTERM cancels the run context before the local drain starts.
	drainCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.StopAcceptingAndDrain(drainCtx); err != nil {
		t.Fatal(err)
	}
	if !client.stopping {
		t.Fatal("supervisor still accepts deliveries")
	}
	exits, err := journal.PendingExits(ctx)
	if err != nil || len(exits) != 1 || exits[0].TurnID != "turn-drain" || exits[0].Exit.CancelIntent == restart || exits[0].Exit.Interrupted != restart {
		t.Fatalf("journaled exits=%+v %v", exits, err)
	}
}

type stubbornTurn struct {
	events chan adapter.Event
	done   chan struct{}
}

type reconnectStopTurn struct{ cancelled chan struct{} }

func (t *reconnectStopTurn) Events() <-chan adapter.Event { return nil }
func (t *reconnectStopTurn) Cancel() error {
	select {
	case t.cancelled <- struct{}{}:
	default:
	}
	return nil
}
func (t *reconnectStopTurn) Wait() (adapter.ExitInfo, error) { return adapter.ExitInfo{}, nil }

func TestReplayedStopAfterDesiredSnapshotRunsControlCancellation(t *testing.T) {
	client := &Client{desired: map[string]protocol.DesiredAgent{}, active: map[string]string{}, running: map[string]adapter.Turn{}}
	turn := &reconnectStopTurn{cancelled: make(chan struct{}, 1)}
	client.desired["agent"] = protocol.DesiredAgent{AgentID: "agent", Generation: 2, Desired: "stopped", DesiredRev: 3}
	client.active["agent"] = "turn"
	client.running["agent"] = turn
	if !client.handleControl(protocol.Frame{AgentID: "agent", Generation: 2, DesiredState: "stopped", DesiredRev: 3}) {
		t.Fatal("stop ack was not deferred while the process runs")
	}
	select {
	case <-turn.cancelled:
	default:
		t.Fatal("stop control at the snapshot revision did not cancel the turn")
	}
	// The control path also schedules the same force-kill fallback used for a
	// live stop. Mark the turn inactive so the delayed callback has no work.
	client.mu.Lock()
	delete(client.active, "agent")
	client.mu.Unlock()
}

func TestStopControlAckWaitsForProcessExit(t *testing.T) {
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.RecordStart(t.Context(), StartRecord{TurnID: "turn-stop", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	frames := make(chan protocol.Frame, 4)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		hello, err := readFrame(r.Context(), conn)
		if err != nil || !hello.SupportsStopReplay {
			return
		}
		for _, frame := range []protocol.Frame{
			{V: protocol.Version, Type: protocol.HelloAck, Desired: []protocol.DesiredAgent{{AgentID: "agent", Generation: 2, Desired: "stopped", DesiredRev: 3}}},
			{V: protocol.Version, Type: protocol.Control, AgentID: "agent", Generation: 2, DesiredState: "stopped", DesiredRev: 3},
		} {
			raw, _ := json.Marshal(frame)
			if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
				return
			}
		}
		for {
			frame, err := readFrame(r.Context(), conn)
			if err != nil {
				return
			}
			frames <- frame
		}
	}))
	defer peer.Close()
	client := NewClient("machine", "key", peer.URL, journal, adapter.NewFake())
	turn := &stubbornTurn{events: make(chan adapter.Event), done: make(chan struct{})}
	client.active["agent"] = "turn-stop"
	client.running["agent"] = turn
	client.turns.Add(1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go client.runTurn(ctx, protocol.Delivery{AgentID: "agent", Generation: 2, TurnID: "turn-stop", Agent: adapter.AgentSpec{Tool: "fake"}}, turn)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	finished := make(chan error, 1)
	go func() { finished <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not connect")
	}
	select {
	case frame := <-frames:
		t.Fatalf("frame sent before process exit: %+v", frame)
	case <-time.After(150 * time.Millisecond):
	}
	close(turn.events)
	close(turn.done)
	select {
	case frame := <-frames:
		if frame.Type != protocol.Ack || frame.Level != "control" || frame.Ref != "3" {
			t.Fatalf("first frame after process exit: %+v", frame)
		}
	case <-time.After(waitGuard):
		t.Fatal("stop ack did not follow process exit")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not stop")
	}
}

func TestFailedExitJournalClearsDeferredStopAck(t *testing.T) {
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RecordStart(t.Context(), StartRecord{TurnID: "turn-bad-journal", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	client.OnError = func(error) {}
	client.active["agent"] = "turn-bad-journal"
	client.pendingStopAck = map[string]protocol.Frame{"agent": {Type: protocol.Ack, Level: "control", Ref: "3"}}
	turn := &stubbornTurn{events: make(chan adapter.Event), done: make(chan struct{})}
	close(turn.events)
	close(turn.done)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	client.turns.Add(1)
	client.runTurn(t.Context(), protocol.Delivery{AgentID: "agent", TurnID: "turn-bad-journal", Agent: adapter.AgentSpec{Tool: "fake"}}, turn)
	if len(client.pendingStopAck) != 0 {
		t.Fatal("failed exit journal kept the deferred stop ack")
	}
}

func (t *stubbornTurn) Events() <-chan adapter.Event { return t.events }
func (t *stubbornTurn) Cancel() error                { return nil }
func (t *stubbornTurn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	return adapter.ExitInfo{}, nil
}

// exitRetryTurn is a turn whose process ends on its own with the given exit: only its events channel has to be closed by the test.
type exitRetryTurn struct{ events chan adapter.Event }

func (t *exitRetryTurn) Events() <-chan adapter.Event { return t.events }
func (t *exitRetryTurn) Cancel() error                { return nil }
func (t *exitRetryTurn) Wait() (adapter.ExitInfo, error) {
	return adapter.ExitInfo{ExitCode: 0, TerminalEvent: true}, nil
}

// runExitRetryTurn runs one turn of a client to its end and returns when the turn goroutine is done.
func runExitRetryTurn(t *testing.T, client *Client, turnID string, turn *exitRetryTurn) {
	t.Helper()
	close(turn.events) // the harness stream ends at once: the turn is only here so its exit write runs
	client.active["agent"] = turnID
	client.running["agent"] = turn
	client.turns.Add(1)
	go client.runTurn(context.Background(), protocol.Delivery{AgentID: "agent", TurnID: turnID, Mode: adapter.ModeNormal}, turn)
	client.turns.Wait()
}

func TestExitWriteIsRetriedBeforeTheExitIsHeld(t *testing.T) {
	previousAttempts, previousBackoff, previousRetry := exitWriteAttempts, exitWriteBackoff, unrecordedRetryEvery
	exitWriteAttempts, exitWriteBackoff, unrecordedRetryEvery = 3, time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		exitWriteAttempts, exitWriteBackoff, unrecordedRetryEvery = previousAttempts, previousBackoff, previousRetry
	})
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "turn-retry", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	failures := make(chan error, 1)
	client.OnError = func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	var calls atomic.Int32
	client.mu.Lock()
	client.recordExit = func(writeCtx context.Context, turnID string, exit adapter.ExitInfo) error {
		if calls.Add(1) <= 2 {
			return errors.New("the journal is busy")
		}
		return journal.RecordExitInfo(writeCtx, turnID, exit)
	}
	client.mu.Unlock()
	runExitRetryTurn(t, client, "turn-retry", &exitRetryTurn{events: make(chan adapter.Event)})
	exits, err := journal.PendingExits(ctx)
	if err != nil || len(exits) != 1 || exits[0].TurnID != "turn-retry" || !exits[0].Exit.TerminalEvent {
		t.Fatalf("retried exit write did not reach the journal: exits=%+v err=%v", exits, err)
	}
	if calls.Load() != 3 {
		t.Fatalf("the exit write was tried %d times, not the two failures plus the successful write", calls.Load())
	}
	select {
	case err := <-failures:
		if strings.Contains(err.Error(), "the journal is busy") {
			t.Fatalf("a retried write was reported as a failure: %v", err)
		}
	default:
	}
}

func TestUnrecordedExitIsHeldReportedAndRetried(t *testing.T) {
	previousAttempts, previousBackoff, previousRetry := exitWriteAttempts, exitWriteBackoff, unrecordedRetryEvery
	exitWriteAttempts, exitWriteBackoff, unrecordedRetryEvery = 2, time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		exitWriteAttempts, exitWriteBackoff, unrecordedRetryEvery = previousAttempts, previousBackoff, previousRetry
	})
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "turn-held", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	reported := make(chan error, 8)
	client.OnError = func(err error) { reported <- err }
	client.mu.Lock()
	client.recordExit = func(context.Context, string, adapter.ExitInfo) error { return errors.New("the journal is busy") }
	client.mu.Unlock()
	runExitRetryTurn(t, client, "turn-held", &exitRetryTurn{events: make(chan adapter.Event)})
	client.mu.Lock()
	held := len(client.unrecordedExits)
	client.mu.Unlock()
	if held != 1 {
		t.Fatalf("the exit whose write kept failing is not held in memory (%d)", held)
	}
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "the journal is busy") {
			select {
			case again := <-reported:
				err = fmt.Errorf("%w; then %v", err, again)
			default:
			}
			t.Fatalf("the held exit was not the reported failure: %v", err)
		}
	default:
		t.Fatal("the exit whose journal write kept failing was not reported")
	}
	// The background retry writes the exit when the journal takes writes again, and the exit reaches the journal like any other.
	client.mu.Lock()
	client.recordExit = journal.RecordExitInfo
	client.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		client.mu.Lock()
		held = len(client.unrecordedExits)
		client.mu.Unlock()
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background retry did not record the held exit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	exits, err := journal.PendingExits(ctx)
	if err != nil || len(exits) != 1 || exits[0].TurnID != "turn-held" || !exits[0].Exit.TerminalEvent {
		t.Fatalf("the held exit did not reach the journal: exits=%+v err=%v", exits, err)
	}
	close(client.unrecordedStop) // no retry outlives the test
}

func TestRetireOwesCountsUnrecordedExits(t *testing.T) {
	journal, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	if client.retireOwes() {
		t.Fatal("a client whose journal owes nothing retires")
	}
	client.mu.Lock()
	client.unrecordedExits["turn-held"] = unrecordedExit{agentID: "agent", turnID: "turn-held"}
	client.mu.Unlock()
	if !client.retireOwes() {
		t.Fatal("an instance holding an exit it could not record does not owe it")
	}
}

// TestExitWriteErrNoRowsMeansAlreadySettled: an exit whose journal write answers sql.ErrNoRows finds its row settled by another path (a
// dead-turn settle, a refused-exit settle, a drain timeout), so it is written once, held nowhere, reported nowhere, and the instance can retire.
func TestExitWriteErrNoRowsMeansAlreadySettled(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "turn-settled", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// Another path settles the row first, the way a dead-turn settle or a refused-exit settle does; the settle reports its exit and the
	// server acknowledges it, so the journal is left owing nothing but the row is no longer running.
	if err := journal.RecordExitInfo(ctx, "turn-settled", supervisorLostExit()); err != nil {
		t.Fatal(err)
	}
	if err := journal.AcknowledgeExit(ctx, "turn-settled"); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	reported := make(chan error, 8)
	client.OnError = func(err error) { reported <- err }
	var calls atomic.Int32
	client.mu.Lock()
	client.recordExit = func(writeCtx context.Context, turnID string, exit adapter.ExitInfo) error {
		calls.Add(1)
		return journal.RecordExitInfo(writeCtx, turnID, exit)
	}
	client.mu.Unlock()
	runExitRetryTurn(t, client, "turn-settled", &exitRetryTurn{events: make(chan adapter.Event)})
	client.mu.Lock()
	held := len(client.unrecordedExits)
	client.mu.Unlock()
	if held != 0 {
		t.Fatalf("an exit whose row was settled by another path is held in memory (%d)", held)
	}
	if client.retireOwes() {
		t.Fatal("an instance whose only unwritten exit was settled by another path still owes work")
	}
	select {
	case err := <-reported:
		t.Fatalf("a settled row was reported as a failed exit write: %v", err)
	default:
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the exit write ran %d times on an already settled row, not once without retries", n)
	}
}

// TestHeldExitStopsWhenItsRowIsSettledByAnotherPath: a held exit whose background retry answers sql.ErrNoRows stops instead of retrying for
// the life of the process, so the instance can retire.
func TestHeldExitStopsWhenItsRowIsSettledByAnotherPath(t *testing.T) {
	previousRetry := unrecordedRetryEvery
	unrecordedRetryEvery = 5 * time.Millisecond
	t.Cleanup(func() { unrecordedRetryEvery = previousRetry })
	journal, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	reported := make(chan error, 8)
	client.OnError = func(err error) { reported <- err }
	client.mu.Lock()
	client.recordExit = func(context.Context, string, adapter.ExitInfo) error { return sql.ErrNoRows }
	client.mu.Unlock()
	client.holdUnrecordedExit(protocol.Delivery{AgentID: "agent", TurnID: "turn-held-settled"}, adapter.ExitInfo{ExitCode: 0, TerminalEvent: true})
	deadline := time.Now().Add(2 * time.Second)
	for {
		client.mu.Lock()
		held := len(client.unrecordedExits)
		client.mu.Unlock()
		if held == 0 && !client.retireOwes() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a held exit whose row was settled by another path is still retried (held=%d owes=%v)", held, client.retireOwes())
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-reported:
		t.Fatalf("a held exit whose row was settled by another path was reported again: %v", err)
	default:
	}
	close(client.unrecordedStop) // no retry outlives the test
}

func TestStopAcceptingAndDrainTimesOutWithTurnID(t *testing.T) {
	journal, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.RecordStart(context.Background(), StartRecord{TurnID: "turn-stuck", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	turn := &stubbornTurn{events: make(chan adapter.Event), done: make(chan struct{})}
	client.active["agent"] = "turn-stuck"
	client.running["agent"] = turn
	client.turns.Add(1)
	go client.runTurn(context.Background(), protocol.Delivery{AgentID: "agent", TurnID: "turn-stuck"}, turn)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = client.StopAcceptingAndDrain(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "agent/turn-stuck") {
		t.Fatalf("drain timeout=%v", err)
	}
	close(turn.events)
	close(turn.done)
	client.turns.Wait()
}

// The server names the spelling a session was saved under; the supervisor starts the harness there only when it really is the checkout.
func TestLaunchSpellingIsUsedOnlyForTheCheckoutItself(t *testing.T) {
	base := t.TempDir()
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(real, "proj")
	other := filepath.Join(real, "other")
	for _, dir := range []string{checkout, other} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(real, "link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	otherLink := filepath.Join(real, "other-link")
	if err := os.Symlink(other, otherLink); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ launch, want string }{
		"none":                     {"", ""},
		"the checkout itself":      {checkout, ""},
		"a link to the checkout":   {link, link},
		"a link to another folder": {otherLink, ""},
		"a relative path":          {"proj", ""},
		"an unclean path":          {link + "/../link", ""},
		"a missing path":           {filepath.Join(real, "gone"), ""},
	} {
		if got := launchSpelling(tc.launch, checkout); got != tc.want {
			t.Errorf("%s: launchSpelling(%q) = %q, want %q", name, tc.launch, got, tc.want)
		}
	}
}

// A supervisor reaches a server on another machine over plain http only when told to; the refusal comes before anything is sent.
func TestSupervisorRefusesPlainHTTPToAnotherMachineUnlessAllowed(t *testing.T) {
	t.Parallel()
	client := &Client{MachineID: "mch_x", MachineKey: "key", ServerURL: "http://203.0.113.9:9897"}
	err := client.RunOnce(t.Context())
	if err == nil || !strings.Contains(err.Error(), "plain http") || !strings.Contains(err.Error(), "--insecure") {
		t.Fatalf("a plain-http remote origin must be refused: %v", err)
	}
	// Told to allow it, it goes on to dial (and fails here because nothing listens): not with the refusal.
	client.AllowPlainHTTP = true
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := client.RunOnce(ctx); err != nil && strings.Contains(err.Error(), "plain http to another machine") {
		t.Fatalf("an allowed origin is still refused: %v", err)
	}
}
