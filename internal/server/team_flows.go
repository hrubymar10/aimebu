package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func (s *Server) teamFlows(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"templates": store.TeamFlowTemplates()})
}

func (s *Server) teamFlowRecommendedModels(w http.ResponseWriter, r *http.Request) {
	rows := store.TeamFlowRecommendedModels()
	machineID, launcherID := r.URL.Query().Get("machine_id"), r.URL.Query().Get("launcher_id")
	if machineID != "" || launcherID != "" {
		if machineID == "" || launcherID == "" {
			apiError(w, 400, "invalid_request", "machine_id and launcher_id must be supplied together")
			return
		}
		launcher, err := s.Store.LauncherByID(r.Context(), machineID, launcherID)
		if err != nil {
			fail(w, err)
			return
		}
		models, err := s.Store.HarnessModels(r.Context(), launcherID)
		if err != nil {
			fail(w, err)
			return
		}
		available := map[string]bool{}
		for _, model := range models {
			available[model.Slug] = true
		}
		filtered := make([]store.FlowModelRecommendationRow, 0, len(rows))
		for _, row := range rows {
			if row.Tool != launcher.Tool {
				continue
			}
			keep := row.Models[:0:0]
			for _, model := range row.Models {
				if available[model.Model] {
					keep = append(keep, model)
				}
			}
			if len(keep) > 0 {
				row.Models = keep
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	writeJSON(w, 200, map[string]any{"rows": rows})
}

func (s *Server) teamFlowState(w http.ResponseWriter, r *http.Request) {
	state, err := s.Store.TeamFlowState(r.Context(), r.PathValue("project"), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, state)
}

func (s *Server) teamFlowPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !teamFlowKeyValid(req.Key) {
		apiError(w, 400, "invalid_request", "unknown team flow")
		return
	}
	preview, err := s.Store.PreviewTeamFlow(r.Context(), r.PathValue("project"), userID(r), req.Key)
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	writeJSON(w, 200, preview)
}

func (s *Server) teamFlowApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key               string              `json:"key"`
		Rev               int64               `json:"rev"`
		RemoveMemberships []store.FlowRemoval `json:"remove_memberships,omitempty"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !teamFlowKeyValid(req.Key) {
		apiError(w, 400, "invalid_request", "unknown team flow")
		return
	}
	result, err := s.Store.ApplyTeamFlow(r.Context(), r.PathValue("project"), userID(r), req.Key, req.Rev, req.RemoveMemberships)
	if errors.Is(err, store.ErrTeamFlowRev) {
		apiError(w, 409, "stale_rev", "team flow changed; reload and try again")
		return
	}
	if errors.Is(err, store.ErrTeamFlowSelection) {
		apiError(w, 409, "stale_conflict", "selected room membership changed; preview again")
		return
	}
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func teamFlowKeyValid(key string) bool {
	for _, item := range store.TeamFlowTemplates() {
		if item.Key == key {
			return true
		}
	}
	return false
}

func (s *Server) referencedMessage(w http.ResponseWriter, r *http.Request) {
	no, err := strconv.ParseInt(r.PathValue("no"), 10, 64)
	if err != nil || no < 1 {
		apiError(w, 400, "invalid_request", "message number must be positive")
		return
	}
	source := r.URL.Query().Get("source_message_id")
	if source == "" {
		apiError(w, 400, "invalid_request", "source_message_id is required")
		return
	}
	message, err := s.Store.ReferencedMessageForUser(r.Context(), userID(r), source, no)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, message)
}
