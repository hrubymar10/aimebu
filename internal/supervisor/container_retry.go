package supervisor

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A Docker launcher that finds its harness container on the way out ("Harness container is removing", or one that vanished between the
// launcher's two looks at it: "does not exist") starts nothing and exits. That is what a rebuild of the container looks like for the few
// seconds between the old one going and the new one being current, so the turn is started again for a bounded time before its failure
// counts.
var (
	// containerRestartLimit is how long a turn waits, from its first such failure, for a container to launch into.
	containerRestartLimit = time.Minute
	// containerRestartEvery is the pause before each new start.
	containerRestartEvery = 2 * time.Second
)

var containerReplacedText = regexp.MustCompile(`Harness container (is removing|does not exist)`)

// containerBeingReplaced says a turn's process ended as a launcher does that found its container going or gone: a failure with that message
// and no answer. The caller also knows the process said nothing else.
func containerBeingReplaced(exit adapter.ExitInfo) bool {
	return exit.ExitCode != 0 && !exit.TerminalEvent && !exit.CancelIntent && containerReplacedText.MatchString(exit.StderrTail)
}

// endedTurn stands for a turn that is over and settled: it has no events left and no process to settle, only its exit.
type endedTurn struct{ exit adapter.ExitInfo }

func (endedTurn) Events() <-chan adapter.Event {
	events := make(chan adapter.Event)
	close(events)
	return events
}
func (endedTurn) Cancel() error                     { return nil }
func (t endedTurn) Wait() (adapter.ExitInfo, error) { return t.exit, nil }

// containerRestart starts a turn again whose launcher found its container being replaced (containerBeingReplaced) and started nothing. It
// returns nil when the failure stands as it is: the wait is over, or the server asked for the turn to be interrupted. Otherwise the failed
// attempt's warm process is settled here and the returned turn replaces it: a running one (again), or, when the agent was stopped meanwhile
// or the new start failed, one that is already over and only carries the exit to report.
func (c *Client) containerRestart(ctx context.Context, delivery protocol.Delivery, failed adapter.Turn, exit adapter.ExitInfo,
	restart func() (adapter.Turn, error), until *time.Time) (next adapter.Turn, again bool) {
	limit, every := containerRestartLimit, containerRestartEvery
	if until.IsZero() {
		*until = time.Now().Add(limit)
		log.Printf("agent %s turn %s: the harness container is being replaced (%s); the turn is started again for up to %v", delivery.AgentID, delivery.TurnID, containerReplacedText.FindString(exit.StderrTail), limit)
	}
	if !time.Now().Before(*until) || c.pendingTurnInterrupt(delivery.AgentID, delivery.TurnID) != nil {
		return nil, false
	}
	// The process of the failed attempt is over; a warm one leaves its slot before another is started on the session.
	c.warmTurnEnded(failed, exit)
	stopped := adapter.ExitInfo{ExitCode: 130, CancelIntent: true, StderrTail: "the agent was stopped while its turn waited for its harness container; the turn was not started again"}
	timer := time.NewTimer(every)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return endedTurn{exit}, false
	case <-timer.C:
	}
	if c.stoppedWhileWaiting(delivery.AgentID) {
		return endedTurn{stopped}, false
	}
	if c.pendingTurnInterrupt(delivery.AgentID, delivery.TurnID) != nil {
		return endedTurn{exit}, false // asked to be cut while it waited: nothing is started for it, and the failure stands
	}
	turn, err := restart()
	if errors.Is(err, errStoppedWhileWaiting) {
		return endedTurn{stopped}, false
	}
	if err != nil {
		exit.StderrTail = strings.TrimSpace(exit.StderrTail + "\nstarting the turn again failed: " + adapter.SanitizeStderr(err.Error()))
		return endedTurn{exit}, false
	}
	c.mu.Lock()
	c.running[delivery.AgentID] = turn
	stopNow := c.stopping || c.desired[delivery.AgentID].Desired == "stopped"
	// An interrupt that came while the new process was starting was signalled to the attempt before it, which had nothing left to end.
	if interrupt := c.turnInterrupts[delivery.AgentID]; interrupt != nil && interrupt.turnID == delivery.TurnID && interrupt.signalled {
		stopNow = true
	}
	c.mu.Unlock()
	if stopNow {
		_ = turn.Cancel()
	} else {
		c.signalTurnInterrupt(delivery.AgentID, delivery.TurnID, turn)
	}
	return turn, true
}
