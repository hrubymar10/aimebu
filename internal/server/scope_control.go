package server

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"slices"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

// usesPreparedWorktree says whether the resume put the agent in the worktree made for it. A record left at the same path by an earlier
// removal (the name is always <room>-<slug>) is not the resume using it: that worktree is unused, and is taken away.
func usesPreparedWorktree(used, prepared store.AgentWorktree) bool {
	return used.Path == prepared.Path && used.State == "ready"
}

// mayPrepareResume says whether a resume could succeed far enough to be worth making worktrees ahead of it: the caller is an owner or
// operator, the revision is current, a room is open and the daily budget has not paused the scope. Anything else is left for the transaction to refuse with its own error, and
// nothing is made first, so a viewer or a stale request leaves no worktree or branch behind.
func (s *Server) mayPrepareResume(r *http.Request, kind, id string, rev int64) bool {
	projectID := id
	if kind == "room" {
		room, err := s.Store.RoomByID(r.Context(), id)
		if err != nil || room.Lifecycle != "open" {
			return false
		}
		projectID = room.ProjectID
	}
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), projectID)
	if err != nil || (role != "owner" && role != "operator") {
		return false
	}
	state, err := s.Store.ScopeControl(r.Context(), kind, id)
	if err != nil || state.Rev != rev {
		return false
	}
	paused, err := s.Store.ScopeBudgetPaused(r.Context(), kind, id)
	return err == nil && !paused
}

func (s *Server) scopeControl(w http.ResponseWriter, r *http.Request) {
	kind, id := "project", r.PathValue("project")
	if id == "" {
		kind, id = "room", r.PathValue("room")
		if _, ok := s.roomAccess(w, r, id); !ok {
			return
		}
	} else if !s.projectAccess(w, r, id) {
		return
	}
	if r.Method == http.MethodGet {
		state, err := s.Store.ScopeControl(r.Context(), kind, id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
		return
	}
	var req struct {
		Action string `json:"action"`
		Rev    int64  `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Action != "pause" && req.Action != "stop" && req.Action != "resume" {
		apiError(w, 400, "invalid_request", "action must be pause, stop, or resume")
		return
	}
	if req.Action == "resume" {
		disabled, needsBind, err := s.Store.ScopeResumeLauncherStatus(r.Context(), kind, id)
		if err != nil {
			fail(w, err)
			return
		}
		if disabled {
			apiError(w, 409, "launcher_unavailable", errLauncherUnavailable.Error())
			return
		}
		if needsBind && dockerBindIsLoopback() {
			apiError(w, 409, "invalid_transition", dockerBindUnavailableMessage)
			return
		}
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return
	}
	// A writer that would find its folder's writer slot taken is started in a worktree of its own, made now, before the transaction: a
	// resume that failed on one agent would leave the whole scope stopped. Where no worktree can be made the refusal stands. Every worktree
	// the resume could touch (those it makes and those of agents that already work in one) is locked once, in name order, before any is made
	// and until the request ends, whether or not this caller may make new ones: the transaction starts the scope's agents either way, and a
	// removal must not take a folder from one it starts. No lock is taken twice, and a skipped writer leaves nothing locked behind.
	prepared := map[string]store.AgentWorktree{}
	if req.Action == "resume" {
		type blockedWriter struct {
			writer  store.ScopeWriter
			room    store.Room
			machine store.Machine
		}
		var blocked []blockedWriter
		names := map[string]bool{}
		if s.mayPrepareResume(r, kind, id, req.Rev) {
			writers, err := s.Store.ScopeResumeWriters(r.Context(), kind, id)
			if err != nil {
				fail(w, err)
				return
			}
			for _, writer := range writers {
				room, roomErr := s.Store.RoomByID(r.Context(), writer.RoomID)
				machine, machineErr := s.Store.MachineByID(r.Context(), writer.MachineID)
				// A room that is not open is not resumed, so nothing is made for it; neither for an agent without a name, whose worktree would
				// be named by a slug picked here and so could not be locked beforehand.
				if roomErr != nil || machineErr != nil || room.Lifecycle != "open" || writer.Slug == "" {
					continue
				}
				blocked = append(blocked, blockedWriter{writer, room, machine})
				names[store.AgentWorktreeName(room.ID, writer.Slug)] = true
			}
		}
		ownNames, err := s.Store.ScopeOwnWorktreeNames(r.Context(), kind, id)
		if err != nil {
			fail(w, err)
			return
		}
		for _, name := range ownNames {
			names[name] = true
		}
		for _, name := range slices.Sorted(maps.Keys(names)) {
			defer s.lockAgentWorktree(name)()
		}
		for _, item := range blocked {
			slug := item.writer.Slug
			if own, _ := s.agentWorktreeForLocked(r.Context(), item.room, item.machine, &slug, "", true); own != nil {
				own.AgentID = item.writer.AgentID
				if s.ownWorktreeUsable(r.Context(), item.writer.AgentID, own) != nil {
					s.discardAgentWorktree(r.Context(), own) // outside the launcher's roots: this agent's refusal stands
					continue
				}
				prepared[item.writer.AgentID] = *own
			}
		}
	}
	request, _ := json.Marshal(struct {
		Kind, ID, Action string
		Rev              int64
	}{kind, id, req.Action, req.Rev})
	digest := sha256.Sum256(request)
	result, reused, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "user", ActorID: userID(r),
		Kind: "scope_control", Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *store.Tx) (string, error) {
		tx.UseScopeWorktrees(prepared)
		state, err := tx.ControlScope(kind, id, req.Action, userID(r), req.Rev)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(state)
		return string(encoded), err
	})
	// A worktree made ahead of the resume that it did not use (the slot was free after all, or the resume failed) is taken away again.
	for agentID, own := range prepared {
		if used, found, lookupErr := s.Store.AgentWorktreeOf(r.Context(), agentID); err != nil || lookupErr != nil || !found || !usesPreparedWorktree(used, own) {
			s.discardAgentWorktree(r.Context(), &own)
		}
	}
	if err != nil {
		spawnError(w, err)
		return
	}
	var state store.ScopeControl
	if err := json.Unmarshal([]byte(result), &state); err != nil {
		fail(w, err)
		return
	}
	if !reused {
		controls := make([]store.AgentCard, 0, len(state.Results))
		for _, item := range state.Results {
			if item.Agent != nil {
				controls = append(controls, *item.Agent)
			}
		}
		s.sendRoomControls(r.Context(), controls)
	}
	writeJSON(w, http.StatusOK, state)
}
