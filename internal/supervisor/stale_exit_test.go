package supervisor

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// captureLog collects what the package logs during a test (the settle line goes through the standard log), safe to read later.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var lines strings.Builder
	previous := log.Writer()
	log.SetOutput(&lineWriter{mu: &mu, lines: &lines})
	t.Cleanup(func() { log.SetOutput(previous) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return lines.String()
	}
}

type lineWriter struct {
	mu    *sync.Mutex
	lines *strings.Builder
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lines.Write(p)
}

// staleExitJournal leaves a journal of the confirmed A13-13 shape: one turn of an older generation, exited and never acknowledged by the
// server, and nothing else owed. The tests build it under their own temp dir; no real journal is ever opened.
func staleExitJournal(t *testing.T) *Journal {
	t.Helper()
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "turn_stale", AgentID: "agent_x", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordExit(ctx, "turn_stale", -1, false, false); err != nil {
		t.Fatal(err)
	}
	return journal
}

// A drain whose only debt is an exit the server refuses with stale_generation (the agent moved to a newer generation while the exit was in
// flight) settles that exit when the refusal arrives, empties the journal within this single connection, and removes it: the endless drain
// loop of A13-13 ends on the first cycle.
func TestDrainSettlesAStaleGenerationExitAndEmptiesTheJournal(t *testing.T) {
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
	// Run, as production runs the drain: it is what marks the retirement complete (retired) once a retire request finds nothing owed.
	go func() { finished <- c.Run(ctx) }()
	exit := peer.next(t, string(protocol.Process))
	if exit.TurnID != "turn_stale" || exit.AgentID != "agent_x" || exit.Generation != 0 || exit.Exit == nil || exit.Exit.ExitCode != -1 {
		t.Fatalf("the drain did not replay the stale exit as it was journaled: %+v", exit)
	}
	// The server's answer (internal/server/machine.go): the refused frame's agent and turn, the code, never the frame type.
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: exit.AgentID, TurnID: exit.TurnID, Code: "stale_generation", Message: "agent frame rejected"}
	select {
	case <-finished:
	case <-time.After(12 * time.Second):
		t.Fatal("the drain did not finish after the stale exit was settled")
	}
	exits, err := journal.PendingExits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(exits) != 0 {
		t.Fatalf("the stale exit is still journaled after the stale_generation refusal: %+v", exits)
	}
	if journal.Owes(context.Background()) {
		t.Fatal("the journal still owes something; the drain could not empty it")
	}
	if n := strings.Count(logged(), "the server refused the late exit with stale_generation"); n != 1 {
		t.Fatalf("the settle was logged %d times, want once: %s", n, logged())
	}
	removed, err := c.FinishRetire()
	if err != nil || !removed {
		t.Fatalf("the emptied journal was not removed: removed=%v err=%v", removed, err)
	}
}

// Any other error code keeps the exit journaled: only stale_generation and already_terminal say the turn is over, and a journal whose debt the server has
// not resolved must be finished the usual way (retried, adopted, or lost with recovery).
func TestOtherErrorCodesKeepTheExitJournaled(t *testing.T) {
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
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: exit.AgentID, TurnID: exit.TurnID, Code: "conflict", Message: "conflict"}
	<-finished // the run context ends; the connection was not cut by a settle
	exits, err := journal.PendingExits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(exits) != 1 {
		t.Fatalf("a conflict refusal settled the exit: %+v", exits)
	}
	if strings.Contains(logged(), "the server refused the late exit") {
		t.Fatalf("the settle was logged for a code that is not stale_generation: %s", logged())
	}
}

// A live turn whose activity frame the server refuses with stale_generation (an older instance still running a turn of a superseded
// generation) keeps its connection: the settle finds no exited row, does nothing, and the turn's own exit is reported and acknowledged
// normally afterwards.
func TestLiveTurnSurvivesAStaleGenerationErrorFrame(t *testing.T) {
	peer := newMachinePeer(t, activeAck("agent"))
	turn := hangingTurn(t, adapter.ModeNormal)
	client, journal, done := interruptClient(t, turn, activeTurn{mode: adapter.ModeNormal})
	client.ServerURL = peer.server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- peer.ran(client.RunOnce(ctx)) }()
	// The server refuses an activity frame of the running turn the way it refuses every frame of a superseded generation.
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: "agent", TurnID: interruptTurnID, Code: "stale_generation", Message: "agent frame rejected"}
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
		t.Fatalf("the connection was cut by the stale_generation refusal of a live turn: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}
