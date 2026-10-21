package server

import (
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
)

func (s *Server) projectRecoverySettings(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	if r.Method == http.MethodGet {
		settings, err := s.Store.GetProjectRecoverySettings(r.Context(), projectID, userID(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, settings)
		return
	}
	var req struct {
		AutoResumeAfterCleanDiagnostic *bool   `json:"auto_resume_after_clean_diagnostic"`
		WorktreeMode                   *string `json:"worktree_mode"`
		HomeLeadOnly                   *bool   `json:"home_lead_only"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.AutoResumeAfterCleanDiagnostic == nil && req.WorktreeMode == nil && req.HomeLeadOnly == nil {
		apiError(w, 400, "invalid_request", "auto_resume_after_clean_diagnostic, worktree_mode or home_lead_only is required")
		return
	}
	if req.WorktreeMode != nil && *req.WorktreeMode != "auto" && *req.WorktreeMode != "off" {
		apiError(w, 400, "invalid_request", "worktree_mode must be auto or off")
		return
	}
	settings, err := s.Store.SetProjectRecoverySettings(r.Context(), projectID, userID(r), store.ProjectSettingsUpdate{AutoResumeAfterCleanDiagnostic: req.AutoResumeAfterCleanDiagnostic, WorktreeMode: req.WorktreeMode, HomeLeadOnly: req.HomeLeadOnly})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, settings)
}
