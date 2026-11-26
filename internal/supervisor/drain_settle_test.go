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

// failIdentityRead renames the column the identity read selects, so RunningTurnProcesses fails while an exit write does not touch it.
func failIdentityRead(t *testing.T, journal *Journal) {
	t.Helper()
	if _, err := journal.db.Exec(`ALTER TABLE turn_start RENAME COLUMN pid_start TO pid_start_renamed`); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RunningTurnProcesses(context.Background()); err == nil {
		t.Fatal("the identity read did not fail")
	}
}

func settleWithFailedRead(t *testing.T, turnID string, pid int, start string) *Journal {
	t.Helper()
	oldGrace := drainSettleGrace
	drainSettleGrace = 0
	t.Cleanup(func() { drainSettleGrace = oldGrace })
	journal := openTestJournal(t)
	recordRunningTurn(t, journal, turnID, pid, pid, start)
	c := NewClient("machine", "key", "http://unused.invalid", journal, adapter.NewFake())
	c.active["agent_x"] = turnID
	// Even a process proved gone must not be settled: the read failed, so nothing is known.
	c.ProcessStartState = func(int) (ProcessStartState, string) { return StartGone, "" }
	failIdentityRead(t, journal)
	c.settleDrainTimeout()
	return journal
}

// An identified row survives a failed identity read: no exit is written, and a later drain settles it.
func TestDrainTimeoutKeepsAnIdentifiedRowWhenTheIdentityReadFails(t *testing.T) {
	journal := settleWithFailedRead(t, "turn-identified", 4242, "recorded")
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 0 {
		t.Fatalf("the failed identity read wrote an exit for an identified turn: exits=%+v err=%v", exits, err)
	}
}

// A failed identity read defers every row, legacy ones included, to a later drain.
func TestDrainTimeoutWritesNoRowWhenTheIdentityReadFails(t *testing.T) {
	journal := settleWithFailedRead(t, "turn-legacy", 0, "")
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 0 {
		t.Fatalf("the failed identity read wrote an exit for a legacy turn: exits=%+v err=%v", exits, err)
	}
}
