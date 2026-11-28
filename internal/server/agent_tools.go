package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

type agentToolContextKey struct{}

func agentToolCaller(r *http.Request) store.AgentToolIdentity {
	caller, _ := r.Context().Value(agentToolContextKey{}).(store.AgentToolIdentity)
	return caller
}

type resumeAgentToolResponse struct {
	store.AgentCard
	Status     string `json:"status,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
}

func (s *Server) requireAgentTool(next http.Handler) http.Handler {
	return s.requireAgentToolScope(next, true)
}

// requireAgentToolMCP guards the MCP endpoint. During a cache keep-alive ping it is not refused: a ping is the real turn's request, so Codex still
// connects to the toolbox and lists its tools (they are part of the cached prompt prefix), and a 403 there is fatal to Codex's MCP client. Every tool
// call made through it still reaches the REST routes behind requireAgentTool, which refuse it with 403 keep_alive_scope (seen by the model as a tool
// error), so the toolbox is as unusable as before.
func (s *Server) requireAgentToolMCP(next http.Handler) http.Handler {
	return s.requireAgentToolScope(next, false)
}

func (s *Server) requireAgentToolScope(next http.Handler, refuseScoped bool) http.Handler {
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
		if refuseScoped && s.keepAliveScoped(caller.ID) {
			// A cache keep-alive ping is running for this agent: it may not act, whatever it was asked in words.
			refuseKeepAliveScope(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentToolContextKey{}, caller)))
	})
}

// recordToolRoomReads notes, before a toolbox read answers, the rooms whose content the answer carries, against the caller's current turn:
// the conversation log records the tool result, and the server withholds that turn from a log reader who may not read one of those rooms
// (Store.TurnWithheldFrom). When it cannot be recorded the read answers with an error instead of the content, so a turn is never shown
// to such a reader with that content in it. It reports whether the handler may go on.
func (s *Server) recordToolRoomReads(w http.ResponseWriter, r *http.Request, roomIDs ...string) bool {
	if err := s.Store.RecordAgentTurnRoomReads(r.Context(), agentToolCaller(r).ID, roomIDs...); err != nil {
		fail(w, err)
		return false
	}
	return true
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
	out := map[string]any{"agents": agents}
	// An active lead also sees the leads of other projects it may message (dm_agent's own rule); nobody else gets the key.
	if caller.IsLead && caller.Desired == "active" {
		others, err := s.Store.OtherLeadersForTool(r.Context(), caller.ID, caller.ProjectID)
		if err != nil {
			fail(w, err)
			return
		}
		out["other_leaders"] = others
	}
	writeJSON(w, http.StatusOK, out)
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
	if !s.recordToolRoomReads(w, r, r.PathValue("room")) {
		return
	}
	writeJSON(w, 200, page)
}

// agentSetRoleRequest is the body of POST /api/agent/agents/{id}/role, the set_role tool.
type agentSetRoleRequest struct {
	RoleKey string `json:"role_key"`
	Rev     int64  `json:"rev"`
}

func (s *Server) agentToolSetRole(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if (!caller.IsLead && !caller.CanManage) || caller.Desired != "active" {
		apiError(w, 403, "forbidden", "active lead role required")
		return
	}
	var req agentSetRoleRequest
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

// agentSpawnRequest is the body of POST /api/agent/agents, the spawn_agent tool. It is decoded strictly, so a field the tool sends must be
// here (TestAgentToolBodiesDecodeStrictly checks every tool's body against its route's type).
type agentSpawnRequest struct {
	RoomID  string `json:"room_id"`
	RoleKey string `json:"role_key"`
	Tool    string `json:"tool"`
	Variant string `json:"variant"`
	Model   string `json:"model"`
	// LauncherID is spawn_agent's launcher_id: the launcher to run the agent on. It is validated like on the person's spawn route (the
	// shared spawnAgentRequest resolves it) and it is one of the harness choices the model-approval gate judges (spawnChoiceInPool).
	LauncherID string `json:"launcher_id"`
	Reason     string `json:"reason"`
	Provider   string `json:"provider"`
	MachineID  string `json:"machine_id"`
	Worktree   bool   `json:"worktree"`
	BaseRef    string `json:"base_ref"`
	// JoinParent is spawn_agent's join_parent: the store allows it to a lead or the flow's dispatcher only (store.SpawnInput.JoinParent).
	JoinParent bool `json:"join_parent"`
	// ApprovedBy names the message of a person that approved this exact harness and model choice outside the role's default pool;
	// without it such a choice is refused (409 model_not_approved).
	ApprovedBy string `json:"approved_by"`
}

// agentStopRequest is the body of POST /api/agent/agents/{id}/stop, the stop_agent tool.
type agentStopRequest struct {
	Rev int64 `json:"rev"`
}

type agentResumeRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) agentToolSpawn(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if (!caller.IsLead && !caller.CanManage) || caller.Desired != "active" {
		apiError(w, http.StatusForbidden, "forbidden", "active lead role required")
		return
	}
	var req agentSpawnRequest
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
	// The model-approval gate: a harness choice outside the role's default pool needs a person's message as approved_by. A person's own
	// spawn (the other route into spawnAgentRequest) is never gated and records no approval.
	approvedBy := ""
	if req.Tool != "" || req.Variant != "" || req.Model != "" || req.LauncherID != "" || req.ApprovedBy != "" {
		inPool, err := s.spawnChoiceInPool(r.Context(), room.ProjectID, req.RoleKey, req)
		if err != nil {
			fail(w, err)
			return
		}
		if !inPool && req.ApprovedBy == "" {
			apiError(w, http.StatusConflict, "model_not_approved", "the harness or model is not in the role's default pool; spawn from the pool, or ask a person to approve this exact choice and cite their message as approved_by")
			return
		}
		if req.ApprovedBy != "" {
			id, err := s.validatedApprovalMessage(r.Context(), room.ProjectID, caller.ID, req.ApprovedBy, req, !inPool)
			if err != nil {
				apiError(w, http.StatusConflict, "model_not_approved", err.Error())
				return
			}
			approvedBy = id
		}
	}
	s.spawnAgentRequest(w, r, room, spawnRequest{RoleKey: req.RoleKey, Tool: req.Tool,
		Variant: req.Variant, LauncherID: req.LauncherID, Model: req.Model, Provider: req.Provider, MachineID: req.MachineID, Worktree: req.Worktree, BaseRef: req.BaseRef, Reason: req.Reason, JoinParent: req.JoinParent, ApprovedBy: approvedBy}, "agent", caller.ID)
}

func (s *Server) agentToolStop(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if (!caller.IsLead && !caller.CanManage) || caller.Desired != "active" {
		apiError(w, http.StatusForbidden, "forbidden", "active lead role required")
		return
	}
	var req agentStopRequest
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

func (s *Server) agentToolResume(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if (!caller.IsLead && !caller.CanManage) || caller.Desired != "active" {
		apiError(w, http.StatusForbidden, "forbidden", "active lead or flow coordinator required")
		return
	}
	var req agentResumeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Reason) > 500 {
		apiError(w, http.StatusBadRequest, "invalid_request", "reason is too long")
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
		Reason  string `json:"reason"`
	}{id, req.Reason})
	digestBytes := sha256.Sum256(requestBytes)
	result, reused, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "agent", ActorID: caller.ID,
		Kind: "control", Key: key, Digest: hex.EncodeToString(digestBytes[:])}, func(tx *store.Tx) (string, error) {
		updated, err := tx.ResumeAgentAsTool(id, caller.ID, req.Reason)
		if err != nil {
			return "", err
		}
		response := resumeAgentToolResponse{AgentCard: updated}
		if updated.BoundaryPending != "" {
			response.Status = "pending_approval"
			response.ApprovalID = updated.BoundaryPending
		}
		encoded, err := json.Marshal(response)
		return string(encoded), err
	})
	if err != nil {
		if errors.Is(err, store.ErrResumeClosedRoom) {
			apiError(w, http.StatusConflict, "room_closed", "reopen the room first")
		} else if errors.Is(err, sql.ErrNoRows) {
			apiError(w, http.StatusNotFound, "not_found", "agent not found")
		} else {
			spawnError(w, err)
		}
		return
	}
	var response resumeAgentToolResponse
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		fail(w, err)
		return
	}
	if !reused && response.Status != "pending_approval" {
		s.sendRoomControls(r.Context(), []store.AgentCard{response.AgentCard})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) agentToolReact(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	id := r.PathValue("message")
	emoji := r.PathValue("emoji")
	if r.Method == http.MethodPut && !validReactionEmoji(emoji) || r.Method == http.MethodDelete && !validReactionRemoval(emoji) {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid emoji")
		return
	}
	// A message the agent may not react to, one in a room it has not joined and one that does not exist, gets the same answer: the status and
	// the text must not tell which message ids or numbers exist in rooms the agent is not in.
	notAvailable := func() { apiError(w, http.StatusForbidden, "forbidden", "message is not available") }
	id, err := s.Store.ResolveMessageRef(r.Context(), id) // a message may be named by its number ("#12") as well as its id
	if errors.Is(err, sql.ErrNoRows) {
		notAvailable()
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	message, err := s.Store.MessageByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		notAvailable()
		return
	}
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
		notAvailable()
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
	if message.Available && !s.recordToolRoomReads(w, r, message.RoomID) {
		return
	}
	writeJSON(w, http.StatusOK, message)
}
