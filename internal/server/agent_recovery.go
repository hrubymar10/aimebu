package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func (s *Server) agentToolRecoveries(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if !activeLead(w, caller) {
		return
	}
	if !caller.IsLead {
		apiError(w, 403, "forbidden", "project lead required")
		return
	}
	items, err := s.Store.ListLeadRecoveries(r.Context(), caller.ID, caller.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"recoveries": items})
}

func (s *Server) agentToolResolveRecovery(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if !activeLead(w, caller) {
		return
	}
	if !caller.IsLead {
		apiError(w, 403, "forbidden", "project lead required")
		return
	}
	key, ok := toolRequestKey(w, r)
	if !ok {
		return
	}
	var req struct {
		Choice string `json:"choice"`
		Note   string `json:"note"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Choice != "resume" && req.Choice != "retry" && req.Choice != "abandon" && req.Choice != "restart" || len(req.Note) > 2000 {
		apiError(w, 400, "invalid_request", "choice or note invalid")
		return
	}
	itemID := r.PathValue("item")
	if !strings.HasPrefix(itemID, "inb_") {
		apiError(w, 400, "invalid_request", "invalid item id")
		return
	}
	rev, err := s.Store.RecoveryItemRevision(r.Context(), itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 404, "not_found", "recovery item not found")
		} else {
			fail(w, err)
		}
		return
	}
	decision, reused, err := s.Store.DecideRecoveryAsAgent(r.Context(), itemID, caller.ID, key, req.Choice, req.Note, rev)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrForbidden):
			apiError(w, 403, "forbidden", "project lead required")
		case errors.Is(err, store.ErrConflict):
			apiError(w, 409, "conflict", "request key reused with different decision")
		case errors.Is(err, store.ErrAlreadyResolved), errors.Is(err, store.ErrAlreadyWithdrawn):
			message := "recovery already resolved"
			if errors.Is(err, store.ErrAlreadyWithdrawn) {
				message = "recovery already withdrawn"
			}
			var settled *store.AlreadySettledError
			if errors.As(err, &settled) && settled.By != "" {
				message = settled.Error()
			}
			apiError(w, 409, "already_resolved", message)
		case errors.Is(err, store.ErrStaleInboxRev):
			apiError(w, 409, "stale_rev", "recovery evidence changed")
		case errors.Is(err, store.ErrChoiceNotOffered):
			apiError(w, 400, "invalid_choice", "choice not offered")
		default:
			fail(w, err)
		}
		return
	}
	status := 201
	if reused {
		status = 200
	}
	writeJSON(w, status, decision)
}
