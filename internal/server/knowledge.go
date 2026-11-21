package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func knowledgeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrMemoryDisabled):
		apiError(w, 409, "memory_disabled", "enable project memory before adding or editing it")
	case errors.Is(err, store.ErrKnowledgeLimit):
		apiError(w, 413, "knowledge_limit", "knowledge body or scope exceeds its size limit")
	case errors.Is(err, store.ErrStaleRev):
		apiError(w, 409, "stale_rev", "record changed; reload it")
	case errors.Is(err, store.ErrInvalidScope):
		apiError(w, 409, "invalid_scope", "knowledge scope must be a project or task room")
	case errors.Is(err, store.ErrInvalidTransition):
		apiError(w, 400, "invalid_request", "invalid knowledge body or revision")
	case errors.Is(err, store.ErrMemorySource):
		apiError(w, 400, "invalid_memory_source", "source message must be in a joined, memory-enabled room of this project")
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "not allowed to manage knowledge")
	default:
		fail(w, err)
	}
}

func (s *Server) roomMemoryEnabled(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	var req struct {
		Enabled json.RawMessage `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		apiError(w, 400, "invalid_request", "enabled is required")
		return
	}
	var enabled *bool
	if err := json.Unmarshal(req.Enabled, &enabled); err != nil {
		apiError(w, 400, "invalid_request", "enabled must be false or null")
		return
	}
	value, err := s.Store.SetRoomMemoryEnabled(r.Context(), room.ID, userID(r), enabled)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"memory_enabled": value})
}

func (s *Server) projectMemory(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), projectID)
	if err != nil {
		fail(w, err)
		return
	}
	if r.Method == http.MethodGet {
		enabled, err := s.Store.ProjectMemoryEnabled(r.Context(), projectID)
		if err != nil {
			fail(w, err)
			return
		}
		records, err := s.Store.ListKnowledge(r.Context(), "project", projectID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"enabled": enabled, "can_edit": role != "viewer", "records": records, "cap_bytes": 4096})
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	item, err := s.Store.ManageKnowledge(r.Context(), "project", projectID, "user", userID(r), "", 0, req.Body, false)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 201, item)
}

func (s *Server) projectMemoryEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		apiError(w, 400, "invalid_request", "enabled is required")
		return
	}
	enabled, err := s.Store.SetProjectMemoryEnabled(r.Context(), r.PathValue("project"), userID(r), *req.Enabled)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"enabled": enabled})
}

func (s *Server) projectMemoryRecord(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
		Rev  int64  `json:"rev"`
	}
	if r.Method == http.MethodDelete {
		rev, err := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
		if err != nil {
			apiError(w, 400, "invalid_request", "rev is required")
			return
		}
		req.Rev = rev
	} else if !readJSON(w, r, &req) {
		return
	}
	item, err := s.Store.ManageKnowledge(r.Context(), "project", r.PathValue("project"), "user", userID(r), r.PathValue("id"), req.Rev, req.Body, r.Method == http.MethodDelete)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) globalMemory(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	if scope != "user_profile" && scope != "agent_shared_notes" {
		apiError(w, 404, "not_found", "unknown global knowledge scope")
		return
	}
	projectID, err := s.Store.GlobalKnowledgeAccess(r.Context(), userID(r))
	if err != nil {
		knowledgeError(w, err)
		return
	}
	owner, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		knowledgeError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.Store.ListAllGlobalKnowledge(r.Context(), scope)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		cap := 4096
		if scope == "user_profile" {
			cap = 2048
		}
		writeJSON(w, 200, map[string]any{"records": items, "can_edit": owner, "cap_bytes": cap})
		return
	}
	var req struct {
		ScopeKey string `json:"scope_key"`
		Body     string `json:"body"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	item, err := s.Store.ManageGlobalKnowledge(r.Context(), scope, req.ScopeKey, projectID, "user", userID(r), "", 0, req.Body, false)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 201, item)
}

