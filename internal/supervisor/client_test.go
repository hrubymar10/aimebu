package supervisor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
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
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not connect")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled supervisor waited for WebSocket close handshake")
	}
}

func TestStopAcceptingAndDrainJournalsInterruptedTurn(t *testing.T) {
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
	go client.runTurn(ctx, protocol.Delivery{AgentID: "agent", TurnID: "turn-drain"}, turn)
	drainCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.StopAcceptingAndDrain(drainCtx); err != nil {
		t.Fatal(err)
	}
	if !client.stopping {
		t.Fatal("supervisor still accepts deliveries")
	}
	exits, err := journal.PendingExits(ctx)
	if err != nil || len(exits) != 1 || exits[0].TurnID != "turn-drain" || !exits[0].Exit.CancelIntent {
		t.Fatalf("journaled exits=%+v %v", exits, err)
	}
}

type stubbornTurn struct {
	events chan adapter.Event
	done   chan struct{}
}

func (t *stubbornTurn) Events() <-chan adapter.Event { return t.events }
func (t *stubbornTurn) Cancel() error                { return nil }
func (t *stubbornTurn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	return adapter.ExitInfo{}, nil
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
