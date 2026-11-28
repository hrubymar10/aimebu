package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/store"
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
	if err := s.forceLocalSupervisorRestart(ctx); err != nil {
		if errors.Is(err, store.ErrMachineScope) {
			apiError(w, http.StatusConflict, "machine_scope", "the local machine is disabled")
			return
		}
		var stopErr *supervisorStopError
		if errors.As(err, &stopErr) {
			apiError(w, http.StatusServiceUnavailable, "supervisor_restart_failed", err.Error())
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"restart_pending": true})
}

// supervisorStopError is a restart the supervisor's manager could not carry out (the process could not be signalled or started).
type supervisorStopError struct{ err error }

func (e *supervisorStopError) Error() string { return e.err.Error() }
func (e *supervisorStopError) Unwrap() error { return e.err }

// forceLocalSupervisorRestart replaces the local supervisor now, whatever it runs: the turns it runs are remembered, so that their ends
// count as interrupted (one automatic continuation on the replacement) and not as cancelled, and the manager is told to stop the process
// and start another. It is the owner's Restart now, and what the drain falls back to for a supervisor it cannot interrupt turn by turn.
func (s *Server) forceLocalSupervisorRestart(ctx context.Context) error {
	host, err := s.Store.HostMachine(ctx)
	if err != nil {
		return err
	}
	if host.Disabled {
		return store.ErrMachineScope
	}
	turnIDs, err := s.Store.MachineRunningTurnIDs(ctx, s.localSupervisorID())
	if err != nil {
		return err
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
		return &supervisorStopError{err}
	}
	return nil
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

// LocalSupervisorConnections is the journal ID of every local supervisor instance that is connected now, the active one and the retiring ones:
// what the manager needs to tell a live instance from one that holds its journal without talking to the server.
func (s *Server) LocalSupervisorConnections() map[string]bool {
	out := map[string]bool{}
	id := s.localSupervisorID()
	if id == "" {
		return out
	}
	for _, c := range s.machineConnections(id) {
		out[c.journalID] = true
	}
	return out
}

func (s *Server) LocalSupervisorCanInterrupt() bool {
	return s.LocalSupervisorConnected() && s.localSupervisorCanInterrupt.Load()
}

// ReplaceOverdueLocalSupervisor is retained for the current manager callback until
// the instance-aware manager replaces it. Normal retirement has no deadline.
func (s *Server) ReplaceOverdueLocalSupervisor(context.Context) (bool, error) {
	return false, nil
}
