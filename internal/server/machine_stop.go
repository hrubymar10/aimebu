package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
)

// stopFailure is an agent that a machine-wide stop could not stop, and why.
type stopFailure struct {
	AgentID string `json:"agent_id"`
	Error   string `json:"error"`
}

type stopOutcome int

const (
	stoppedNow stopOutcome = iota
	stoppedAlready
	stoppedForbidden
	stoppedFailed
)

// stopAgentOnMachine stops one agent the way a single stop does, for actor. An agent that someone else changed meanwhile (a stale
// revision) is read again once: it is left alone if it is stopped, revoked or no longer on this machine by then, and stopped on the
// revision it has now otherwise. Only "there is no such agent any more" on that second look counts as already stopped; any other error
// is a failure of its own, never mistaken for success.
func (s *Server) stopAgentOnMachine(ctx context.Context, actor, machineID string, agent store.AgentRuntime) (stopOutcome, store.AgentCard, error) {
	rev := agent.DesiredRev
	for attempt := 0; attempt < 2; attempt++ {
		var updated store.AgentCard
		err := s.Store.Tx(ctx, func(tx *store.Tx) error {
			var err error
			updated, err = tx.SetAgentDesired(agent.ID, actor, "stopped", rev)
			return err
		})
		switch {
		case err == nil:
			return stoppedNow, updated, nil
		case errors.Is(err, store.ErrForbidden):
			return stoppedForbidden, store.AgentCard{}, nil
		case !errors.Is(err, store.ErrStaleRev):
			return stoppedFailed, store.AgentCard{}, err
		}
		current, lookup := s.Store.AgentRuntimeByID(ctx, agent.ID)
		switch {
		case errors.Is(lookup, sql.ErrNoRows):
			return stoppedAlready, store.AgentCard{}, nil
		case lookup != nil:
			return stoppedFailed, store.AgentCard{}, lookup
		case current.Revoked || current.Desired == "stopped" || current.MachineID != machineID:
			return stoppedAlready, store.AgentCard{}, nil
		}
		rev = current.DesiredRev
	}
	return stoppedFailed, store.AgentCard{}, store.ErrStaleRev
}

// POST /api/machines/{id}/agents/stop: stops every agent that runs on the machine, the way a single stop does (the desired state goes to
// stopped and the machine is told), and by the same permission: an agent in a project the person may not manage is left running and counted
// as forbidden. Like the other machine routes it is for the owner and the admins, so it tells nobody else which machines exist or what
// runs on them. Stopping is idempotent, so there is no request key; a second call finds nothing left to stop.
func (s *Server) machineStopAgents(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	machine, err := s.Store.MachineByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	agents, err := s.Store.AgentsForMachine(r.Context(), machine.ID)
	if err != nil {
		fail(w, err)
		return
	}
	stopped, controls, failed := []string{}, []store.AgentCard{}, []stopFailure{}
	already, forbidden, tried := 0, 0, 0
	for _, agent := range agents {
		if agent.Revoked || agent.Desired == "stopped" {
			already++
			continue
		}
		tried++
		outcome, updated, err := s.stopAgentOnMachine(r.Context(), userID(r), machine.ID, agent)
		switch outcome {
		case stoppedNow:
			stopped = append(stopped, agent.ID)
			controls = append(controls, updated)
		case stoppedAlready:
			already++
		case stoppedForbidden:
			forbidden++
		default:
			failed = append(failed, stopFailure{AgentID: agent.ID, Error: err.Error()})
		}
	}
	if tried > 0 && forbidden == tried {
		apiError(w, 403, "forbidden", "actor is not allowed to manage the agents on this machine")
		return
	}
	s.sendRoomControls(r.Context(), controls)
	writeJSON(w, 200, map[string]any{"stopped": stopped, "already_stopped": already, "forbidden": forbidden, "failed": failed})
}
