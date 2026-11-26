package server

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A process exit for a turn the server has already ended in the other state is refused with its own code, already_terminal, and not the
// general invalid_transition: the code is what tells a supervisor's journal the turn cannot change any more, so the refused exit is settled
// instead of resent forever (the invalid_transition variant of the A14-4 drain bug). The refusal is the protocol working: the connection
// stays up and the same frame is refused the same way again.
func TestTerminalMismatchExitIsRefusedAsAlreadyTerminal(t *testing.T) {
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	// The server's own record ends the turn as killed (a stop reached the store first); the supervisor's journal still holds the exit
	// as a plain exit and replays it.
	ctx := context.Background()
	if err := f.db.RecordProcessInfo(ctx, turn.ID, agent.ID, turn.Generation, adapter.ExitInfo{ExitCode: 137, CancelIntent: true}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: agent.ID, TurnID: turn.ID, Generation: turn.Generation,
		Exit: &adapter.ExitInfo{ExitCode: 0}})
	readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if err := conn.Write(readCtx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		_, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("no answer to the mismatched process frame: %v", err)
		}
		var answer protocol.Frame
		if err := json.Unmarshal(data, &answer); err != nil {
			t.Fatal(err)
		}
		if answer.Type != protocol.Error || answer.Code != "already_terminal" || answer.AgentID != agent.ID || answer.TurnID != turn.ID {
			t.Fatalf("mismatched process frame answer=%+v, want error frame already_terminal for the turn", answer)
		}
	}
}
