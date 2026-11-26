package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// lostExit is the exit a turn gets when the supervisor that ran it died and nothing else proves how it ended. It is the exit a drain
// instance has always recorded for the turns its journal still listed as running, so the settlement semantics do not change.
func lostExit() adapter.ExitInfo {
	return adapter.ExitInfo{ExitCode: -1, ErrorEvent: "supervisor_lost", ErrorSummary: "the supervisor that ran this turn died"}
}

// orphanTermGrace is how long a live process group whose turn nobody will deliver is given to end on a SIGTERM before it is killed.
const orphanTermGrace = 5 * time.Second

// orphanTermPoll is how often the wait for a signalled group checks whether it is gone.
const orphanTermPoll = 50 * time.Millisecond

// The SIGKILL may take a moment to become visible to the process lookup.
var orphanKillGrace = 2 * time.Second
var orphanKillPoll = 50 * time.Millisecond

// settleDeadTurns settles the turns a dead predecessor left running in this journal. A row whose start recorded its harness process
// identity is settled only once that identity has been checked, and the check has three outcomes (see ProcessStartState): a process the
// system reports gone proves the turn dead; a process running with the same pid and start time is an orphan whose output nobody will
// deliver, so its group is asked to end (SIGTERM, a bounded wait, SIGKILL) before the turn is settled; and a lookup that cannot tell
// proves nothing, so the row is left untouched for the next start or hello. A row without an identity — from a supervisor that did not
// record one — is left to the drain path, which settles it as it always has, unless includeLegacy says to settle it here too.
//
// It runs once when the supervisor starts (includeLegacy false: before the first hello, so a hello never reports a dead predecessor's
// turn as running when the journal can vouch for its process) and again in a drain's hello (includeLegacy true, in place of the old
// assumption that every row is dead). It returns how many turns it settled.
func (c *Client) settleDeadTurns(ctx context.Context, includeLegacy bool) (int, error) {
	turns, err := c.Journal.RunningTurnProcesses(ctx)
	if err != nil {
		return 0, err
	}
	settled := 0
	settle := func(turnID string) error {
		err := c.Journal.RecordExitInfo(ctx, turnID, lostExit())
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the turn ended while it was being settled; its own exit stands
		}
		if err == nil {
			settled++
		}
		return err
	}
	for _, turn := range turns {
		if turn.Pid <= 0 || turn.Pgid <= 0 || turn.Start == "" {
			// No identity: today's behaviour. A drain ends the row as it always did; a regular startup leaves it to that path.
			if !includeLegacy {
				continue
			}
			if err := settle(turn.TurnID); err != nil {
				return settled, err
			}
			continue
		}
		switch state, start := c.processState(turn.Pid); state {
		case StartKnown:
			if start != turn.Start {
				// The pid now belongs to a later process: never signalled, and the turn is dead all the same.
				if err := settle(turn.TurnID); err != nil {
					return settled, err
				}
				break
			}
			// An orphan whose output nobody will deliver. Only ever a process whose pid AND start time match is signalled.
			if !c.terminateOrphanGroup(turn.Pid, turn.Pgid, turn.Start) {
				// The lookup could not tell while the group was being ended: no kill and no settle on its word; the row is left for
				// the next pass.
				break
			}
			if err := settle(turn.TurnID); err != nil {
				return settled, err
			}
		case StartGone:
			// The system confirms the pid is absent: the turn's process is gone.
			if err := settle(turn.TurnID); err != nil {
				return settled, err
			}
		default: // StartUnknown
			// ps could not tell: nothing is proved, nothing is signalled, the row stays running for the next check.
			c.logUnknownStart(turn.TurnID)
		}
	}
	if !includeLegacy {
		return settled, nil
	}
	// EndDeadTurns settles every row still listed as running — by now only the rows without an identity, which are the only ones its
	// old assumption was ever sound for.
	n, err := c.Journal.EndDeadTurns(ctx, lostExit())
	if err != nil {
		return settled, err
	}
	return settled + n, nil
}

// terminateOrphanGroup asks a live process group to end and, within a bounded wait, forces it. It says whether the group's end was
// verified: a lookup that cannot tell — before the first signal or at any point of the wait — is not a verified end, so the signalling
// stops there and the caller leaves the row for the next check. The pid and its start time are checked again before the first signal and
// before the kill, so a group whose process ended (or whose pid moved on) in the meantime is never touched.
func (c *Client) terminateOrphanGroup(pid, pgid int, start string) bool {
	// gone is true only when the system proves the pid no longer carries the recorded process; a lookup that cannot tell is not gone,
	// and it stops the signalling rather than reading as an end.
	gone := func() bool {
		state, now := c.processState(pid)
		return state == StartGone || state == StartKnown && now != start
	}
	unclear := func() bool {
		state, _ := c.processState(pid)
		return state == StartUnknown
	}
	if gone() {
		return true
	}
	if unclear() {
		return false
	}
	_ = adapter.SignalGroup(pgid, syscall.SIGTERM, gone)
	deadline := time.Now().Add(orphanTermGrace)
	for {
		if gone() {
			return true
		}
		if unclear() {
			return false
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(orphanTermPoll)
	}
	if unclear() {
		return false
	}
	_ = adapter.SignalGroup(pgid, syscall.SIGKILL, gone)
	deadline = time.Now().Add(orphanKillGrace)
	for {
		if gone() {
			return true
		}
		if unclear() || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(orphanKillPoll)
	}
}

// logUnknownStart says once per turn per process run that the turn's process could not be checked, so a silent unchecked row is visible
// in the log without one line per pass.
func (c *Client) logUnknownStart(turnID string) {
	if _, loaded := c.unknownStarts.LoadOrStore(turnID, true); loaded {
		return
	}
	log.Printf("supervisor: the process identity of turn %s could not be checked (ps could not tell); the turn is left running for the next check", turnID)
}

// recordTurnProcess journals the identity of the process a turn's adapter just started, so a later supervisor can tell that process from
// a later one that reused its pid. An adapter that offers no identity, a pid the system cannot stamp with a verified start time (the
// record would be unverifiable, so it is not recorded: the row keeps the legacy behaviour), and a turn that ended before the identity was
// read all record nothing.
func (c *Client) recordTurnProcess(ctx context.Context, turnID string, turn adapter.Turn) {
	identified, ok := turn.(adapter.ProcessIdentified)
	if !ok {
		return
	}
	pid, pgid := identified.ProcessIdentity()
	if pid <= 0 || pgid <= 0 {
		return
	}
	state, start := c.processState(pid)
	if state != StartKnown {
		return
	}
	if err := c.Journal.RecordTurnProcess(ctx, turnID, pid, pgid, start); err != nil && !errors.Is(err, sql.ErrNoRows) {
		c.report("a turn's process identity could not be journaled", err)
	}
}
