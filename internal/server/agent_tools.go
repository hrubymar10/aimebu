package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

type agentToolContextKey struct{}

func agentToolCaller(r *http.Request) store.AgentToolIdentity {
	caller, _ := r.Context().Value(agentToolContextKey{}).(store.AgentToolIdentity)
	return caller
}

func (s *Server) requireAgentTool(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || len(key) != 47 || !strings.HasPrefix(key, "aak_") {
			apiError(w, http.StatusUnauthorized, "unauthorized", "invalid agent credential")
			return
		}
		hashBytes := sha256.Sum256([]byte(key))
		caller, err := s.Store.AgentToolIdentityByKeyHash(r.Context(), hex.EncodeToString(hashBytes[:]))
		if err != nil || s.Auth.VerifyAgentKey(r.Context(), caller.ID, key) != nil {
			apiError(w, http.StatusUnauthorized, "unauthorized", "invalid agent credential")
			return
		}
		if caller.Desired == "stopped" {
			apiError(w, http.StatusForbidden, "forbidden", "stopped agent cannot use toolbox")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentToolContextKey{}, caller)))
	})
}

func (s *Server) agentToolRooms(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	rooms, err := s.Store.RoomsForAgent(r.Context(), caller.ID, caller.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

func (s *Server) agentToolUsageLeft(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	headroom, err := s.Store.ProjectToolHeadroom(r.Context(), caller.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": headroom, "provider": s.agentProviderQuota(caller)})
}

type agentQuotaWindow struct {
	PercentLeft float64    `json:"percent_left"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
}

type agentProviderQuota struct {
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Stale   bool              `json:"stale,omitempty"`
	Session *agentQuotaWindow `json:"session,omitempty"`
	Weekly  *agentQuotaWindow `json:"weekly,omitempty"`
}

func (s *Server) agentProviderQuota(caller store.AgentToolIdentity) agentProviderQuota {
	name := caller.Tool
	if name == "claude" {
		name = "claude-code"
	} else if name != "codex" && name != "claude-code" {
		name = caller.Provider
	}
	quota := agentProviderQuota{Name: name, Status: "unknown"}
	if s.Usages == nil {
		return quota
	}
	for _, provider := range s.Usages.Snapshot().Providers {
		if provider.ProviderName != name {
			continue
		}
		for _, profile := range provider.Profiles {
			if !profile.Active {
				continue
			}
			for _, window := range profile.Windows {
				left := max(0, min(100, 100-window.PercentUsed))
				value := &agentQuotaWindow{PercentLeft: left, ResetAt: window.ResetAt}
				switch window.Key {
				case "session":
					quota.Session = value
				case "weekly":
					quota.Weekly = value
				}
			}
			if quota.Session != nil || quota.Weekly != nil {
				quota.Status = "ok"
				quota.Stale = profile.Stale || profile.Status != "ok"
				if quota.Stale {
					quota.Status = "stale"
				}
			}
			return quota
		}
		return quota
	}
	return quota
}

func (s *Server) agentToolSpawn(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if !caller.IsLead || caller.Desired != "active" {
		apiError(w, http.StatusForbidden, "forbidden", "active lead role required")
		return
	}
	var req struct {
		RoomID    string `json:"room_id"`
		RoleKey   string `json:"role_key"`
		Tool      string `json:"tool"`
		Variant   string `json:"variant"`
		Model     string `json:"model"`
		Provider  string `json:"provider"`
		MachineID string `json:"machine_id"`
		Worktree  bool   `json:"worktree"`
		BaseRef   string `json:"base_ref"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	room, err := s.Store.RoomByID(r.Context(), req.RoomID)
	if err != nil {
		fail(w, err)
		return
	}
	if room.ProjectID != caller.ProjectID {
		apiError(w, http.StatusForbidden, "forbidden", "room belongs to another project")
		return
	}
	s.spawnAgentRequest(w, r, room, spawnRequest{RoleKey: req.RoleKey, Tool: req.Tool,
		Variant: req.Variant, Model: req.Model, Provider: req.Provider, MachineID: req.MachineID, Worktree: req.Worktree, BaseRef: req.BaseRef}, "agent", caller.ID)
}

func (s *Server) agentToolStop(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if !caller.IsLead || caller.Desired != "active" {
		apiError(w, http.StatusForbidden, "forbidden", "active lead role required")
		return
	}
	var req struct {
		Rev int64 `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return
	}
	id := r.PathValue("id")
	requestBytes, _ := json.Marshal(struct {
		AgentID string `json:"agent_id"`
		Rev     int64  `json:"rev"`
	}{id, req.Rev})
	digestBytes := sha256.Sum256(requestBytes)
	result, reused, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "agent", ActorID: caller.ID,
		Kind: "control", Key: key, Digest: hex.EncodeToString(digestBytes[:])}, func(tx *store.Tx) (string, error) {
		updated, err := tx.StopAgentAsTool(id, caller.ID, req.Rev)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(updated)
		return string(encoded), err
	})
	if err != nil {
		spawnError(w, err)
		return
	}
	var updated store.AgentCard
	if err := json.Unmarshal([]byte(result), &updated); err != nil {
		fail(w, err)
		return
	}
	if !reused {
		s.sendRoomControls(r.Context(), []store.AgentCard{updated})
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) agentToolReact(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	id := r.PathValue("message")
	emoji := r.PathValue("emoji")
	if r.Method == http.MethodPut && !validReactionEmoji(emoji) || r.Method == http.MethodDelete && !validReactionRemoval(emoji) {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid emoji")
		return
	}
	message, err := s.Store.MessageByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	rooms, err := s.Store.RoomsForAgent(r.Context(), caller.ID, caller.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	joined := false
	for _, room := range rooms {
		if room.ID == message.RoomID {
			joined = true
			break
		}
	}
	if !joined {
		apiError(w, http.StatusForbidden, "forbidden", "not a member of the room")
		return
	}
	err = s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		return tx.SetReaction(id, "agent", caller.ID, emoji, r.Method == http.MethodPut)
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
