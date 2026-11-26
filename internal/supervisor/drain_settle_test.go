package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestStopAcceptingAndDrainTimeoutSettlesRunningTurns(t *testing.T) {
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
	go client.runTurn(context.Background(), protocol.Delivery{AgentID: "agent", TurnID: "turn-stuck", Mode: adapter.ModeNormal}, turn)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.StopAcceptingAndDrain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain timeout=%v", err)
	}
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 || exits[0].TurnID != "turn-stuck" || exits[0].Exit.ErrorEvent != "supervisor_lost" {
		t.Fatalf("the drain timeout left the turn without a journaled exit: exits=%+v err=%v", exits, err)
	}
	agents, err := journal.OpenTurns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		for _, open := range agent.OpenTurns {
			if open.TurnID == "turn-stuck" && open.Exec != "exited" {
				t.Fatalf("the journal still lists the settled turn as %q", open.Exec)
			}
		}
	}
	close(turn.events)
	close(turn.done)
	client.turns.Wait()
}
