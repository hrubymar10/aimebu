package supervisor

import (
	"context"
	"fmt"
	"log"
	"sync"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// A turn interrupt cuts one running turn and leaves its agent as it is: desired stays active, no generation changes, and the next delivery
// starts a new turn in the same session. The server asks for one with a turn_interrupt frame; the turn's exit is then reported as
// interrupted, never as a cancel, and the server decides what becomes of the turn's notes.

// turnInterruptedOnRequest is the reason an exit carries when the server asked for the turn to be cut.
const turnInterruptedOnRequest = "the turn was interrupted on request"

// turnInterruptTerminateGrace is how long a turn in a container is given to end on a terminate before it is killed (the launcher's own cleanup
// waits up to five seconds for the session in the container).
const turnInterruptTerminateGrace = keepAliveTerminateGrace

// activeTurn is what an interrupt needs to know of the turn an agent runs: which one, its mode, and whether its process is in a container
// whose launcher confirms the end of the session in it.
type activeTurn struct {
	turnID        string
	mode          adapter.Mode
	containerized bool
	confirmsEnd   bool
}

// turnInterrupt is one requested interrupt. terminated, when it is set, gets the answer of the terminate the turn's process was sent: whether
// it ended on it. released is closed by a stop of the agent or a supervisor shutdown, which end the wait for an unconfirmed end.
type turnInterrupt struct {
	turnID     string
	signalled  bool
	terminated chan bool
	released   chan struct{}
	once       sync.Once
}

func (i *turnInterrupt) release() { i.once.Do(func() { close(i.released) }) }

// noteActiveTurn records the turn an agent was just admitted for. The caller holds c.mu.
func (c *Client) noteActiveTurn(agentID string, turn activeTurn) {
	if c.activeTurns == nil {
		c.activeTurns = make(map[string]activeTurn)
	}
	c.activeTurns[agentID] = turn
}

// handleTurnInterrupt applies a turn_interrupt frame and says what it did. An interrupt of a turn the agent's stop (or the supervisor's
// shutdown) is already ending is "ok" and stays a stop. A repeat for the same turn is "ok" and signals nothing again.
func (c *Client) handleTurnInterrupt(frame protocol.Frame) string {
	c.mu.Lock()
	turnID := c.active[frame.AgentID]
	if frame.TurnID == "" || turnID != frame.TurnID || c.desired[frame.AgentID].Generation != frame.Generation {
		c.mu.Unlock()
		return "not_running"
	}
	info := c.activeTurns[frame.AgentID]
	if info.turnID != turnID || info.mode != adapter.ModeNormal {
		c.mu.Unlock()
		return "unsupported_mode"
	}
	// A supervisor that is only retiring (it takes no new work and exits once idle) still cuts a turn: that is how the server bounds its
	// drain. One that is shutting down is ending every turn already.
	if c.desired[frame.AgentID].Desired == "stopped" || c.shutdownRequested.Load() {
		c.mu.Unlock()
		return "ok"
	}
	if existing := c.turnInterrupts[frame.AgentID]; existing != nil && existing.turnID == turnID {
		c.mu.Unlock()
		return "ok"
	}
	if c.turnInterrupts == nil {
		c.turnInterrupts = make(map[string]*turnInterrupt)
	}
	c.turnInterrupts[frame.AgentID] = &turnInterrupt{turnID: turnID, released: make(chan struct{})}
	turn := c.running[frame.AgentID]
	c.mu.Unlock()
	log.Printf("turn interrupt: agent %s turn %s: %s", frame.AgentID, turnID, adapter.SanitizeStderr(frame.Reason))
	if turn != nil {
		// A turn whose process is still starting is signalled by the delivery once it runs.
		c.signalTurnInterrupt(frame.AgentID, turnID, turn)
	}
	return "ok"
}

// signalTurnInterrupt tells the process of an interrupted turn to end, once. A process in a container whose launcher confirms the end is
// told to terminate (the launcher ends the session in the container and says so); anything else is cancelled like a stopped turn, and
// killed if it has not gone after 30 seconds.
func (c *Client) signalTurnInterrupt(agentID, turnID string, turn adapter.Turn) {
	c.mu.Lock()
	interrupt := c.turnInterrupts[agentID]
	if interrupt == nil || interrupt.turnID != turnID || interrupt.signalled {
		c.mu.Unlock()
		return
	}
	interrupt.signalled = true
	info := c.activeTurns[agentID]
	terminator, canTerminate := turn.(interface{ Terminate(time.Duration) bool })
	if canTerminate && info.containerized && info.confirmsEnd {
		interrupt.terminated = make(chan bool, 1)
	}
	terminated := interrupt.terminated
	c.mu.Unlock()
	if terminated != nil {
		go func() {
			defer telemetry.Recover(context.Background())
			terminated <- terminator.Terminate(turnInterruptTerminateGrace)
		}()
		return
	}
	_ = turn.Cancel()
	time.AfterFunc(30*time.Second, func() {
		c.mu.Lock()
		stillRunning := c.active[agentID] == turnID
		c.mu.Unlock()
		if stillRunning {
			if killer, ok := turn.(interface{ Kill() error }); ok {
				_ = killer.Kill()
			} else {
				_ = turn.Cancel()
			}
		}
	})
}

// pendingTurnInterrupt is the interrupt asked for the turn, unless a stop of the agent cleared it since (a stop wins).
func (c *Client) pendingTurnInterrupt(agentID, turnID string) *turnInterrupt {
	c.mu.Lock()
	defer c.mu.Unlock()
	if interrupt := c.turnInterrupts[agentID]; interrupt != nil && interrupt.turnID == turnID {
		return interrupt
	}
	return nil
}

// clearTurnInterrupt forgets the agent's interrupt and ends its wait for an unconfirmed end: a stop of the agent, which then is what ends the
// turn. The caller holds c.mu.
func (c *Client) clearTurnInterrupt(agentID string) {
	if interrupt := c.turnInterrupts[agentID]; interrupt != nil {
		interrupt.release()
		delete(c.turnInterrupts, agentID)
	}
}

// forgetTurnInterrupt drops the interrupt of a turn that has ended.
func (c *Client) forgetTurnInterrupt(agentID, turnID string) {
	c.mu.Lock()
	if interrupt := c.turnInterrupts[agentID]; interrupt != nil && interrupt.turnID == turnID {
		delete(c.turnInterrupts, agentID)
	}
	c.mu.Unlock()
}

// settleTurnInterrupt runs when the process of a turn has been waited for. It says whether the turn's interrupt still stands and whether the
// supervisor ended the process for it. When the process ran in a container and its end there is not confirmed (also when the turn's answer
// was already in and the exit is terminal), the turn is not reported ended yet: the agent's admission slot is held (the server sees the turn still running and sends no other) for the quarantine a keep-alive
// ping's unconfirmed end gets, or until the agent is stopped or the supervisor shuts down.
func (c *Client) settleTurnInterrupt(delivery protocol.Delivery, turn adapter.Turn, exit adapter.ExitInfo) (requested, ours bool) {
	interrupt := c.pendingTurnInterrupt(delivery.AgentID, delivery.TurnID)
	if interrupt == nil {
		return false, false
	}
	c.mu.Lock()
	terminated := interrupt.terminated
	info := c.activeTurns[delivery.AgentID]
	c.mu.Unlock()
	_, warm := turn.(*warmTurn)
	usedTerminate, confirmed := false, false
	if terminated != nil {
		usedTerminate = true
		confirmed = <-terminated
		if !warm {
			// The launcher exits 143 once the session in the container is gone; any other status says the wrapper was killed.
			confirmed = confirmed && exit.ExitCode == 128+int(syscall.SIGTERM)
		}
	}
	// A warm process that was closed records no cancel intent of its own; anywhere else the adapter's record says whether the signal ended it.
	ours = exit.CancelIntent || (usedTerminate && warm)
	// A turn that finished by itself (a clean terminal exit nobody signalled) is gone. One whose answer was in but whose wrapper the
	// supervisor then ended is terminal too, yet says nothing of the process in the container: its end needs the same confirmation.
	selfEnded := exit.TerminalEvent && exit.ExitCode == 0 && !exit.CancelIntent
	if !ours || selfEnded || info.turnID != delivery.TurnID || !info.containerized || confirmed {
		return true, ours
	}
	wait := c.quarantine()
	c.degraded("an interrupted turn's end in its container was not confirmed", fmt.Errorf("agent %s turn %s: the end of the interrupted turn's process in its container is not confirmed; holding the agent for %s", delivery.AgentID, delivery.TurnID, wait))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-interrupt.released:
	}
	// A stop that came during the wait cleared the interrupt: the exit is then the stop's.
	return c.pendingTurnInterrupt(delivery.AgentID, delivery.TurnID) != nil, ours
}
