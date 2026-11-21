package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

const interruptTurnID = "turn-int"

// interruptClient runs one turn of the agent "agent" (generation 2, desired active) under a client with a journal, the way a delivery leaves it.
func interruptClient(t *testing.T, turn adapter.Turn, info activeTurn) (*Client, *Journal, chan struct{}) {
	t.Helper()
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: interruptTurnID, AgentID: "agent", Generation: 2, Mode: string(info.mode), SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	info.turnID = interruptTurnID
	client.desired["agent"] = protocol.DesiredAgent{AgentID: "agent", Generation: 2, Desired: "active", DesiredRev: 3}
	client.active["agent"] = interruptTurnID
	client.running["agent"] = turn
	client.noteActiveTurn("agent", info)
	client.turns.Add(1)
	go client.runTurn(ctx, protocol.Delivery{AgentID: "agent", Generation: 2, TurnID: interruptTurnID, Mode: info.mode, Agent: adapter.AgentSpec{Tool: "fake"}}, turn)
	done := make(chan struct{})
	go func() { client.turns.Wait(); close(done) }()
	return client, journal, done
}

func hangingTurn(t *testing.T, mode adapter.Mode) adapter.Turn {
	t.Helper()
	fake := adapter.NewFake()
	fake.Script(interruptTurnID, adapter.Step{Kind: adapter.StepHang})
	turn, err := fake.Start(context.Background(), adapter.TurnSpec{TurnID: interruptTurnID, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = turn.Cancel() })
	return turn
}

func interruptFrame() protocol.Frame {
	return protocol.Frame{V: protocol.Version, Type: protocol.TurnInterrupt, ReqID: "req", AgentID: "agent", Generation: 2, TurnID: interruptTurnID, Reason: "test"}
}

func waitEnded(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(waitGuard):
		t.Fatal("the turn never ended")
	}
}

