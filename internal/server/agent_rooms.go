package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func activeLead(w http.ResponseWriter, caller store.AgentToolIdentity) bool {
	if (caller.IsLead || caller.CanManage) && caller.Desired == "active" {
		return true
	}
	apiError(w, http.StatusForbidden, "forbidden", "active lead role required")
	return false
}

func toolRequestKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return "", false
	}
	return key, true
}

func toolDigest(value any) string {
	data, _ := json.Marshal(value)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (s *Server) agentToolCreateRoom(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if !activeLead(w, caller) {
		return
	}
	key, ok := toolRequestKey(w, r)
	if !ok {
		return
	}
	var req struct {
		Title    string          `json:"title"`
		ParentID string          `json:"parent_id"`
		Purpose  string          `json:"purpose,omitempty"`
		Worktree worktreeRequest `json:"worktree"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !store.ValidRoomPurpose(req.Purpose) {
		apiError(w, 400, "invalid_request", "purpose must be task, research or general")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || len(req.Title) > 200 {
		apiError(w, 400, "invalid_request", "title must be 1 to 200 bytes")
		return
	}
	result, _, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "agent", ActorID: caller.ID,
		Kind: "room_create", Key: key, Digest: toolDigest(req)}, func(tx *store.Tx) (string, error) {
		room, err := tx.CreateTaskRoomAsTool(caller.ID, caller.ProjectID, req.ParentID, req.Title, req.Purpose)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(room)
		return string(data), err
	})
	if err != nil {
		toolRoomError(w, err)
		return
	}
	var room store.Room
	if err := json.Unmarshal([]byte(result), &room); err != nil {
		fail(w, err)
		return
	}
	if req.Worktree.Enabled {
		host, err := s.Store.HostMachine(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		item, workErr := s.ensureRoomWorktree(r.Context(), room, host, req.Worktree.BaseRef)
		if workErr != nil {
			item.State, item.Reason = "failed", workErr.Error()
		}
		room.Worktree = &item
	}
	writeJSON(w, 201, room)
}

func (s *Server) agentToolCloseRoom(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if !activeLead(w, caller) {
		return
	}
	key, ok := toolRequestKey(w, r)
	if !ok {
		return
	}
	var req struct {
		LifecycleRev int64 `json:"lifecycle_rev"`
		Cascade      bool  `json:"cascade"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	roomID := r.PathValue("room")
	target, err := s.Store.RoomByID(r.Context(), roomID)
	if err != nil {
		toolRoomError(w, err)
		return
	}
	if target.ProjectID != caller.ProjectID {
		apiError(w, http.StatusForbidden, "forbidden", "room belongs to another project")
		return
	}
	result, reused, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "agent", ActorID: caller.ID,
		Kind: "room_close", Key: key, Digest: toolDigest(struct {
			RoomID       string `json:"room_id"`
			LifecycleRev int64  `json:"lifecycle_rev"`
			Cascade      bool   `json:"cascade"`
		}{roomID, req.LifecycleRev, req.Cascade})}, func(tx *store.Tx) (string, error) {
		room, controls, err := tx.CloseTaskRoomAsTool(roomID, caller.ID, req.LifecycleRev, req.Cascade)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(struct {
			Room     store.Room        `json:"room"`
			Controls []store.AgentCard `json:"controls"`
		}{room, controls})
		return string(data), err
	})
	if err != nil {
		toolRoomError(w, err)
		return
	}
	var response struct {
		Room     store.Room        `json:"room"`
		Controls []store.AgentCard `json:"controls"`
	}
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		fail(w, err)
		return
	}
	if !reused {
		s.sendRoomControls(r.Context(), response.Controls)
	}
	writeJSON(w, http.StatusAccepted, response.Room)
}

func toolRoomError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "you must belong to the parent or task room, or have created the task room")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "room not found")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "idempotency_conflict", "key reused with different request")
	case errors.Is(err, store.ErrDepthExceeded):
		apiError(w, 400, "depth_exceeded", "room depth exceeded")
	default:
		roomLifecycleError(w, err)
	}
}
