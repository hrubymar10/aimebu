package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// turnInterruptAnswerWait is how long the server waits for a supervisor's answer to a turn_interrupt frame: the supervisor only signals the
// turn's process before it answers, so an answer that takes longer than this is not coming.
const turnInterruptAnswerWait = 10 * time.Second

// errTurnInterruptUnsupported is a machine whose supervisor does not announce supports_turn_interrupt.
var errTurnInterruptUnsupported = errors.New("the supervisor on that machine cannot interrupt a turn without stopping its agent (it does not report supports_turn_interrupt)")

// machineInterruptsTurns says whether the machine is connected and its supervisor cuts one turn on a turn_interrupt frame.
func (s *Server) machineInterruptsTurns(machineID string) bool {
	conn, err := s.machineForID(machineID)
	return err == nil && conn.supportsTurnInterrupt
}

// interruptTurn sends the turn_interrupt frame for a request the store has recorded (store.RequestTurnInterrupt) and returns the
// supervisor's answer: "ok", "not_running" or "unsupported_mode". An error says the frame could not be sent or was not answered; the
// supervisor may still have applied it. The turn's end arrives as its process frame either way.
func (s *Server) interruptTurn(ctx context.Context, target store.TurnInterruptTarget, reason string) (string, error) {
	conn, err := s.machineForID(target.MachineID)
	if err != nil {
		return "", err
	}
	if !conn.supportsTurnInterrupt {
		return "", errTurnInterruptUnsupported
	}
	frame := protocol.Frame{V: protocol.Version, Type: protocol.TurnInterrupt, ReqID: newRequestNonce(), AgentID: target.AgentID,
		Generation: target.Generation, TurnID: target.TurnID, Reason: reason}
	channel := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	if conn.pathPending == nil {
		conn.pathPending = make(map[string]chan protocol.Frame)
	}
	conn.pathPending[frame.ReqID] = channel
	conn.harnessMu.Unlock()
	defer func() {
		conn.harnessMu.Lock()
		delete(conn.pathPending, frame.ReqID)
		conn.harnessMu.Unlock()
	}()
	if err := conn.send(frame); err != nil {
		return "", err
	}
	wait := s.turnInterruptWait
	if wait <= 0 {
		wait = turnInterruptAnswerWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case result := <-channel:
		return result.Result, nil
	case <-conn.done:
		return "", fmt.Errorf("%w: supervisor disconnected before it answered the turn interrupt", ErrMachineUnavailable)
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", fmt.Errorf("%w: supervisor did not answer the turn interrupt", ErrSlowMachine)
	}
}
