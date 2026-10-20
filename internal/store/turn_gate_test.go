package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A turn that starts after the switcher's quiescence check must not slip in before the swap it protects: while the gate is
// held, recording a turn and acknowledging one as running both wait, and neither changes what the check would see.
func TestHoldTurnsDefersTurnCreationAndStart(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	running := func() bool {
		agents, err := s.SwitcherAgents(ctx, "mch_test", "codex")
		if err != nil || len(agents) != 1 {
			t.Fatalf("agents = %+v, %v", agents, err)
		}
		return agents[0].Running
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if running() {
		t.Fatal("the fixture agent should be quiet before the gate is taken")
	}
	release := s.HoldTurns()
	recorded := make(chan error, 1)
	go func() {
		_, err := s.RecordTurn(ctx, Turn{AgentID: f.agent.ID, SessionID: "ses_test", Generation: 0, RoomID: f.room.ID, Attempt: 2, Mode: "normal", Exec: "pending"})
		recorded <- err
	}()
	select {
	case err := <-recorded:
		t.Fatalf("a turn was recorded while the gate was held (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	if running() {
		t.Fatal("the quiescence check changed while the gate was held")
	}
	release()
	select {
	case err := <-recorded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn was never recorded after the gate was released")
	}
	if !running() {
		t.Fatal("the recorded turn did not count as running after release")
	}
}

// A holder that breaks the rule and opens a transaction does not hang the store: the transaction gives up with
// ErrTurnGateHeld, the hold ends normally and transactions work again.
func TestATransactionOnTheHoldingPathFailsInsteadOfHanging(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	s.turnGateWait = 50 * time.Millisecond
	release := s.HoldTurns()
	start := time.Now()
	err := s.Tx(context.Background(), func(*Tx) error { return nil })
	if !errors.Is(err, ErrTurnGateHeld) {
		t.Fatalf("Tx under the hold = %v, want ErrTurnGateHeld", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the transaction took far longer than the wait bound")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Tx(ctx, func(*Tx) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context must stop the wait: %v", err)
	}
	release()
	if err := s.Tx(context.Background(), func(*Tx) error { return nil }); err != nil {
		t.Fatalf("Tx after the hold ended: %v", err)
	}
}
