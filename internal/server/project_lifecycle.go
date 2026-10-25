package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/hrubymar10/aimebu/internal/store"
)

// An archived project is read-only: every change a person makes to it, its rooms, its agents and its Inbox is refused with 409
// project_archived, whatever the route. These are the few changes that stay possible (reopening and deleting it, reading a room, a
// person's own notification preference, stopping or pausing an agent, resolving a recovery or a failed delivery, and taking a worktree
// away so that the project can be deleted). Starting an agent is refused inside the store; a recovery decision there can only abandon,
// and ends with the agent stopped.
func archivedAllows(method, path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 4 && parts[1] == "projects" && method == http.MethodPost && (parts[3] == "reopen" || parts[3] == "archive"):
		return true
	case len(parts) == 3 && parts[1] == "projects" && method == http.MethodDelete:
		return true
	case len(parts) == 4 && parts[1] == "agents" && parts[3] == "control" && method == http.MethodPost:
		// Stopping and pausing an agent change no content; the store refuses starting one.
		return true
	case len(parts) == 4 && parts[1] == "inbox" && method == http.MethodPost && (parts[3] == "decision" || parts[3] == "dismiss"):
		// What resolves a problem that was already there (a recovery decision, a failed delivery) is how a stuck agent is freed so the
		// project can be deleted; the closed gate keeps anything from running meanwhile.
		return true
	case len(parts) == 4 && parts[1] == "rooms" && ((parts[3] == "read" && method == http.MethodPost) || (parts[3] == "preferences" && method == http.MethodPatch) || (parts[3] == "worktree" && method == http.MethodDelete)):
		return true
	}
	return false
}

// archivedGuard refuses a change to an archived project. It reads the id of the room, project, agent, inbox item or message from the
// path (the routes that reach a project all have that shape) and asks whether its project is archived; anything else passes.
func (s *Server) archivedGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 {
			next.ServeHTTP(w, r)
			return
		}
		kind := map[string]string{"projects": "project", "rooms": "room", "agents": "agent", "inbox": "inbox", "messages": "message"}[parts[1]]
		if kind == "" || archivedAllows(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// Whoever may write to the project at all (a member, the instance owner, an admin) is refused with the archive named; anyone else
		// gets what the route itself would answer for an object they cannot see, so an id does not reveal anything.
		s.Auth.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, signed *http.Request) {
			if archived, err := s.Store.ArchivedScope(signed.Context(), userID(signed), kind, parts[2]); err == nil && archived {
				apiError(w, 409, "project_archived", "this project is archived and read-only; reopen it to change anything")
				return
			}
			next.ServeHTTP(w, r)
		})).ServeHTTP(w, r)
	})
}

// POST /api/projects/{project}/archive: the project's owner or an admin (who need not be a member) makes the project read-only and stops
// its agents. An agent that cannot be stopped (its execution is unknown until a recovery decision) is listed in stop_failed and left.
func (s *Server) archiveProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if allowed, _ := s.projectOwnerOrAdmin(w, r, id); !allowed {
		return
	}
	controls, failures, err := s.Store.ArchiveProject(r.Context(), id, userID(r))
	if err != nil {
		s.projectLifecycleError(w, err)
		return
	}
	s.sendRoomControls(r.Context(), controls)
	project, err := s.Store.ProjectByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	view := projectView(project)
	view["stop_failed"] = failures
	if failures == nil {
		view["stop_failed"] = []store.ArchiveFailure{}
	}
	writeJSON(w, 200, view)
}

// POST /api/projects/{project}/reopen: lifts the archive. Agents and schedules stay off until a person starts them again.
func (s *Server) reopenProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if allowed, _ := s.projectOwnerOrAdmin(w, r, id); !allowed {
		return
	}
	if err := s.Store.ReopenProject(r.Context(), id, userID(r)); err != nil {
		s.projectLifecycleError(w, err)
		return
	}
	s.projectState(w, r, id)
}

// DELETE /api/projects/{project} {name}: the project's owner or an admin deletes an archived project and everything in it. The body names
// the project, as the person typed it to confirm.
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if allowed, _ := s.projectOwnerOrAdmin(w, r, id); !allowed {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.Store.DeleteProject(r.Context(), id, req.Name, userID(r)); err != nil {
		s.projectLifecycleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) projectState(w http.ResponseWriter, r *http.Request, id string) {
	project, err := s.Store.ProjectByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, projectView(project))
}

func (s *Server) projectLifecycleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "no such project")
	case errors.Is(err, store.ErrProjectNotArchived):
		apiError(w, 409, "project_not_archived", "archive the project before deleting it")
	case errors.Is(err, store.ErrProjectBusy):
		apiError(w, 409, "project_busy", "an agent may still be running or a worktree is still live: stop the agents (abandon a waiting recovery item in the Inbox), bring the machine of an agent that has not reported stopping online or revoke it, and remove the worktrees first")
	case errors.Is(err, store.ErrProjectNameMismatch):
		apiError(w, 400, "invalid_request", "name must be the project's name")
	case errors.Is(err, store.ErrProjectArchived):
		apiError(w, 409, "project_archived", "this project is archived and read-only; reopen it to change anything")
	default:
		fail(w, err)
	}
}
