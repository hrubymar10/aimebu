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

func TestDrainTimeoutPreservesUnverifiedIdentifiedTurns(t *testing.T) {
	oldGrace := drainSettleGrace
	drainSettleGrace = 0
	t.Cleanup(func() { drainSettleGrace = oldGrace })
	for _, tc := range []struct {
		name  string
		pid   int
		state ProcessStartState
		start string
		want  bool
	}{
		{"alive", 4242, StartKnown, "recorded", false},
		{"unknown", 4242, StartUnknown, "", false},
		{"gone", 4242, StartGone, "", true},
		{"legacy", 0, StartUnknown, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := openTestJournal(t)
			recordRunningTurn(t, journal, "turn-timeout", tc.pid, tc.pid, "recorded")
			c := NewClient("machine", "key", "http://unused.invalid", journal, adapter.NewFake())
			c.active["agent_x"] = "turn-timeout"
			c.ProcessStartState = func(int) (ProcessStartState, string) { return tc.state, tc.start }
			c.settleDrainTimeout()
			turns, err := journal.RunningTurnProcesses(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (len(turns) == 0) != tc.want {
				t.Fatalf("settled=%v, want %v; running=%+v", len(turns) == 0, tc.want, turns)
			}
		})
	}
}

func TestDrainTimeoutWithNoTurnsSkipsGrace(t *testing.T) {
	oldGrace := drainSettleGrace
	drainSettleGrace = 300 * time.Millisecond
	t.Cleanup(func() { drainSettleGrace = oldGrace })
	c := NewClient("machine", "key", "http://unused.invalid", openTestJournal(t), adapter.NewFake())
	start := time.Now()
	c.settleDrainTimeout()
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Fatalf("empty drain waited for grace: %s", elapsed)
	}
}

// When the identity read fails at the drain timeout, the drain falls back to the per-id lost-exit write, so legacy rows are still settled.
func TestDrainTimeoutSettlesLegacyRowsWhenTheIdentityReadFails(t *testing.T) {
	oldGrace := drainSettleGrace
	drainSettleGrace = 0
	t.Cleanup(func() { drainSettleGrace = oldGrace })
	journal := openTestJournal(t)
	recordRunningTurn(t, journal, "turn-legacy", 0, 0, "")
	c := NewClient("machine", "key", "http://unused.invalid", journal, adapter.NewFake())
	c.active["agent_x"] = "turn-legacy"
	// The identity read selects pid_start; renaming the column makes it fail while the exit write does not touch it.
	if _, err := journal.db.Exec(`ALTER TABLE turn_start RENAME COLUMN pid_start TO pid_start_renamed`); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RunningTurnProcesses(context.Background()); err == nil {
		t.Fatal("the identity read did not fail")
	}
	c.settleDrainTimeout()
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 || exits[0].TurnID != "turn-legacy" || exits[0].Exit.ErrorEvent != "supervisor_lost" {
		t.Fatalf("the failed identity read left the legacy turn unsettled: exits=%+v err=%v", exits, err)
	}
}
