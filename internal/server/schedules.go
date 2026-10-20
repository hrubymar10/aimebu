package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

type scheduleRequest struct {
	TargetAgentID   string `json:"target_agent_id"`
	TargetLead      bool   `json:"target_lead"`
	Body            string `json:"body"`
	Kind            string `json:"kind"`
	Timezone        string `json:"timezone"`
	IntervalSeconds int64  `json:"interval_seconds"`
	LocalTime       string `json:"local_time"`
	Weekday         int    `json:"weekday"`
	At              string `json:"at"`
}

func (s *Server) schedules(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if room.Kind != "task" {
		apiError(w, 409, "invalid_scope", "schedules belong to task rooms")
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.Store.ListSchedules(r.Context(), room.ID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"schedules": items})
		return
	}
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), room.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	if role == "viewer" {
		apiError(w, 403, "forbidden", "owner or operator required")
		return
	}
	var req scheduleRequest
	if !readJSON(w, r, &req) {
		return
	}
	item := store.Schedule{RoomID: room.ID, ProjectID: room.ProjectID, CreatorKind: "user", CreatorID: userID(r), TargetAgentID: req.TargetAgentID, TargetLead: req.TargetLead, Body: req.Body, Kind: req.Kind, Timezone: req.Timezone, IntervalSeconds: req.IntervalSeconds, LocalTime: req.LocalTime, Weekday: req.Weekday, NextRunAt: req.At}
	item, err = s.StoreCreateSchedule(r, item)
	if err != nil {
		scheduleError(w, err)
		return
	}
	writeJSON(w, 201, item)
}
func (s *Server) StoreCreateSchedule(r *http.Request, item store.Schedule) (store.Schedule, error) {
	var created store.Schedule
	err := s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		var err error
		created, err = tx.CreateSchedule(item, time.Now())
		return err
	})
	return created, err
}
func (s *Server) schedule(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), room.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	if role == "viewer" {
		apiError(w, 403, "forbidden", "owner or operator required")
		return
	}
	id := r.PathValue("id")
	if r.Method == http.MethodDelete {
		err = s.Store.Tx(r.Context(), func(tx *store.Tx) error { return tx.DeleteSchedule(id, room.ID) })
		if err != nil {
			scheduleError(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	var req struct {
		Enabled bool  `json:"enabled"`
		Rev     int64 `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var item store.Schedule
	err = s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		var e error
		item, e = tx.SetScheduleEnabled(id, room.ID, req.Enabled, req.Rev, time.Now())
		return e
	})
	if err != nil {
		scheduleError(w, err)
		return
	}
	writeJSON(w, 200, item)
}
func scheduleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrScheduleInvalid):
		apiError(w, 400, "invalid_schedule", "invalid schedule, target, time, or room")
	case errors.Is(err, store.ErrScheduleLimit):
		apiError(w, 409, "schedule_limit", "room has 20 active schedules")
	case errors.Is(err, store.ErrStaleRev):
		apiError(w, 409, "stale_rev", "schedule changed")
	default:
		fail(w, err)
	}
}

func (s *Server) agentToolSchedules(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	roomID := r.PathValue("room")
	room, err := s.Store.RoomByID(r.Context(), roomID)
	if err != nil {
		fail(w, err)
		return
	}
	if room.ProjectID != caller.ProjectID || room.Kind != "task" {
		apiError(w, 403, "forbidden", "task room in your project required")
		return
	}
	joined, err := s.Store.AgentInRoom(r.Context(), caller.ID, roomID)
	if err != nil {
		fail(w, err)
		return
	}
	if !joined {
		apiError(w, 403, "forbidden", "room membership required")
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.Store.ListSchedules(r.Context(), roomID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"schedules": items})
		return
	}
	if !caller.IsLead || caller.Desired != "active" {
		apiError(w, 403, "forbidden", "active lead required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required")
		return
	}
	var req scheduleRequest
	if !readJSON(w, r, &req) {
		return
	}
	item := store.Schedule{RoomID: roomID, ProjectID: room.ProjectID, CreatorKind: "agent", CreatorID: caller.ID, TargetAgentID: req.TargetAgentID, TargetLead: req.TargetLead, Body: req.Body, Kind: req.Kind, Timezone: req.Timezone, IntervalSeconds: req.IntervalSeconds, LocalTime: req.LocalTime, Weekday: req.Weekday, NextRunAt: req.At}
	raw, _ := json.Marshal(item)
	digest := sha256.Sum256(raw)
	result, reused, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "agent", ActorID: caller.ID, Kind: "schedule_create", Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *store.Tx) (string, error) {
		created, e := tx.CreateSchedule(item, time.Now())
		if e != nil {
			return "", e
		}
		data, e := json.Marshal(created)
		return string(data), e
	})
	if err != nil {
		scheduleError(w, err)
		return
	}
	status := 201
	if reused {
		status = 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(result))
}
