package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
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

func (s *Server) agentToolAgents(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	agents, err := s.Store.ProjectAgentsForTool(r.Context(), caller.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) agentToolRoomHistory(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	before, limit := int64(0), 50
	if raw := r.URL.Query().Get("before_no"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			apiError(w, 400, "invalid_request", "before_no must be a positive integer")
			return
		}
		before = value
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			apiError(w, 400, "invalid_request", "limit must be between 1 and 100")
			return
		}
		limit = value
	}
	page, err := s.Store.AgentRoomHistory(r.Context(), caller.ID, caller.ProjectID, r.PathValue("room"), before, limit)
	if err == store.ErrForbidden {
		apiError(w, 403, "forbidden", "room is not available")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func (s *Server) agentToolSetRole(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if !caller.IsLead || caller.Desired != "active" {
		apiError(w, 403, "forbidden", "active lead role required")
		return
	}
	var req struct {
		RoleKey string `json:"role_key"`
		Rev     int64  `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.RoleKey == "" || req.Rev < 0 {
		apiError(w, 400, "invalid_request", "role_key and nonnegative rev required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return
	}
	id := r.PathValue("id")
	requestBytes, _ := json.Marshal(struct {
		AgentID string `json:"agent_id"`
		RoleKey string `json:"role_key"`
		Rev     int64  `json:"rev"`
	}{id, req.RoleKey, req.Rev})
	digestBytes := sha256.Sum256(requestBytes)
	result, reused, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "agent", ActorID: caller.ID, Kind: "control", Key: key, Digest: hex.EncodeToString(digestBytes[:])}, func(tx *store.Tx) (string, error) {
		card, err := tx.SetAgentRoleAsTool(id, caller.ID, req.RoleKey, req.Rev)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(card)
		return string(encoded), err
	})
	if err != nil {
		spawnError(w, err)
		return
	}
	var card store.AgentCard
	if err := json.Unmarshal([]byte(result), &card); err != nil {
		fail(w, err)
		return
	}
	if !reused {
		s.sendRoomControls(r.Context(), []store.AgentCard{card})
	}
	writeJSON(w, 200, card)
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
		Reason    string `json:"reason"`
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
		Variant: req.Variant, Model: req.Model, Provider: req.Provider, MachineID: req.MachineID, Worktree: req.Worktree, BaseRef: req.BaseRef, Reason: req.Reason}, "agent", caller.ID)
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
	id, err := s.Store.ResolveMessageRef(r.Context(), id) // a message may be named by its number ("#12") as well as its id
	if err != nil {
		fail(w, err)
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

// GET /api/agent/message-numbers/{no}: the message_get tool. A message is returned only from a room of the agent's own project (and a
// private chat only to a member of it); any other number, one that does not exist included, answers the same "not available" with nothing else.
func (s *Server) agentToolMessageByNo(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	no, err := strconv.ParseInt(r.PathValue("no"), 10, 64)
	if err != nil || no < 1 {
		apiError(w, http.StatusBadRequest, "invalid_request", "message number must be a positive integer")
		return
	}
	message, err := s.Store.AgentMessageByNo(r.Context(), caller.ID, caller.ProjectID, no)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}
