package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// remoteStubTurn is a turn that does not end when the supervisor's context is cancelled, like a real harness process that is a child of no
// one the supervisor waits for: only Cancel (the drain's interrupt) ends it.
type remoteStubTurn struct {
	streamClosed sync.Once
	doneClosed   sync.Once
	done         chan struct{}
	stream       chan adapter.Event
}

func (t *remoteStubTurn) Events() <-chan adapter.Event {
	t.streamClosed.Do(func() { close(t.stream) }) // the harness stream ends at once: the turn is only here so its exit write runs
	return t.stream
}

func (t *remoteStubTurn) Cancel() error {
	t.doneClosed.Do(func() { close(t.done) })
	return nil
}
func (t *remoteStubTurn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	return adapter.ExitInfo{ExitCode: 143, CancelIntent: true}, nil
}

// remoteStubAdapter starts exactly one turn that ignores the supervisor's context.
type remoteStubAdapter struct {
	started chan struct{}
	turn    *remoteStubTurn
}

func (a *remoteStubAdapter) Start(context.Context, adapter.TurnSpec) (adapter.Turn, error) {
	select {
	case <-a.started:
	default:
		close(a.started)
	}
	return a.turn, nil
}
func (a *remoteStubAdapter) Catalog() []adapter.CatalogEntry {
	return []adapter.CatalogEntry{{Tool: "stubborn", Version: "test", Environment: adapter.Environment{Variant: "test"}}}
}
func (a *remoteStubAdapter) Capabilities(string, adapter.Environment) adapter.Capabilities {
	return adapter.Capabilities{}
}

// TestRemoteSupervisorDrainsOnExit runs a remote supervisor (not --local) against a fake server that delivers one turn which ignores the
// supervisor's context, and cancels the run context the way SIGTERM does (main's signal context). The supervisor must drain like a local
// one: the turn is interrupted and its exit is journaled before the process exits, so the journal never keeps a running row of a supervisor
// that is gone.
func TestRemoteSupervisorDrainsOnExit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_SENTRY_DSN", "off")
	if err := os.MkdirAll(filepath.Join(root, "supervisor"), 0o700); err != nil {
		t.Fatal(err)
	}
	credential, err := json.Marshal(hostMachineCredential{MachineID: "machine-test", Key: "key-test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "supervisor", "machine.key"), credential, 0o600); err != nil {
		t.Fatal(err)
	}
	stub := &remoteStubAdapter{started: make(chan struct{}), turn: &remoteStubTurn{done: make(chan struct{}), stream: make(chan adapter.Event)}}
	previousHook := supervisorClientHook
	supervisorClientHook = func(client *supervisor.Client) { client.Adapter = stub }
	t.Cleanup(func() { supervisorClientHook = previousHook })

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		handshaken := false
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var frame protocol.Frame
			if json.Unmarshal(raw, &frame) != nil {
				continue
			}
			if frame.Type != protocol.Hello || handshaken {
				continue
			}
			handshaken = true
			ack := protocol.Frame{V: protocol.Version, Type: protocol.HelloAck, Desired: []protocol.DesiredAgent{{AgentID: "agent-remote", Generation: 1, Desired: "active", DesiredRev: 1}}}
			data, err := json.Marshal(ack)
			if err != nil {
				return
			}
			if conn.Write(ctx, websocket.MessageText, data) != nil {
				return
			}
			delivery := protocol.DeliverFrame(protocol.Delivery{
				AgentID: "agent-remote", Generation: 1, DesiredRev: 1, TurnID: "turn-remote", Mode: adapter.ModeNormal,
				Notes: []protocol.DeliveryNote{{NoteID: "note-1", RoomID: "room", From: "owner", MessageID: "msg-1", Body: "run"}},
				Agent: adapter.AgentSpec{ID: "agent-remote", Tool: "stubborn", Variant: "test", Model: "test", Provider: "stubborn"},
			})
			data, err = json.Marshal(delivery)
			if err != nil {
				return
			}
			if conn.Write(ctx, websocket.MessageText, data) != nil {
				return
			}
		}
	}))
	defer peer.Close()
	t.Setenv("AIMEBU_URL", peer.URL)

	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() { code <- supervisorCommand(ctx, false, io.Discard) }()
	select {
	case <-stub.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake server's delivery did not start a turn")
	}
	// The turn goroutine registers the running turn just after the adapter's Start returns; give it that moment before the SIGTERM.
	time.Sleep(250 * time.Millisecond)
	cancel()
	select {
	case got := <-code:
		if got != 0 {
			t.Fatalf("supervisor exit code %d", got)
		}
	case <-time.After(20 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("the remote supervisor did not exit after the signal\n%s", buf[:n])
	}
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(root, "supervisor", "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 || exits[0].TurnID != "turn-remote" || !exits[0].Exit.CancelIntent {
		t.Fatalf("the interrupted turn of a remote supervisor was not journaled: exits=%+v err=%v", exits, err)
	}
	agents, err := journal.OpenTurns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		for _, open := range agent.OpenTurns {
			if open.TurnID == "turn-remote" && open.Exec != "exited" {
				t.Fatalf("the journal still lists the turn as %q after the drain", open.Exec)
			}
		}
	}
}
