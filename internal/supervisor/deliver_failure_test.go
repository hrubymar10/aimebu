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
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

// machinePeer is a scripted server side of the machine socket. Each accepted
// connection answers hello with ack(hello), then forwards supervisor frames
// to in and writes frames from out until drop is signalled.
type machinePeer struct {
	server *httptest.Server
	hellos chan protocol.Frame
	in     chan protocol.Frame
	out    chan protocol.Frame
	drop   chan struct{}
}

func newMachinePeer(t *testing.T, ack func(hello protocol.Frame) protocol.Frame) *machinePeer {
	t.Helper()
	p := &machinePeer{hellos: make(chan protocol.Frame, 8), in: make(chan protocol.Frame, 64), out: make(chan protocol.Frame, 16), drop: make(chan struct{}, 1)}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(protocol.MaxMachineFrameBytes)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		hello, err := readFrame(ctx, conn)
		if err != nil {
			return
		}
		p.hellos <- hello
		raw, _ := json.Marshal(ack(hello))
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			return
		}
		go func() {
			defer cancel()
			for {
				frame, err := readFrame(ctx, conn)
				if err != nil {
					return
				}
				select {
				case p.in <- frame:
				case <-ctx.Done():
					return
				}
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.drop:
				return
			case frame := <-p.out:
				raw, _ := json.Marshal(frame)
				if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *machinePeer) next(t *testing.T, want string) protocol.Frame {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case frame := <-p.in:
			if frame.Type == want {
				return frame
			}
		case <-timeout:
			t.Fatalf("no %q frame from supervisor", want)
		}
	}
}

func activeAck(agentID string) func(protocol.Frame) protocol.Frame {
	return func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck,
			Desired: []protocol.DesiredAgent{{AgentID: agentID, Desired: "active"}}}
	}
}

func testDelivery(agentID, turnID, tool, variant, checkout string) protocol.Frame {
	return protocol.DeliverFrame(protocol.Delivery{AgentID: agentID, TurnID: turnID, Mode: adapter.ModeNormal,
		Agent:        adapter.AgentSpec{ID: agentID, Tool: tool, Variant: variant, Model: "test"},
		CheckoutPath: checkout, Context: json.RawMessage(`{"role_body":"worker"}`),
		Notes: []protocol.DeliveryNote{{NoteID: "note-" + turnID, RoomID: "room", MessageID: "msg", Body: "task", From: "owner"}}})
}

func TestStartFailureIsJournaledAndKeepsMachineConnection(t *testing.T) {
	// Keep NewClient's harness discovery away from any real CLI on PATH.
	t.Setenv("PATH", t.TempDir())
	cases := []struct {
		name, variant string
		adapter       adapter.Adapter
		checkout      func(t *testing.T) string
		exitCode      int
		evidence      string
	}{
		{name: "missing executable", variant: "native", adapter: claude.New(), exitCode: 127, evidence: "missing-claude",
			checkout: func(t *testing.T) string { return t.TempDir() }},
		{name: "unusable docker checkout", variant: "docker", adapter: claude.NewDocker(), exitCode: 1, evidence: ".aimebu",
			checkout: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, ".aimebu"), []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
				return dir
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			const agentID = "agt_start"
			peer := newMachinePeer(t, activeAck(agentID))
			j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			c := NewClient("machine", "key", peer.server.URL, j, tc.adapter)
			c.OnError = func(error) {}
			if err := c.setAgentKey(agentID, "aak_"+strings.Repeat("s", 43)); err != nil {
				t.Fatal(err)
			}
			for i := range c.Harnesses {
				if c.Harnesses[i].Tool == "claude" && c.Harnesses[i].Variant == tc.variant {
					c.Harnesses[i].Available = true
					c.Harnesses[i].LauncherPath = filepath.Join(t.TempDir(), "missing-claude")
				}
			}
			done := make(chan error, 1)
			go func() { done <- c.RunOnce(ctx) }()
			peer.out <- testDelivery(agentID, "turn-start-fail", "claude", tc.variant, tc.checkout(t))
			process := peer.next(t, protocol.Process)
			if process.TurnID != "turn-start-fail" || process.Exit == nil || process.Exit.TerminalEvent ||
				process.Exit.ExitCode != tc.exitCode || !strings.Contains(process.Exit.StderrTail, "harness failed to start") ||
				!strings.Contains(process.Exit.StderrTail, tc.evidence) {
				t.Fatalf("start failure exit=%+v", process.Exit)
			}
			peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Ping}
			peer.next(t, protocol.Pong)
			select {
			case err := <-done:
				t.Fatalf("start failure dropped the machine connection: %v", err)
			default:
			}
			exits, err := j.PendingExits(ctx)
			if err != nil || len(exits) != 1 || exits[0].TurnID != "turn-start-fail" || !strings.Contains(exits[0].Exit.StderrTail, tc.evidence) {
				t.Fatalf("journaled exits=%+v %v", exits, err)
			}
			c.mu.Lock()
			busy := c.active[agentID]
			c.mu.Unlock()
			if busy != "" {
				t.Fatalf("admission slot still held by %q", busy)
			}
			cancel()
			<-done
		})
	}
}

func TestJournalFailureKillsAndWaitsForHarness(t *testing.T) {
	cases := []struct {
		name   string
		inject func(ctx context.Context, j *Journal, turnID string) error
		emit   adapter.Event
	}{
		{name: "event", emit: adapter.Event{Kind: adapter.EventActivity, SourceEventID: "dup", Summary: "working"},
			inject: func(ctx context.Context, j *Journal, turnID string) error {
				return j.RecordEvent(ctx, turnID, "dup", "activity", "something else", json.RawMessage(`{}`))
			}},
		{name: "answer", emit: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)},
			inject: func(ctx context.Context, j *Journal, turnID string) error {
				_, err := j.RecordAnswer(ctx, turnID, 0, "conflicting-digest", json.RawMessage(`{}`))
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			const turnID = "turn-journal-fail"
			if _, err := j.RecordStart(ctx, StartRecord{TurnID: turnID, AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			// A conflicting row makes the turn's own write fail deterministically.
			if err := tc.inject(ctx, j, turnID); err != nil {
				t.Fatal(err)
			}
			fake := adapter.NewFake()
			fake.Script(turnID, adapter.Step{Kind: adapter.StepEmit, Event: tc.emit}, adapter.Step{Kind: adapter.StepHang})
			turn, err := fake.Start(ctx, adapter.TurnSpec{TurnID: turnID, Mode: adapter.ModeNormal})
			if err != nil {
				t.Fatal(err)
			}
			client := NewClient("machine", "key", "http://127.0.0.1:1", j, fake)
			client.OnError = func(error) {}
			client.active["agent"] = turnID
			client.running["agent"] = turn
			client.turns.Add(1)
			finished := make(chan struct{})
			go func() {
				client.runTurn(ctx, protocol.Delivery{AgentID: "agent", TurnID: turnID}, turn)
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				t.Fatal("runTurn did not return after the journal write failed")
			}
			exited := make(chan struct{})
			go func() { _, _ = turn.Wait(); close(exited) }()
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				t.Fatal("harness kept running untracked after the journal write failed")
			}
			exits, err := j.PendingExits(ctx)
			if err != nil || len(exits) != 1 || exits[0].Exit.TerminalEvent || exits[0].Exit.CancelIntent ||
				!strings.Contains(exits[0].Exit.StderrTail, "journal write failed") {
				t.Fatalf("journaled exit=%+v %v", exits, err)
			}
		})
	}
}
