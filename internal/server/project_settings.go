package server

import "net/http"

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
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.AutoResumeAfterCleanDiagnostic == nil && req.WorktreeMode == nil {
		apiError(w, 400, "invalid_request", "auto_resume_after_clean_diagnostic or worktree_mode is required")
		return
	}
	if req.WorktreeMode != nil && *req.WorktreeMode != "auto" && *req.WorktreeMode != "off" {
		apiError(w, 400, "invalid_request", "worktree_mode must be auto or off")
		return
	}
	settings, err := s.Store.SetProjectRecoverySettings(r.Context(), projectID, userID(r), req.AutoResumeAfterCleanDiagnostic, req.WorktreeMode)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, settings)
}
