package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A drain whose only debt is an exit the server refuses with already_terminal (the server's turn is already terminal in the other state:
// killed where the journal says exited) settles that exit when the refusal arrives, empties the journal within this single connection, and
// removes it — the same drain-bug class as the stale_generation refusal, by its own code.
func TestDrainSettlesAnAlreadyTerminalExitAndEmptiesTheJournal(t *testing.T) {
	peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	journal := staleExitJournal(t)
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	c.DrainOnly = true
	logged := captureLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- c.Run(ctx) }()
	exit := peer.next(t, string(protocol.Process))
	if exit.TurnID != "turn_stale" || exit.AgentID != "agent_x" || exit.Exit == nil {
		t.Fatalf("the drain did not replay the refused exit as it was journaled: %+v", exit)
	}
	// The server's answer (internal/server/machine.go): the refused frame's agent and turn, the code, never the frame type.
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: exit.AgentID, TurnID: exit.TurnID, Code: "already_terminal", Message: "turn.exec \"killed\" -> \"exited\""}
	select {
	case <-finished:
	case <-time.After(12 * time.Second):
		t.Fatal("the drain did not finish after the already-terminal exit was settled")
	}
	exits, err := journal.PendingExits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(exits) != 0 {
		t.Fatalf("the exit is still journaled after the already_terminal refusal: %+v", exits)
	}
	if journal.Owes(context.Background()) {
		t.Fatal("the journal still owes something; the drain could not empty it")
	}
	if n := strings.Count(logged(), "the server refused the late exit with already_terminal"); n != 1 {
		t.Fatalf("the settle was logged %d times, want once: %s", n, logged())
	}
	removed, err := c.FinishRetire()
	if err != nil || !removed {
		t.Fatalf("the emptied journal was not removed: removed=%v err=%v", removed, err)
	}
}

// A plain invalid_transition never settles the exit: the code does not say the turn is over (the server refuses out-of-order answers and
// other transitions of turns that may still be running the same way), so an older server that answers the terminal mismatch with
// invalid_transition alone leaves the exit journaled for the usual paths.
func TestAPlainInvalidTransitionKeepsTheExitJournaled(t *testing.T) {
	peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	journal := staleExitJournal(t)
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	c.DrainOnly = true
	logged := captureLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- peer.ran(c.RunOnce(ctx)) }()
	exit := peer.next(t, string(protocol.Process))
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: exit.AgentID, TurnID: exit.TurnID, Code: "invalid_transition", Message: "turn.exec \"killed\" -> \"exited\""}
	<-finished // the run context ends; the connection was not cut by a settle
	exits, err := journal.PendingExits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(exits) != 1 {
		t.Fatalf("an invalid_transition refusal settled the exit: %+v", exits)
	}
	if strings.Contains(logged(), "the server refused the late exit") {
		t.Fatalf("the settle was logged for a code that does not say the turn is over: %s", logged())
	}
}

// A live turn whose activity frame the server refuses with already_terminal (a code that names no frame) keeps its connection: the settle
// finds no exited row, does nothing, and the turn's own exit is reported and acknowledged normally afterwards.
func TestLiveTurnSurvivesAnAlreadyTerminalErrorFrame(t *testing.T) {
	peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	turn := hangingTurn(t, adapter.ModeNormal)
	client, journal, done := interruptClient(t, turn, activeTurn{mode: adapter.ModeNormal})
	client.ServerURL = peer.server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- peer.ran(client.RunOnce(ctx)) }()
	// The server refuses an activity frame of the running turn; the Error does not say which frame it answered.
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: "agent", TurnID: interruptTurnID, Code: "already_terminal", Message: "agent frame rejected"}
	_ = turn.Cancel()
	waitEnded(t, done)
	exit := peer.next(t, string(protocol.Process))
	if exit.TurnID != interruptTurnID || exit.Generation != 2 {
		t.Fatalf("the live turn's own exit was not reported: %+v", exit)
	}
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Ack, Level: "process", TurnID: exit.TurnID}
	deadline := time.Now().Add(5 * time.Second)
	for {
		exits, err := journal.PendingExits(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(exits) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live turn's exit was not acknowledged: %+v", exits)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-finished:
		t.Fatalf("the connection was cut by the already_terminal refusal of a live turn: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}