// An interrupted turn ends as interrupted, not cancelled, and its agent is neither stopped nor left busy.
func TestTurnInterruptEndsTheTurnAsInterrupted(t *testing.T) {
	client, journal, done := interruptClient(t, hangingTurn(t, adapter.ModeNormal), activeTurn{mode: adapter.ModeNormal})
	if result := client.handleTurnInterrupt(interruptFrame()); result != "ok" {
		t.Fatalf("result=%q", result)
	}
	if result := client.handleTurnInterrupt(interruptFrame()); result != "ok" {
		t.Fatalf("a repeat must succeed: %q", result)
	}
	waitEnded(t, done)
	exit := pendingExit(t, journal)
	if !exit.Interrupted || exit.CancelIntent || exit.TerminalEvent || exit.ErrorEvent != turnInterruptedOnRequest || exit.ExitCode != 130 {
		t.Fatalf("exit=%+v", exit)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.active["agent"] != "" || client.desired["agent"].Desired != "active" || client.turnInterrupts["agent"] != nil {
		t.Fatalf("active=%q desired=%+v interrupts=%v", client.active["agent"], client.desired["agent"], client.turnInterrupts)
	}
}

func TestTurnInterruptRefusals(t *testing.T) {
	client, _, _ := interruptClient(t, hangingTurn(t, adapter.ModeCorrection), activeTurn{mode: adapter.ModeCorrection})
	other := interruptFrame()
	other.TurnID = "turn-other"
	stale := interruptFrame()
	stale.Generation = 1
	unknown := interruptFrame()
	unknown.AgentID = "nobody"
	for name, item := range map[string]struct {
		frame protocol.Frame
		want  string
	}{"another turn": {other, "not_running"}, "stale generation": {stale, "not_running"}, "unknown agent": {unknown, "not_running"}, "correction turn": {interruptFrame(), "unsupported_mode"}} {
		if result := client.handleTurnInterrupt(item.frame); result != item.want {
			t.Fatalf("%s: result=%q want %q", name, result, item.want)
		}
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.turnInterrupts) != 0 {
		t.Fatalf("a refused interrupt left a mark: %v", client.turnInterrupts)
	}
}

// A stop that follows the interrupt wins: the exit is the stop's cancel.
func TestStopWinsOverTurnInterrupt(t *testing.T) {
	gate := make(chan struct{})
	turn := &terminableTurn{events: make(chan adapter.Event), done: make(chan struct{}), gate: gate}
	client, journal, done := interruptClient(t, turn, activeTurn{mode: adapter.ModeNormal})
	if result := client.handleTurnInterrupt(interruptFrame()); result != "ok" {
		t.Fatalf("result=%q", result)
	}
	client.handleControl(protocol.Frame{AgentID: "agent", Generation: 2, DesiredState: "stopped", DesiredRev: 4})
	if result := client.handleTurnInterrupt(interruptFrame()); result != "ok" {
		t.Fatalf("an interrupt of a stopping turn: %q", result)
	}
	close(gate)
	waitEnded(t, done)
	if exit := pendingExit(t, journal); !exit.CancelIntent || exit.Interrupted {
		t.Fatalf("exit=%+v", exit)
	}
}

// An answer that was in before the process was told to end wins: the turn is terminal, not interrupted.
func TestAnswerBeforeTurnInterruptWins(t *testing.T) {
	turn := &terminableTurn{events: make(chan adapter.Event), done: make(chan struct{}), terminal: true}
	client, journal, done := interruptClient(t, turn, activeTurn{mode: adapter.ModeNormal})
	if result := client.handleTurnInterrupt(interruptFrame()); result != "ok" {
		t.Fatalf("result=%q", result)
	}
	waitEnded(t, done)
	if exit := pendingExit(t, journal); !exit.TerminalEvent || exit.Interrupted {
		t.Fatalf("exit=%+v", exit)
	}
	if result := client.handleTurnInterrupt(interruptFrame()); result != "not_running" {
		t.Fatalf("an ended turn: %q", result)
	}
}

// terminableTurn is a turn whose process ends when it is cancelled or told to terminate, like a harness in a container.
type terminableTurn struct {
	mu         sync.Mutex
	events     chan adapter.Event
	done       chan struct{}
	gate       chan struct{} // when set, the process ends only once it is closed
	ended      bool
	exit       adapter.ExitInfo
	terminal   bool // the answer was already in when the signal came
	confirms   bool // what Terminate answers
	termCode   int
	terminated int
}

func (t *terminableTurn) Events() <-chan adapter.Event { return t.events }
func (t *terminableTurn) end(code int) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	t.ended = true
	t.exit = adapter.ExitInfo{ExitCode: code, CancelIntent: true, TerminalEvent: t.terminal}
	gate := t.gate
	t.mu.Unlock()
	go func() {
		if gate != nil {
			<-gate
		}
		close(t.events)
		close(t.done)
	}()
}
func (t *terminableTurn) Cancel() error { t.end(137); return nil }
func (t *terminableTurn) Terminate(time.Duration) bool {
	t.mu.Lock()
	t.terminated++
	t.mu.Unlock()
	t.end(t.termCode)
	<-t.done
	return t.confirms
}
func (t *terminableTurn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit, nil
}

// A turn in a container whose launcher confirms the end is told to terminate; a confirmed end frees the agent at once.
func TestTurnInterruptTerminatesAContainerTurn(t *testing.T) {
	turn := &terminableTurn{events: make(chan adapter.Event), done: make(chan struct{}), confirms: true, termCode: 143}
	client, journal, done := interruptClient(t, turn, activeTurn{mode: adapter.ModeNormal, containerized: true, confirmsEnd: true})
	client.kaQuarantine = time.Hour // an unconfirmed end would hold the turn far past the test
	if result := client.handleTurnInterrupt(interruptFrame()); result != "ok" {
		t.Fatalf("result=%q", result)
	}
	waitEnded(t, done)
	if exit := pendingExit(t, journal); !exit.Interrupted || exit.CancelIntent || exit.ExitCode != 143 {
		t.Fatalf("exit=%+v", exit)
	}
	if turn.terminated != 1 {
		t.Fatalf("terminate calls=%d", turn.terminated)
	}
}

