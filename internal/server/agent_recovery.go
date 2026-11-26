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
	// A worker's recovery is decided by whoever manages it: a lead, a dispatcher of a managing flow, or any role that can spawn.
	if !((caller.IsLead || caller.CanManage || caller.CanSpawn) && caller.Desired == "active") {
		apiError(w, 403, "forbidden", "active lead role required")
		return
	}
	items, err := s.Store.ListLeadRecoveries(r.Context(), caller.ID, caller.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	// A recovery's evidence is what the failed turn's process printed in its room.
	rooms := make([]string, 0, len(items))
	for _, item := range items {
		rooms = append(rooms, item.RoomID)
	}
	if !s.recordToolRoomReads(w, r, rooms...) {
		return
	}
	writeJSON(w, 200, map[string]any{"recoveries": items})
}

// agentResolveRecoveryRequest is the body of POST /api/agent/recoveries/{item}/decision, the resolve_recovery tool.
type agentResolveRecoveryRequest struct {
	Choice string `json:"choice"`
	Note   string `json:"note"`
}

func (s *Server) agentToolResolveRecovery(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	// The same gate as the list above: the store re-checks that the caller is the manager the item was assigned to.
	if !((caller.IsLead || caller.CanManage || caller.CanSpawn) && caller.Desired == "active") {
		apiError(w, 403, "forbidden", "active lead role required")
		return
	}
	key, ok := toolRequestKey(w, r)
	if !ok {
		return
	}
	var req agentResolveRecoveryRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Choice != "resume" && req.Choice != "retry" && req.Choice != "abandon" && req.Choice != "abandon_stop" && req.Choice != "restart" && req.Choice != "continue" || len(req.Note) > 2000 {
		apiError(w, 400, "invalid_request", "choice or note invalid")
		return
	}
	itemID := r.PathValue("item")
	if !strings.HasPrefix(itemID, "inb_") {
		apiError(w, 400, "invalid_request", "invalid item id")
		return
	}
	// Abandon and stop stops the agent, which takes the permission stop_agent takes (a role that can only spawn may abandon, not stop); the
	// store runs the rest of stop_agent's check, and a refusal there leaves the item open.
	if req.Choice == "abandon_stop" && !caller.IsLead && !caller.CanManage {
		apiError(w, 403, "forbidden", "stopping an agent needs the permission stop_agent needs: an active lead, or the flow's coordinator")
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
		case errors.Is(err, store.ErrInvalidTransition):
			// An expected refusal, not a server fault: restart exists only for a broken session.
			apiError(w, 409, "invalid_transition", "this recovery can't take that choice; for a failed start or an unconfirmed warm process, abandon it, or retry once the quarantine ends")
		default:
			spawnError(w, err)
		}
		return
	}
	status := 201
	if reused {
		status = 200
	}
	// Abandon and stop stopped the agent: its machine is told as a stop_agent tells it.
	if decision.StoppedAgent != nil && !reused {
		s.sendRoomControls(r.Context(), []store.AgentCard{*decision.StoppedAgent})
	}
	writeJSON(w, status, decision)
}
