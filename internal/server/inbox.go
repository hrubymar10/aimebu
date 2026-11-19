package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/usages"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.InboxForUser(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	// A superseded ask is out of the Inbox (its list and its badge count drop it) but not over: its answer controls stay in the chat, so its
	// row rides along here and the Inbox view leaves it out on its state.
	superseded, err := s.Store.SupersededAsksForUser(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	items = append(items, superseded...)
	if s.usageAlerts != nil {
		admin, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
		if err != nil {
			fail(w, err)
			return
		}
		if admin {
			for _, alert := range s.usageAlerts.OpenAlerts() {
				if s.strongerUsageGuard(r, alert) {
					continue
				}
				reset := "reset time unavailable"
				if alert.ResetAt != nil {
					reset = "resets " + alert.ResetAt.UTC().Format("2 Jan 2006 15:04 MST")
				}
				question := fmt.Sprintf("%s · %s · %s: %.1f%% remaining; %s.", providers.ProviderLabel(alert.Provider), alert.Profile, alert.Window, alert.Remaining, reset)
				items = append(items, store.InboxEntry{ID: alert.ID, Kind: "usage_alert", Question: question, State: "open",
					Options: []string{}, CreatedAt: alert.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt < items[j].CreatedAt || items[i].CreatedAt == items[j].CreatedAt && items[i].ID < items[j].ID
	})
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) strongerUsageGuard(r *http.Request, alert usages.UsageAlert) bool {
	guard := s.limitGuard()
	if guard == nil {
		return false
	}
	tool := alert.Provider
	if tool == "claude-code" {
		tool = "claude"
	}
	if tool != "claude" && tool != "codex" {
		return false
	}
	view, err := guard.View(r.Context(), tool)
	return err == nil && view.Held
}

func (s *Server) dismissInboxItem(w http.ResponseWriter, r *http.Request) {
	if s.usageAlerts != nil && strings.HasPrefix(r.PathValue("item"), "usage_alert_") {
		admin, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
		if err != nil {
			fail(w, err)
			return
		}
		if !admin {
			apiError(w, 403, "forbidden", "cannot dismiss this alert")
			return
		}
		if err := s.usageAlerts.Dismiss(r.PathValue("item")); err != nil {
			if errors.Is(err, usages.ErrAlertNotFound) {
				apiError(w, 404, "not_found", "usage alert does not exist")
			} else {
				fail(w, err)
			}
			return
		}
		s.usageAlertRevision.Add(1)
		s.nudgeUsageWatchers()
		w.WriteHeader(http.StatusNoContent)
		return
	}
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
	err := s.Store.DismissInboxItem(r.Context(), r.PathValue("item"), userID(r), req.Rev)
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
	// An approval can ask about a shared agent note (approve it, or decline and delete it) instead of about a boundary.
	noteID, err := s.Store.NoteApprovalID(r.Context(), r.PathValue("item"))
	if err != nil {
		fail(w, err)
		return
	}
	boundary := false
	if noteID == "" {
		if boundary, err = s.Store.IsBoundaryInbox(r.Context(), r.PathValue("item")); err != nil {
			fail(w, err)
			return
		}
	}
	if noteID != "" {
		decision = store.RecoveryDecision{ItemID: r.PathValue("item"), Choice: req.Choice, Rev: req.Rev + 1}
		err = s.Store.DecideNoteApproval(r.Context(), r.PathValue("item"), userID(r), req.Choice, int64(req.Rev))
	} else if boundary {
		// An approval starts the agent, which may work in a worktree of its own: it holds that worktree's lock, as any start does, so a removal
		// cannot take the folder between its check and this start.
		agentID, idErr := s.Store.BoundaryAgentID(r.Context(), r.PathValue("item"))
		if idErr != nil {
			fail(w, idErr)
			return
		}
		defer s.lockOwnWorktree(r.Context(), agentID)()
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
		case errors.Is(err, store.ErrChoiceNotOffered):
			apiError(w, 400, "invalid_choice", "this item does not offer that choice")
		case errors.Is(err, store.ErrInvalidTransition):
			apiError(w, 409, "invalid_transition", "choice is invalid for this recovery item")
		case errors.Is(err, store.ErrProjectArchived):
			apiError(w, 409, "project_archived", "this project is archived: only Abandon is possible, and it ends with the agent stopped; reopen the project to continue")
		default:
			fail(w, err)
		}
		return
	}
	status := 201
	if reused {
		status = 200
	}
	// An Abandon in an archived project stops the agent; its machine is told as an archive tells it.
	if decision.StoppedAgent != nil && !reused {
		s.sendRoomControls(r.Context(), []store.AgentCard{*decision.StoppedAgent})
	}
	if boundary && !reused {
		agentID, lookupErr := s.Store.BoundaryAgentID(r.Context(), r.PathValue("item"))
		if lookupErr == nil {
			if card, cardErr := s.Store.AgentCardByID(r.Context(), agentID); cardErr == nil {
				if machine, machineErr := s.machineForID(card.MachineID); machineErr == nil {
					machine.boundMu.Lock()
					machine.bound[agentID] = card.Generation
					machine.boundMu.Unlock()
					_ = machine.sendControl(protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: agentID, Generation: card.Generation,
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