func (s *Server) globalMemoryRecord(w http.ResponseWriter, r *http.Request) {
	projectID, err := s.Store.GlobalKnowledgeAccess(r.Context(), userID(r))
	if err != nil {
		knowledgeError(w, err)
		return
	}
	var req struct {
		ScopeKey string `json:"scope_key"`
		Body     string `json:"body"`
		Rev      int64  `json:"rev"`
	}
	if r.Method == http.MethodDelete {
		req.ScopeKey = r.URL.Query().Get("scope_key")
		req.Rev, err = strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
		if err != nil {
			apiError(w, 400, "invalid_request", "rev is required")
			return
		}
	} else if !readJSON(w, r, &req) {
		return
	}
	item, err := s.Store.ManageGlobalKnowledge(r.Context(), r.PathValue("scope"), req.ScopeKey, projectID, "user", userID(r), r.PathValue("id"), req.Rev, req.Body, r.Method == http.MethodDelete)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) globalMemoryActivation(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.GlobalKnowledgeAccess(r.Context(), userID(r)); err != nil {
		knowledgeError(w, err)
		return
	}
	var req struct {
		ScopeKey string `json:"scope_key"`
		Rev      int64  `json:"rev"`
		Active   *bool  `json:"active"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Active == nil {
		apiError(w, 400, "invalid_request", "active is required")
		return
	}
	item, err := s.Store.SetGlobalKnowledgeActive(r.Context(), r.PathValue("scope"), req.ScopeKey, userID(r), r.PathValue("id"), req.Rev, *req.Active)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) taskNotes(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if room.Kind != "task" {
		apiError(w, 409, "invalid_scope", "task notes belong to task rooms")
		return
	}
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), room.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	if r.Method == http.MethodGet {
		records, err := s.Store.ListKnowledge(r.Context(), "task", room.ID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"can_edit": role != "viewer", "records": records, "cap_bytes": 4096})
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	item, err := s.Store.ManageKnowledge(r.Context(), "task", room.ID, "user", userID(r), "", 0, req.Body, false)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 201, item)
}

func (s *Server) taskNote(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if room.Kind != "task" {
		apiError(w, 409, "invalid_scope", "task notes belong to task rooms")
		return
	}
	var req struct {
		Body string `json:"body"`
		Rev  int64  `json:"rev"`
	}
	if r.Method == http.MethodDelete {
		rev, err := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
		if err != nil {
			apiError(w, 400, "invalid_request", "rev is required")
			return
		}
		req.Rev = rev
	} else if !readJSON(w, r, &req) {
		return
	}
	item, err := s.Store.ManageKnowledge(r.Context(), "task", room.ID, "user", userID(r), r.PathValue("id"), req.Rev, req.Body, r.Method == http.MethodDelete)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) agentToolMemoryAdd(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	var req struct {
		Scope           string `json:"scope"`
		ScopeKey        string `json:"scope_key"`
		RoomID          string `json:"room_id"`
		Body            string `json:"body"`
		SourceMessageID string `json:"source_message_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	scopeID := caller.ProjectID
	if req.Scope == "task" {
		scopeID = req.RoomID
	} else if req.Scope == "user_profile" || req.Scope == "agent_shared_notes" {
		item, err := s.Store.ManageGlobalKnowledgeWithSource(r.Context(), req.Scope, req.ScopeKey, caller.ProjectID, "agent", caller.ID, "", 0, req.Body, false, req.SourceMessageID)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		writeJSON(w, 201, item)
		return
	} else if req.Scope != "project" {
		apiError(w, 400, "invalid_request", "scope must be project, task, user_profile, or agent_shared_notes")
		return
	}
	item, err := s.Store.ManageKnowledgeWithSource(r.Context(), req.Scope, scopeID, "agent", caller.ID, "", 0, req.Body, false, req.SourceMessageID)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 201, item)
}

func agentMemoryScope(r *http.Request) (string, string, error) {
	caller := agentToolCaller(r)
	scope := r.URL.Query().Get("scope")
	switch scope {
	case "project":
		return scope, caller.ProjectID, nil
	case "task":
		roomID := r.URL.Query().Get("room_id")
		if roomID == "" {
			return "", "", store.ErrInvalidScope
		}
		return scope, roomID, nil
	case "user_profile", "agent_shared_notes":
		key := r.URL.Query().Get("scope_key")
		if key == "" || scope == "agent_shared_notes" && key != "global" {
			return "", "", store.ErrInvalidScope
		}
		return scope, key, nil
	default:
		return "", "", store.ErrInvalidScope
	}
}

