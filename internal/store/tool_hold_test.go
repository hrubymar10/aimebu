package store

import (
	"context"
	"testing"
	"time"
)

func TestAToolHoldKeepsNewTurnsQueuedOnTheHostUntilReleased(t *testing.T) {
	s, _, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "go"}); err != nil {
		t.Fatal(err)
	}
	var tool string
	if err := s.db.QueryRow(`SELECT tool FROM agent WHERE id=?`, agent.ID).Scan(&tool); err != nil {
		t.Fatal(err)
	}
	s.SetToolHold("some-other-tool", "x")
	if s.ToolHold(tool) != "" {
		t.Fatal("a hold on another tool must not hold this one")
	}
	s.SetToolHold(tool, "approaching the usage limit")
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
		t.Fatalf("held tool dispatched: ready=%t err=%v", ready, err)
	}
	s.SetToolHold(tool, "")
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || !ready {
		t.Fatalf("the queued note was not dispatched after release: ready=%t err=%v", ready, err)
	}
}

func TestGlobalSettingValueRoundTrips(t *testing.T) {
	s, _, _, _, _ := wakeFixture(t)
	ctx := context.Background()
	if _, ok, err := s.GlobalSettingValue(ctx, "k"); err != nil || ok {
		t.Fatalf("missing key: ok=%t err=%v", ok, err)
	}
	for _, v := range []string{"1", "2"} {
		if err := s.SetGlobalSettingValue(ctx, "k", v); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := s.GlobalSettingValue(ctx, "k"); err != nil || !ok || got != v {
			t.Fatalf("got %q ok=%t err=%v", got, ok, err)
		}
	}
}

func TestAToolHoldDoesNotStopAgentsOnARemoteMachine(t *testing.T) {
	s, _, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "go"}); err != nil {
		t.Fatal(err)
	}
	var tool, machineID string
	if err := s.db.QueryRow(`SELECT tool,machine_id FROM agent WHERE id=?`, agent.ID).Scan(&tool, &machineID); err != nil {
		t.Fatal(err)
	}
	var host int
	if err := s.db.QueryRow(`SELECT is_host FROM machine WHERE id=?`, machineID).Scan(&host); err != nil || host != 1 {
		t.Fatalf("the fixture agent is expected to run on the host: is_host=%d %v", host, err)
	}
	s.SetToolHold(tool, "approaching the usage limit")
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
		t.Fatalf("a held tool must not dispatch on the host: ready=%t err=%v", ready, err)
	}
	// The same agent on a remote machine (its own login, its own quota) is not held by the host's account.
	if _, err := s.db.Exec(`UPDATE machine SET is_host=0 WHERE id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || !ready {
		t.Fatalf("a remote-machine agent must still be dispatched: ready=%t err=%v", ready, err)
	}
}

// An account switch holds new turns of the tool on the host in a hold of its own (the limit guard's verdict is set and cleared on its own schedule),
// counts nested holds, releases only its own, and leaves other tools alone.
func TestHoldDispatchKeepsNewTurnsQueuedUntilReleased(t *testing.T) {
	s, _, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "go"}); err != nil {
		t.Fatal(err)
	}
	var tool string
	if err := s.db.QueryRow(`SELECT tool FROM agent WHERE id=?`, agent.ID).Scan(&tool); err != nil {
		t.Fatal(err)
	}
	other := s.HoldDispatch("some-other-tool")
	first := s.HoldDispatch(tool)
	second := s.HoldDispatch(tool)
	s.SetToolHold(tool, "") // the guard clearing its own verdict must not lift a switch's hold
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
		t.Fatalf("a switch's hold did not hold: ready=%t err=%v", ready, err)
	}
	first()
	first() // a second release of the same hold changes nothing
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
		t.Fatalf("a nested hold was released by another's release: ready=%t err=%v", ready, err)
	}
	second()
	other()
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || !ready {
		t.Fatalf("the queued note was not dispatched after the switch: ready=%t err=%v", ready, err)
	}
}

// Taking the hold waits for the transactions that are open: a turn that was being dispatched is recorded by the time HoldDispatch returns.
func TestHoldDispatchWaitsForOpenTransactions(t *testing.T) {
	s, _, _, _, _ := wakeFixture(t)
	opened, finish, held := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = s.Tx(context.Background(), func(tx *Tx) error {
			close(opened)
			<-finish
			return nil
		})
	}()
	<-opened
	go func() {
		s.HoldDispatch("claude")()
		close(held)
	}()
	select {
	case <-held:
		t.Fatal("the hold returned while a transaction was open")
	case <-time.After(150 * time.Millisecond):
	}
	close(finish)
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the hold never returned")
	}
}
