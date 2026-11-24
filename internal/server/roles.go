package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// roleBody is a role write. Roles are global only; a client that still names a project would silently create a global role, so a
// project_id on a write is refused.
type roleBody struct {
	store.RoleDefinition
	ProjectID string `json:"project_id"`
}

func readRole(w http.ResponseWriter, r *http.Request) (store.RoleDefinition, bool) {
	var body roleBody
	if !readJSON(w, r, &body) {
		return store.RoleDefinition{}, false
	}
	if body.ProjectID != "" {
		apiError(w, 400, "invalid_request", "roles are global; project_id is not accepted")
		return store.RoleDefinition{}, false
	}
	return body.RoleDefinition, true
}

func definitionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "insufficient permission")
	case errors.Is(err, store.ErrStaleRev):
		apiError(w, 409, "stale_rev", "definition changed; reload it")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "conflict", "name or key already exists")
	case errors.Is(err, store.ErrRoleNameCollision):
		apiError(w, 409, "role_name_collision", "role key collides with an agent slug or human login; pick another key")
	case errors.Is(err, store.ErrRoleInUse):
		apiError(w, 409, "role_in_use", "stop or reassign agents using this role")
	case errors.Is(err, store.ErrRoleExtended):
		apiError(w, 409, "role_extended", "other roles extend this role; change or delete them first")
	case errors.Is(err, store.ErrRoleExtends):
		apiError(w, 400, "invalid_extends", "extends must name another role and may not make a cycle or chain deeper than 5")
	case errors.Is(err, store.ErrRoleBodyExtension):
		apiError(w, 400, "invalid_extension", fmt.Sprintf("the extension is longer than %d bytes", store.MaxRoleBodyExtensionLen))
	case errors.Is(err, store.ErrInvalidSpawnDefaults):
		apiError(w, 400, "invalid_spawn_defaults", err.Error())
	case errors.Is(err, store.ErrInvalidTransition):
		apiError(w, 400, "invalid_request", "invalid role definition")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "definition not found")
	default:
		fail(w, err)
	}
}

func (s *Server) projectRoleSpawnDefaults(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ProjectRoleSpawnDefaults(r.Context(), r.PathValue("project"), userID(r))
	if err != nil {
		definitionError(w, err)
		return
	}
	writeJSON(w, 200, items)
}

func (s *Server) projectRoleSpawnDefault(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Inherit       bool                       `json:"inherit"`
		DefaultAgents []store.SpawnDefaultChoice `json:"default_agents"`
		PickMode      string                     `json:"pick_mode"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	item, err := s.Store.SetProjectRoleSpawnDefaults(r.Context(), r.PathValue("project"), userID(r), r.PathValue("role"),
		body.Inherit, body.DefaultAgents, body.PickMode)
	if err != nil {
		definitionError(w, err)
		return
	}
	writeJSON(w, 200, item)
}

func (s *Server) roles(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		roles, err := s.Store.ListRoles(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, roles)
		return
	}
	role, ok := readRole(w, r)
	if !ok {
		return
	}
	role.ID = ""
	saved, err := s.Store.ManageRole(r.Context(), userID(r), role, false)
	if err != nil {
		definitionError(w, err)
		return
	}
	writeJSON(w, 201, saved)
}

func (s *Server) role(w http.ResponseWriter, r *http.Request) {
	var input store.RoleDefinition
	if r.Method == http.MethodDelete {
		input.ID = r.PathValue("id")
		rev, err := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
		if err != nil {
			apiError(w, 400, "invalid_request", "rev required")
			return
		}
		input.Rev = rev
	} else {
		var ok bool
		if input, ok = readRole(w, r); !ok {
			return
		}
		input.ID = r.PathValue("id")
	}
	saved, err := s.Store.ManageRole(r.Context(), userID(r), input, r.Method == http.MethodDelete)
	if err != nil {
		definitionError(w, err)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(204)
		return
	}
	writeJSON(w, 200, saved)
}

// roleReset puts a built-in global role back to what shipped (instance owner or an admin).
func (s *Server) roleReset(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Rev int64 `json:"rev"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	saved, err := s.Store.ResetRole(r.Context(), userID(r), r.PathValue("id"), input.Rev)
	if err != nil {
		definitionError(w, err)
		return
	}
	writeJSON(w, 200, saved)
}

// rolePrompts lists every live role as one project's role-prompt settings show it: the base, both saved extensions, and the
// delivered text. Only the project's owner or operators.
func (s *Server) rolePrompts(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.RolePromptSettings(r.Context(), userID(r), r.PathValue("project"))
	if err != nil {
		definitionError(w, err)
		return
	}
	writeJSON(w, 200, items)
}

// rolePrompt saves or clears one project's extension of one role (its role_key in the path). An empty body clears it.
func (s *Server) rolePrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Body string `json:"body"`
		Rev  int64  `json:"rev"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	project, roleKey := r.PathValue("project"), r.PathValue("role")
	roleID, err := s.Store.RoleIDByKey(r.Context(), roleKey)
	if err != nil {
		definitionError(w, err)
		return
	}
	saved, err := s.Store.SetRoleBodyExtension(r.Context(), userID(r), project, roleID, body.Body, body.Rev)
	if err != nil {
		definitionError(w, err)
		return
	}
	writeJSON(w, 200, saved)
}
