package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

func (s *Server) SetLocalSupervisorID(id string) {
	s.localSupervisorIDMu.Lock()
	s.LocalSupervisorID = id
	s.localSupervisorIDMu.Unlock()
}

func (s *Server) localSupervisorID() string {
	s.localSupervisorIDMu.RLock()
	defer s.localSupervisorIDMu.RUnlock()
	return s.LocalSupervisorID
}

func (s *Server) restartLocalSupervisorOwner(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	if r.PathValue("id") != s.localSupervisorID() || s.LocalSupervisorRestart == nil {
		apiError(w, http.StatusNotFound, "not_found", "no managed local supervisor")
		return
	}
	s.restartLocalSupervisor(w, r.Context())
}

func (s *Server) restartLocalSupervisorDoctor(w http.ResponseWriter, r *http.Request) {
	if !s.doctorAuthorized(r) {
		apiError(w, http.StatusForbidden, "forbidden", "a same-host request with the doctor key is required")
		return
	}
	if s.LocalSupervisorRestart == nil {
		apiError(w, http.StatusNotFound, "not_found", "no managed local supervisor")
		return
	}
	s.restartLocalSupervisor(w, r.Context())
}

func (s *Server) restartLocalSupervisor(w http.ResponseWriter, ctx context.Context) {
	if s.localSupervisorUpgradeRequired.Load() {
		apiError(w, http.StatusConflict, "server_upgrade_required", "restart the server to finish the upgrade")
		return
	}
	turnIDs, err := s.Store.MachineRunningTurnIDs(ctx, s.localSupervisorID())
	if err != nil {
		fail(w, err)
		return
	}
	s.localRestartMu.Lock()
	if s.localRestartTurns == nil {
		s.localRestartTurns = make(map[string]time.Time)
	}
	for id, deadline := range s.localRestartTurns {
		if time.Now().After(deadline) {
			delete(s.localRestartTurns, id)
		}
	}
	for _, id := range turnIDs {
		s.localRestartTurns[id] = time.Now().Add(10 * time.Minute)
	}
	s.localRestartMu.Unlock()
	wasPending := s.localSupervisorPending.Swap(true)
	if err := s.LocalSupervisorRestart(); err != nil {
		s.localSupervisorPending.Store(wasPending)
		s.localRestartMu.Lock()
		for _, id := range turnIDs {
			delete(s.localRestartTurns, id)
		}
		s.localRestartMu.Unlock()
		apiError(w, http.StatusServiceUnavailable, "supervisor_restart_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"restart_pending": true})
}

// An older adopted supervisor reports its SIGTERM cancellation as an owner
// cancel. The new server knows which running turns the owner chose to
// interrupt, so it can preserve the normal one-time continuation on replay.
func (s *Server) localSupervisorRestartExit(turnID string, exit adapter.ExitInfo) adapter.ExitInfo {
	s.localRestartMu.Lock()
	deadline, marked := s.localRestartTurns[turnID]
	s.localRestartMu.Unlock()
	if !marked || time.Now().After(deadline) || !exit.CancelIntent || exit.TerminalEvent {
		return exit
	}
	exit.CancelIntent = false
	exit.Interrupted = true
	exit.ErrorEvent = "local supervisor restarted by owner"
	if exit.ExitCode == 0 {
		exit.ExitCode = 130
	}
	return exit
}

func (s *Server) LocalSupervisorConnected() bool {
	id := s.localSupervisorID()
	if id == "" {
		return false
	}
	conn, err := s.machineForID(id)
	if err != nil {
		return false
	}
	select {
	case <-conn.done:
		return false
	default:
		return true
	}
}

func (s *Server) LocalSupervisorCanInterrupt() bool {
	return s.LocalSupervisorConnected() && s.localSupervisorCanInterrupt.Load()
}

// The bounded drain. A local supervisor whose build or turn environment no longer matches the server's is asked to retire at idle, and new
// local dispatch is held until its replacement connects. One long turn would hold every local agent for as long as it runs, so the wait
// has a limit: the owner's drain grace. When it runs out, every ordinary turn the old supervisor still runs is interrupted with the
// continue disposition (see store.RequestTurnInterrupt): the turn ends, the supervisor goes idle and exits, and the turn is continued on
// its replacement, outside the chain's one automatic continuation. A command outside a turn is never ended for this: the old supervisor
// stays up until its commands finish, as before.
const (
	supervisorDrainGraceKey     = "supervisor_drain_grace_seconds"
	defaultDrainGraceSeconds    = 5 * 60
	maxDrainGraceSeconds        = 24 * 3600
	supervisorDrainPurpose      = "supervisor_drain"
	supervisorDrainActor        = "supervisor_drain"
	supervisorDrainFrameReason  = "the supervisor is being replaced and the drain grace is over"
	supervisorDrainGraceMinutes = "supervisor_drain_grace_minutes"
)

// drainGraceSeconds is the owner's drain grace: the saved value, else five minutes. 0 is off (the supervisor is waited for without limit).
// A value that cannot be read counts as none saved.
func (s *Server) drainGraceSeconds(ctx context.Context) (int, error) {
	value, ok, err := s.Store.GlobalSettingValue(ctx, supervisorDrainGraceKey)
	if err != nil || !ok {
		return defaultDrainGraceSeconds, err
	}
	seconds, convErr := strconv.Atoi(value)
	if convErr != nil || seconds < 0 || seconds > maxDrainGraceSeconds {
		return defaultDrainGraceSeconds, nil
	}
	return seconds, nil
}

// clearLocalDrain forgets the drain's deadline: a supervisor that matches the server is connected.
func (s *Server) clearLocalDrain() {
	s.localDrainMu.Lock()
	s.localDrainDeadline = time.Time{}
	s.localDrainMu.Unlock()
}

// startLocalDrainGrace runs after retire_at_idle was sent to a mismatched local supervisor: it waits for the drain's deadline on this
// connection and then interrupts what still runs. The deadline is set when the mismatch is first seen and kept across the supervisor's
// reconnects (each of which gets a retire_at_idle of its own), so a flapping connection does not extend the grace. Nothing is started for a
// grace of 0 or for a supervisor that does not announce supports_turn_interrupt: it is waited for as before.
func (s *Server) startLocalDrainGrace(ctx context.Context, c *machineConn) {
	if !c.supportsTurnInterrupt {
		return
	}
	grace, err := s.drainGraceSeconds(ctx)
	if err != nil {
		telemetry.ReportWarning(ctx, "machine", fmt.Errorf("reading the supervisor drain grace: %w", err))
		return
	}
	if grace == 0 {
		return
	}
	s.localDrainMu.Lock()
	if s.localDrainDeadline.IsZero() {
		s.localDrainDeadline = time.Now().Add(time.Duration(grace) * time.Second)
	}
	deadline := s.localDrainDeadline
	s.localDrainMu.Unlock()
	c.workers.Add(1)
	telemetry.Go(ctx, func() {
		defer c.workers.Done()
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-c.done:
			return
		case <-ctx.Done():
			return
		}
		// The owner may have turned the grace off while it ran.
		if now, err := s.drainGraceSeconds(ctx); err != nil || now == 0 {
			return
		}
		s.interruptDrainingTurns(ctx, c)
	})
}

// interruptDrainingTurns interrupts, with the continue disposition, every ordinary turn the machine still runs. A turn that is not running
// any more, or is not an ordinary one (a correction, a diagnostic, a handoff turn), is left to finish. So is the turn of an agent a team
// flow change in progress holds: the change has its own interrupt and decides what becomes of that turn. A turn somebody already asked
// to be interrupted keeps that request's disposition; it is only sent the frame again.
func (s *Server) interruptDrainingTurns(ctx context.Context, c *machineConn) {
	turnIDs, err := s.Store.MachineRunningTurnIDs(ctx, c.id)
	if err != nil {
		telemetry.ReportError(ctx, "machine", fmt.Errorf("supervisor drain: listing the running turns: %w", err))
		return
	}
	for _, turnID := range turnIDs {
		target, err := s.Store.RequestTurnInterrupt(ctx, store.TurnInterruptRequest{TurnID: turnID, ActorKind: "system", ActorID: supervisorDrainActor,
			Purpose: supervisorDrainPurpose, Disposition: store.TurnInterruptContinue, Yield: true})
		if errors.Is(err, store.ErrInvalidTransition) {
			continue
		}
		if err != nil {
			telemetry.ReportError(ctx, "machine", fmt.Errorf("supervisor drain: recording a turn interrupt: %w", err))
			continue
		}
		result, err := s.interruptTurn(ctx, target, supervisorDrainFrameReason)
		log.Printf("supervisor drain: grace over, turn %s of agent %s interrupted to continue on the new supervisor: result %q, error %v", target.TurnID, target.AgentID, result, err)
		if err != nil {
			return // the connection is gone or does not answer: the supervisor's next connection tries again
		}
	}
}
