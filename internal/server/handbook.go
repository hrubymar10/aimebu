package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func handbookError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "handbook access denied")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "handbook or project not found")
	case errors.Is(err, store.ErrHandbookSection), errors.Is(err, store.ErrHandbookEdit):
		apiError(w, 400, "invalid_request", "provide a valid section, one of content or patch, and a nonnegative rev")
	case errors.Is(err, store.ErrProjectArchived):
		apiError(w, 409, "project_archived", "project is archived")
	default:
		fail(w, err)
	}
}

func (s *Server) projectHandbook(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if _, err := s.Store.UserProjectRole(r.Context(), userID(r), project); err != nil {
		handbookError(w, err)
		return
	}
	doc, err := s.Store.GetHandbook(r.Context(), project)
	if err != nil {
		handbookError(w, err)
		return
	}
	writeJSON(w, 200, doc)
}

func (s *Server) projectHandbookHistory(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if _, err := s.Store.UserProjectRole(r.Context(), userID(r), project); err != nil {
		handbookError(w, err)
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			apiError(w, 400, "invalid_request", "limit must be 1..100")
			return
		}
		limit = value
	}
	var before int64
	if raw := r.URL.Query().Get("before_rev"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			apiError(w, 400, "invalid_request", "before_rev must be positive")
			return
		}
		before = value
	}
	history, err := s.Store.HandbookRevisions(r.Context(), project, limit, before)
	if err != nil {
		handbookError(w, err)
		return
	}
	writeJSON(w, 200, history)
}

func (s *Server) projectHandbookSnapshot(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if _, err := s.Store.UserProjectRole(r.Context(), userID(r), project); err != nil {
		handbookError(w, err)
		return
	}
	rev, err := strconv.ParseInt(r.PathValue("rev"), 10, 64)
	if err != nil || rev < 1 {
		apiError(w, 400, "invalid_request", "rev must be positive")
		return
	}
	doc, err := s.Store.HandbookSnapshot(r.Context(), project, rev)
	if err != nil {
		handbookError(w, err)
		return
	}
	writeJSON(w, 200, doc)
}

// agentHandbookUpdateRequest is the body of PUT /api/agent/handbook, the handbook_update tool.
type agentHandbookUpdateRequest struct {
	Section string  `json:"section"`
	Content *string `json:"content"`
	Patch   *string `json:"patch"`
	Rev     *int64  `json:"rev"`
}

func (s *Server) agentHandbook(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	access, err := s.Store.HandbookAgentAccess(r.Context(), caller.ProjectID, caller.ID)
	if err != nil {
		handbookError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		doc, err := s.Store.GetHandbook(r.Context(), caller.ProjectID)
		if err != nil {
			handbookError(w, err)
			return
		}
		if access == "limited" {
			doc.Sections = map[string]string{"current_state": doc.Sections["current_state"], "open_backlog": doc.Sections["open_backlog"]}
			doc.Markdown = "## Current state\n" + doc.Sections["current_state"] + "\n\n## Open backlog\n" + doc.Sections["open_backlog"]
		}
		writeJSON(w, 200, doc)
		return
	}
	if access != "full" {
		apiError(w, 403, "forbidden", "only the project lead may update the handbook")
		return
	}
	var req agentHandbookUpdateRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Rev == nil {
		apiError(w, 400, "invalid_request", "rev is required")
		return
	}
	doc, err := s.Store.UpdateHandbook(r.Context(), caller.ProjectID, caller.ID, req.Section, req.Content, req.Patch, *req.Rev)
	if errors.Is(err, store.ErrHandbookRevision) {
		current, getErr := s.Store.GetHandbook(r.Context(), caller.ProjectID)
		if getErr != nil {
			handbookError(w, getErr)
			return
		}
		writeJSON(w, 409, map[string]any{"error": "stale_rev", "current_rev": current.Rev})
		return
	}
	if err != nil {
		handbookError(w, err)
		return
	}
	writeJSON(w, 200, doc)
}
