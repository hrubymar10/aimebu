package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/worktree"
)

// agentRemove (DELETE /api/agents/{id}) removes a stopped agent for good. The project's owner or an operator may. Its worktree of its own, if it
// has one, goes first, by the rules of the worktree's own removal (clean and merged is removed, work in it is kept and the answer says so, the
// branch is kept); then the agent leaves every room, what waited on it in the Inbox is withdrawn and it is revoked. Its messages stay.
func (s *Server) agentRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, err := s.Store.AgentRemovalInfo(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if !s.projectOperator(w, r, info.ProjectID) {
		return
	}
	if archived, err := s.Store.ArchivedScope(r.Context(), userID(r), "agent", id); err == nil && archived {
		apiError(w, 409, "project_archived", "this project is archived and read-only; reopen it to change anything")
		return
	}
	if !info.Stopped() {
		apiError(w, 409, "agent_not_stopped", "stop "+info.Slug+" first: only a stopped agent can be removed")
		return
	}
	removedTree := map[string]any{}
	if own, found, err := s.Store.AgentWorktreeOf(r.Context(), id); err != nil {
		fail(w, err)
		return
	} else if found {
		// The lock a start of this agent holds, kept through the removal below: the agent cannot be started into a folder that is going away.
		defer s.lockAgentWorktree(own.Name)()
		removedTree = map[string]any{"name": own.Name, "path": own.Path, "branch": own.Branch, "state": own.State}
		if own.State != "removed" {
			result, ok := s.removeOwnWorktree(w, r, own)
			if !ok {
				return
			}
			if result.State == "preserved" {
				// Work is in it: the agent stays, and its name with it, until the person has dealt with the worktree on purpose. Removing the agent would free
				// the name, and a new agent of that name would find this work in its folder.
				apiError(w, 409, "worktree_preserved", info.Slug+"'s worktree has work in it ("+result.Reason+"): remove or merge the worktree first, then remove the agent")
				return
			}
			removedTree["state"], removedTree["reason"] = result.State, result.Reason
		}
		// The agent's name is free from here on, so the branch it kept is set aside under another one: a new agent of that name starts from the room's base.
		machine, root, err := s.ownWorktreeHost(r.Context(), own)
		if err != nil {
			worktreeError(w, err)
			return
		}
		retired, err := s.machineWorktree(r.Context(), machine, root, own.Name, "", "retire", false)
		if err != nil {
			worktreeError(w, err)
			return
		}
		if retired.Branch != "" {
			removedTree["kept_branch"] = retired.Branch
		}
	}
	removed, err := s.Store.RemoveAgent(r.Context(), id, userID(r))
	switch {
	case errors.Is(err, store.ErrAgentNotStopped):
		apiError(w, 409, "agent_not_stopped", "stop "+info.Slug+" first: only a stopped agent can be removed")
		return
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "this agent is gone or was removed already")
		return
	case err != nil:
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"agent_id": removed.ID, "slug": removed.Slug, "worktree": removedTree})
}

// removeOwnWorktree removes the worktree an agent works in, as DELETE /api/agents/{id}/worktree does without force: it is refused while an active
// agent uses it, left in place with the reason when there is work in it. It answers the request itself on a failure and says whether it went on.
func (s *Server) removeOwnWorktree(w http.ResponseWriter, r *http.Request, own store.AgentWorktree) (worktree.Result, bool) {
	var none worktree.Result
	inUse, err := s.Store.RoomWorktreeInUse(r.Context(), own.MachineID, own.Path)
	if err != nil {
		fail(w, err)
		return none, false
	}
	if inUse {
		apiError(w, 409, "worktree_in_use", "an active agent still uses this agent's worktree: stop it first")
		return none, false
	}
	machine, root, err := s.ownWorktreeHost(r.Context(), own)
	if err != nil {
		worktreeError(w, err)
		return none, false
	}
	result, err := s.machineWorktree(r.Context(), machine, root, own.Name, own.BaseRef, "remove", false)
	if err != nil {
		worktreeError(w, err)
		return none, false
	}
	if err := s.Store.SetAgentWorktreeState(r.Context(), own.AgentID, result.State); err != nil {
		fail(w, err)
		return none, false
	}
	return result, true
}

// ownWorktreeHost finds the machine an agent's own worktree is on and the project folder there it was made from.
func (s *Server) ownWorktreeHost(ctx context.Context, own store.AgentWorktree) (store.Machine, string, error) {
	room, err := s.Store.RoomByID(ctx, own.RoomID)
	if err != nil {
		return store.Machine{}, "", err
	}
	machine, err := s.Store.MachineByID(ctx, own.MachineID)
	if err != nil {
		return store.Machine{}, "", err
	}
	root, err := s.selectedMachinePath(ctx, room.ProjectID, machine)
	return machine, root, err
}