func (s *Server) agentToolMemoryList(w http.ResponseWriter, r *http.Request) {
	scope, key, err := agentMemoryScope(r)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	caller := agentToolCaller(r)
	if scope == "project" || scope == "task" {
		if err := s.Store.KnowledgeAgentAccess(r.Context(), scope, key, caller.ID); err != nil {
			knowledgeError(w, err)
			return
		}
		items, err := s.Store.ListKnowledge(r.Context(), scope, key)
		if err != nil {
			knowledgeError(w, err)
			return
		}
		if scope == "project" {
			active := make([]store.KnowledgeRecord, 0, len(items))
			for _, item := range items {
				if item.Active {
					active = append(active, item)
				}
			}
			items = active
		}
		writeJSON(w, 200, map[string]any{"records": items, "cap_bytes": 4096})
		return
	}
	if enabled, err := s.Store.ProjectMemoryEnabled(r.Context(), caller.ProjectID); err != nil {
		knowledgeError(w, err)
		return
	} else if !enabled {
		knowledgeError(w, store.ErrMemoryDisabled)
		return
	}
	// A person's profile is read by the agents of the projects that person belongs to, and by no others.
	if scope == "user_profile" {
		if member, err := s.Store.IsProjectMember(r.Context(), caller.ProjectID, key); err != nil {
			knowledgeError(w, err)
			return
		} else if !member {
			knowledgeError(w, store.ErrForbidden)
			return
		}
	}
	items, err := s.Store.ListGlobalKnowledge(r.Context(), scope, key)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	active := make([]store.KnowledgeRecord, 0, len(items))
	for _, item := range items {
		if item.Active {
			active = append(active, item)
		}
	}
	cap := 4096
	if scope == "user_profile" {
		cap = 2048
	}
	writeJSON(w, 200, map[string]any{"records": active, "cap_bytes": cap})
}

func (s *Server) agentToolMemoryRecord(w http.ResponseWriter, r *http.Request) {
	scope, key, err := agentMemoryScope(r)
	if err != nil {
		knowledgeError(w, err)
		return
	}
	caller := agentToolCaller(r)
	var req struct {
		Body string `json:"body"`
		Rev  int64  `json:"rev"`
	}
	if r.Method == http.MethodDelete {
		req.Rev, err = strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
		if err != nil {
			apiError(w, 400, "invalid_request", "rev is required")
			return
		}
	} else if !readJSON(w, r, &req) {
		return
	}
	var item store.KnowledgeRecord
	if scope == "project" || scope == "task" {
		item, err = s.Store.ManageKnowledge(r.Context(), scope, key, "agent", caller.ID, r.PathValue("id"), req.Rev, req.Body, r.Method == http.MethodDelete)
	} else {
		item, err = s.Store.ManageGlobalKnowledge(r.Context(), scope, key, caller.ProjectID, "agent", caller.ID, r.PathValue("id"), req.Rev, req.Body, r.Method == http.MethodDelete)
	}
	if errors.Is(err, store.ErrStaleRev) {
		var records []store.KnowledgeRecord
		if scope == "project" || scope == "task" {
			records, _ = s.Store.ListKnowledge(r.Context(), scope, key)
		} else {
			records, _ = s.Store.ListGlobalKnowledge(r.Context(), scope, key)
		}
		for _, current := range records {
			if current.ID == r.PathValue("id") {
				writeJSON(w, 409, map[string]any{"error": "stale_rev", "current": current})
				return
			}
		}
	}
	if err != nil {
		knowledgeError(w, err)
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) agentToolRecall(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			apiError(w, 400, "invalid_request", "limit must be an integer")
			return
		}
	}
	hits, err := s.Store.RecallKnowledge(r.Context(), agentToolCaller(r).ID, r.URL.Query().Get("query"), limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 401, "unauthorized", "agent not found")
		} else {
			knowledgeError(w, err)
		}
		return
	}
	writeJSON(w, 200, map[string]any{"results": hits})
}
