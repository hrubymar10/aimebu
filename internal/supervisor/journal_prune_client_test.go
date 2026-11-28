package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// startCounter counts the processes the supervisor asks the adapter to start.
type startCounter struct {
	adapter.Adapter
	starts atomic.Int32
}

func (s *startCounter) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	s.starts.Add(1)
	return s.Adapter.Start(ctx, spec)
}

// A delivery replayed after its turn settled and was pruned must be acknowledged as known and start nothing; a delivery of another generation
// for that turn is a conflict.
func TestRedeliveryOfAPrunedTurnStartsNoProcessAndOtherGenerationConflicts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const agentID, turnID = "agt_pruned", "turn-pruned"
	peer := newMachinePeer(t, activeAck(agentID))
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	fake := adapter.NewFake()
	fake.Script(turnID, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	counter := &startCounter{Adapter: fake}
	c := NewClient("machine", "key", peer.server.URL, j, counter)
	c.OnError = func(err error) { t.Log("onerror:", err) }
	if err := c.setAgentKey(agentID, "aak_"+strings.Repeat("s", 43)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- peer.ran(c.RunOnce(ctx)) }()

	delivery := protocol.Delivery{AgentID: agentID, TurnID: turnID, Generation: 0, DesiredRev: 0, Mode: adapter.ModeNormal,
		Agent: adapter.AgentSpec{ID: agentID, Tool: "fake", Model: "test"},
		Notes: []protocol.DeliveryNote{{NoteID: "note-1", RoomID: "room", MessageID: "msg", Body: "task", From: "owner"}}}
	peer.out <- protocol.DeliverFrame(delivery)
	if process := peer.next(t, protocol.Process); process.TurnID != turnID {
		t.Fatalf("process=%+v", process)
	}
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Ack, TurnID: turnID, Level: "process"}
	// Settled: acknowledged, then pruned (the grace is zero here).
	for deadline := time.Now().Add(10 * time.Second); ; {
		if r, err := j.PruneSettled(ctx, 0); err != nil {
			t.Fatal(err)
		} else if r.Turns == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the settled turn was never pruned")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if spec, err := j.TurnSpecJSON(ctx, turnID); err != nil || len(spec) != 0 {
		t.Fatalf("spec was not pruned: %q %v", spec, err)
	}
	if counter.starts.Load() != 1 {
		t.Fatalf("starts=%d before the replay", counter.starts.Load())
	}

	peer.out <- protocol.DeliverFrame(delivery)
	accepted := peer.next(t, protocol.Ack)
	for accepted.Level != "accepted" {
		accepted = peer.next(t, protocol.Ack)
	}
	if accepted.TurnID != turnID || accepted.Generation != 0 {
		t.Fatalf("ack=%+v", accepted)
	}
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Ping}
	peer.next(t, protocol.Pong)
	select {
	case err := <-done:
		t.Fatalf("a replay of a pruned turn dropped the connection: %v", err)
	default:
	}
	if got := counter.starts.Load(); got != 1 {
		t.Fatalf("the replay started a process: starts=%d", got)
	}

	other := delivery
	other.Generation = 2
	peer.out <- protocol.DeliverFrame(other)
	select {
	case err := <-done:
		if !errors.Is(err, ErrJournalConflict) {
			t.Fatalf("another generation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("another generation of a pruned turn was not refused")
	}
	if got := counter.starts.Load(); got != 1 {
		t.Fatalf("starts=%d", got)
	}
}
