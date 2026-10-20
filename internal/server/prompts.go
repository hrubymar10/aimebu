package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/hrubymar10/aimebu/internal/adapter/prompt"
	"github.com/hrubymar10/aimebu/internal/store"
)

func promptError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrPromptOverride) {
		apiError(w, 400, "invalid_request", "unknown prompt slot, or a value that is empty or longer than the limit")
		return
	}
	switch {
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "insufficient permission")
	case errors.Is(err, store.ErrStaleRev):
		apiError(w, 409, "stale_rev", "prompt changed; reload it")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "prompt override not found")
	default:
		fail(w, err)
	}
}

// promptScope lets the global scope through (any signed-in user reads it; writes check the owner in the store) and a project scope only for
// someone with access to that project.
func (s *Server) promptScope(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if projectID == "" {
		return true
	}
	return s.projectAccess(w, r, projectID)
}

// GET /api/prompts?project_id=: every overridable prompt slot with its shipped text, the saved global (and project) overrides and
// what a turn would get, plus the inline-plans setting. Any signed-in user reads the global view; a project's view needs access to it.
func (s *Server) prompts(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if !s.promptScope(w, r, projectID) {
		return
	}
	slots, err := s.Store.ListPrompts(r.Context(), projectID)
	if err != nil {
		fail(w, err)
		return
	}
	inline, err := s.Store.InlinePlanAppendix(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	owner, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	canEdit := owner
	if projectID != "" {
		role, err := s.Store.UserProjectRole(r.Context(), userID(r), projectID)
		if err != nil {
			fail(w, err)
			return
		}
		canEdit = role == "owner" || role == "operator"
	}
	writeJSON(w, 200, map[string]any{"slots": slots, "max": prompt.MaxOverrideLen, "inline_plan_appendix": inline, "can_edit": canEdit, "can_edit_global": owner})
}

// PUT /api/prompts/{key} {project_id?, value, rev} saves an override (rev 0 creates); DELETE /api/prompts/{key}?project_id=&rev= resets it.
func (s *Server) promptOverride(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if r.Method == http.MethodDelete {
		projectID := r.URL.Query().Get("project_id")
		if !s.promptScope(w, r, projectID) {
			return
		}
		rev, err := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
		if err != nil {
			apiError(w, 400, "invalid_request", "rev is required")
			return
		}
		if err := s.Store.ResetPromptOverride(r.Context(), userID(r), projectID, key, rev); err != nil {
			promptError(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	var input struct {
		ProjectID string `json:"project_id"`
		Value     string `json:"value"`
		Rev       int64  `json:"rev"`
	}
	if !readJSON(w, r, &input) || !s.promptScope(w, r, input.ProjectID) {
		return
	}
	if err := s.Store.SetPromptOverride(r.Context(), userID(r), input.ProjectID, key, input.Value, input.Rev); err != nil {
		promptError(w, err)
		return
	}
	w.WriteHeader(204)
}

// PUT /api/inline-plans {value: "always"|"optional"} (instance owner only).
func (s *Server) inlinePlans(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Value string `json:"value"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if err := s.Store.SetInlinePlanAppendix(r.Context(), userID(r), input.Value); err != nil {
		promptError(w, err)
		return
	}
	w.WriteHeader(204)
}