// An end in a container that is not confirmed holds the agent's admission slot: the turn is not reported ended until the quarantine is over.
func TestUnconfirmedContainerInterruptHoldsTheSlot(t *testing.T) {
	for name, info := range map[string]activeTurn{
		"terminate not confirmed":  {mode: adapter.ModeNormal, containerized: true, confirmsEnd: true},
		"launcher confirms no end": {mode: adapter.ModeNormal, containerized: true},
	} {
		t.Run(name, func(t *testing.T) {
			turn := &terminableTurn{events: make(chan adapter.Event), done: make(chan struct{}), termCode: 137}
			client, journal, done := interruptClient(t, turn, info)
			client.kaQuarantine = 400 * time.Millisecond
			began := time.Now()
			if result := client.handleTurnInterrupt(interruptFrame()); result != "ok" {
				t.Fatalf("result=%q", result)
			}
			<-turn.done
			time.Sleep(100 * time.Millisecond)
			client.mu.Lock()
			held := client.active["agent"]
			client.mu.Unlock()
			if exits, err := journal.PendingExits(context.Background()); err != nil || len(exits) != 0 || held != interruptTurnID {
				t.Fatalf("the slot was not held: active=%q exits=%+v %v", held, exits, err)
			}
			waitEnded(t, done)
			if waited := time.Since(began); waited < 400*time.Millisecond {
				t.Fatalf("the hold ended after %s", waited)
			}
			if exit := pendingExit(t, journal); !exit.Interrupted || exit.CancelIntent {
				t.Fatalf("exit=%+v", exit)
			}
			if (turn.terminated == 1) != info.confirmsEnd {
				t.Fatalf("terminate calls=%d", turn.terminated)
			}
		})
	}
}

// A stop during the hold ends it, and the exit is then the stop's.
func TestStopEndsTheUnconfirmedInterruptHold(t *testing.T) {
	turn := &terminableTurn{events: make(chan adapter.Event), done: make(chan struct{}), termCode: 137}
	client, journal, done := interruptClient(t, turn, activeTurn{mode: adapter.ModeNormal, containerized: true, confirmsEnd: true})
	client.kaQuarantine = time.Hour
	if result := client.handleTurnInterrupt(interruptFrame()); result != "ok" {
		t.Fatalf("result=%q", result)
	}
	<-turn.done
	time.Sleep(50 * time.Millisecond)
	client.handleControl(protocol.Frame{AgentID: "agent", Generation: 2, DesiredState: "stopped", DesiredRev: 4})
	waitEnded(t, done)
	if exit := pendingExit(t, journal); !exit.CancelIntent || exit.Interrupted {
		t.Fatalf("exit=%+v", exit)
	}
}

// The supervisor announces the capability and answers the frame on its connection.
func TestTurnInterruptFrameIsAnsweredOverTheConnection(t *testing.T) {
	frames := make(chan protocol.Frame, 8)
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
		frames <- hello
		missing := interruptFrame()
		missing.ReqID, missing.TurnID = "req-missing", "turn-gone"
		for _, frame := range []protocol.Frame{
			{V: protocol.Version, Type: protocol.HelloAck, Desired: []protocol.DesiredAgent{{AgentID: "agent", Generation: 2, Desired: "active", DesiredRev: 3}}},
			missing, interruptFrame(),
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
			if frame.Type == protocol.TurnInterruptResult || frame.Type == protocol.Process {
				frames <- frame
			}
		}
	}))
	defer peer.Close()
	client, _, done := interruptClient(t, hangingTurn(t, adapter.ModeNormal), activeTurn{mode: adapter.ModeNormal})
	client.ServerURL = peer.URL
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- client.RunOnce(ctx) }()
	next := func() protocol.Frame {
		t.Helper()
		select {
		case frame := <-frames:
			return frame
		case <-time.After(waitGuard):
			t.Fatal("no frame")
			return protocol.Frame{}
		}
	}
	if hello := next(); !hello.SupportsTurnInterrupt {
		t.Fatal("the hello does not announce turn interrupts")
	}
	if frame := next(); frame.Type != protocol.TurnInterruptResult || frame.ReqID != "req-missing" || frame.Result != "not_running" {
		t.Fatalf("frame=%+v", frame)
	}
	if frame := next(); frame.Type != protocol.TurnInterruptResult || frame.ReqID != "req" || frame.Result != "ok" || frame.TurnID != interruptTurnID {
		t.Fatalf("frame=%+v", frame)
	}
	if frame := next(); frame.Type != protocol.Process || frame.Exit == nil || !frame.Exit.Interrupted || frame.Exit.CancelIntent {
		t.Fatalf("frame=%+v exit=%+v", frame, frame.Exit)
	}
	waitEnded(t, done)
	cancel()
	select {
	case <-finished:
	case <-time.After(waitGuard):
		t.Fatal("supervisor did not stop")
	}
}
