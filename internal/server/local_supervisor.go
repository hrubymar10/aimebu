package server

import (
	"context"
	"net/http"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
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
