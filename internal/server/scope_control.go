package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

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
	// Keep existing agent worktrees locked while the scope resumes agents in them.
	if req.Action == "resume" {
		names, err := s.Store.ScopeOwnWorktreeNames(r.Context(), kind, id)
		if err != nil {
			fail(w, err)
			return
		}
		for _, name := range names {
			defer s.lockAgentWorktree(name)()
		}
	}
	request, _ := json.Marshal(struct {
		Kind, ID, Action string
		Rev              int64
	}{kind, id, req.Action, req.Rev})
	digest := sha256.Sum256(request)
	result, reused, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "user", ActorID: userID(r),
		Kind: "scope_control", Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *store.Tx) (string, error) {
		state, err := tx.ControlScope(kind, id, req.Action, userID(r), req.Rev)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(state)
		return string(encoded), err
	})
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
