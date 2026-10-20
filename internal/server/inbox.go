package server

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.InboxForUser(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) dismissDeliveryFailure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rev int `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Rev < 0 {
		apiError(w, 400, "invalid_request", "invalid revision")
		return
	}
	err := s.Store.DismissDeliveryFailure(r.Context(), r.PathValue("item"), userID(r), req.Rev)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "inbox item does not exist")
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "cannot dismiss this item")
	case errors.Is(err, store.ErrStaleInboxRev):
		apiError(w, 409, "stale_rev", "inbox item changed")
	case errors.Is(err, store.ErrAlreadyResolved), errors.Is(err, store.ErrAlreadyWithdrawn):
		apiError(w, 409, "already_resolved", "inbox item is no longer open")
	default:
		fail(w, err)
	}
}

func (s *Server) decideRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Choice string `json:"choice"`
		Rev    int    `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Rev < 0 || req.Choice != "resume" && req.Choice != "retry" && req.Choice != "abandon" && req.Choice != "continue" && req.Choice != "override" && req.Choice != "approve" && req.Choice != "deny" {
		apiError(w, 400, "invalid_request", "invalid recovery choice or revision")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required")
		return
	}
	kind, err := s.Store.InboxKind(r.Context(), r.PathValue("item"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 404, "not_found", "inbox item does not exist")
		} else {
			fail(w, err)
		}
		return
	}
	var decision store.RecoveryDecision
	var reused bool
	boundary, err := s.Store.IsBoundaryInbox(r.Context(), r.PathValue("item"))
	if err != nil {
		fail(w, err)
		return
	}
	if boundary {
		decision, reused, err = s.Store.DecideBoundary(r.Context(), r.PathValue("item"), userID(r), key, req.Choice, req.Rev)
	} else if kind == "usage_limit" {
		decision, reused, err = s.Store.DecideUsageLimit(r.Context(), r.PathValue("item"), userID(r), key, req.Choice, req.Rev)
	} else if kind == "usage_info" {
		decision, reused, err = s.Store.DecideProviderLimit(r.Context(), r.PathValue("item"), userID(r), key, req.Choice, req.Rev)
	} else {
		decision, reused, err = s.Store.DecideRecovery(r.Context(), r.PathValue("item"), userID(r), key, req.Choice, req.Rev)
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrForbidden):
			apiError(w, 403, "forbidden", "owner or operator required")
		case errors.Is(err, store.ErrConflict):
			apiError(w, 409, "conflict", "request key reused with different decision")
		case errors.Is(err, store.ErrAlreadyResolved):
			apiError(w, 409, "already_resolved", "inbox item is resolved")
		case errors.Is(err, store.ErrAlreadyWithdrawn):
			apiError(w, 409, "already_withdrawn", "inbox item is withdrawn")
		case errors.Is(err, store.ErrStaleInboxRev):
			apiError(w, 409, "stale_rev", "inbox item or recovery evidence changed")
		case errors.Is(err, store.ErrInvalidTransition):
			apiError(w, 409, "invalid_transition", "choice is invalid for this recovery item")
		case errors.Is(err, store.ErrWriterOccupied):
			apiError(w, 409, "writer_occupied", "another agent holds the checkout writer role")
		default:
			fail(w, err)
		}
		return
	}
	status := 201
	if reused {
		status = 200
	}
	if boundary && !reused {
		agentID, lookupErr := s.Store.BoundaryAgentID(r.Context(), r.PathValue("item"))
		if lookupErr == nil {
			if card, cardErr := s.Store.AgentCardByID(r.Context(), agentID); cardErr == nil {
				if machine, machineErr := s.machineForID(card.MachineID); machineErr == nil {
					machine.boundMu.Lock()
					machine.bound[agentID] = card.Generation
					machine.boundMu.Unlock()
					_ = machine.send(protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: agentID, Generation: card.Generation,
						DesiredRev: card.DesiredRev, DesiredState: card.Desired})
					if pending, ready, spawnErr := s.Store.PendingSpawnByID(r.Context(), agentID); spawnErr == nil && ready {
						_ = s.sendPendingSpawn(r.Context(), machine, pending)
					}
				}
			}
		}
	}
	writeJSON(w, status, decision)
}
