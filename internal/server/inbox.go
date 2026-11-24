package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
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
				question := usageAlertQuestion(alert)
				items = append(items, store.InboxEntry{ID: alert.ID, Kind: "usage_alert", Question: question, State: "open",
					Provider: alert.Provider, Profile: alert.Profile, Window: alert.Window, Options: []string{}, CreatedAt: alert.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")})
			}
			// A window reset is good news, not a task: it rides along as a
			// `notice` row the browser shows once as a toast, and the Inbox
			// (its list and its count) leaves it out on its state.
			for _, notice := range s.usageAlerts.Notices() {
				items = append(items, store.InboxEntry{ID: notice.ID, Kind: "usage_notice", Question: usageAlertQuestion(notice), State: "notice",
					Provider: notice.Provider, Profile: notice.Profile, Window: notice.Window, Options: []string{}, CreatedAt: notice.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt < items[j].CreatedAt || items[i].CreatedAt == items[j].CreatedAt && items[i].ID < items[j].ID
	})
	writeJSON(w, http.StatusOK, items)
}

// usageAlertQuestion renders one Inbox line per alert tier. The threshold
// tiers keep the shape the remaining-quota alerts always had; the opt-in
// alert tiers describe the episode, the reset or the expiry instead.
func usageAlertQuestion(alert usages.UsageAlert) string {
	who := providers.ProviderLabel(alert.Provider) + " · " + alert.Profile
	reset := "reset time unavailable"
	if alert.ResetAt != nil {
		reset = "resets " + alert.ResetAt.UTC().Format("2 Jan 2006 15:04 MST")
	}
	switch alert.Tier {
	case "pace":
		eta := ""
		if alert.ResetAt != nil {
			eta = " before its " + alert.ResetAt.UTC().Format("2 Jan 2006 15:04 MST") + " reset"
		}
		return fmt.Sprintf("%s · %s: projected to run out%s; %.1f%% remaining.", who, alert.Window, eta, alert.Remaining)
	case "reset":
		return fmt.Sprintf("%s · %s: the window reset; a fresh allowance is available.", who, alert.Window)
	case "expiry", "expiring", "expired":
		expires := "no expiry reported"
		if alert.ExpiresAt != nil {
			expires = alert.ExpiresAt.UTC().Format("2 Jan 2006 15:04 MST")
		}
		tool := alert.Provider
		if tool == "claude-code" {
			tool = "claude"
		}
		if alert.Tier == "expired" {
			return fmt.Sprintf("%s: the access token has expired. Run %s to renew it.", who, tool)
		}
		return fmt.Sprintf("%s: the access token expires %s. Run %s to renew it.", who, expires, tool)
	case "login_rejected":
		// Hint is a full sentence naming the CLI (codex login, claude /login)
		// or the provider's own redacted fix; an older record without one
		// still reads complete. FirstSeen and the credential's hash tail say
		// when the login stopped working and which login it was — not the
		// time the item was raised, which can be later.
		text := ""
		if alert.Hint == "" {
			text = fmt.Sprintf("%s: login rejected: log in again.", who)
		} else {
			text = fmt.Sprintf("%s: login rejected: log in again. %s", who, alert.Hint)
		}
		if alert.FirstSeen != nil && !alert.FirstSeen.IsZero() {
			text += fmt.Sprintf(" First rejected at %s", alert.FirstSeen.UTC().Format("2 Jan 2006 15:04 MST"))
			if alert.TokenTail != "" {
				text += fmt.Sprintf(" (token hash tail %s)", alert.TokenTail)
			}
			text += "."
		}
		return text
	default:
		return fmt.Sprintf("%s · %s: %.1f%% remaining; %s.", who, alert.Window, alert.Remaining, reset)
	}
}

// strongerUsageGuard says whether an active limit guard hides this alert: it
// takes over a tool's quota, so its threshold and pace notices stay hidden
// while it holds. The reset notice and the expiry alert describe facts, not
// quota pressure, and are never hidden.
func (s *Server) strongerUsageGuard(r *http.Request, alert usages.UsageAlert) bool {
	if alert.Tier != "warning" && alert.Tier != "critical" && alert.Tier != "pace" {
		return false
	}
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
	if req.Rev < 0 || req.Choice != "resume" && req.Choice != "retry" && req.Choice != "abandon" && req.Choice != "continue" && req.Choice != "override" && req.Choice != "restart" && req.Choice != "approve" && req.Choice != "deny" {
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
