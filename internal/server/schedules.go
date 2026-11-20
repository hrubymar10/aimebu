package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/cron"
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
	Cron            string `json:"cron"`
	At              string `json:"at"`
}

func (s *Server) schedules(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if !store.SchedulableRoomKind(room.Kind) {
		apiError(w, 409, "invalid_scope", "schedules belong to task rooms and the project home room")
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
	item := store.Schedule{RoomID: room.ID, ProjectID: room.ProjectID, CreatorKind: "user", CreatorID: userID(r), TargetAgentID: req.TargetAgentID, TargetLead: req.TargetLead, Body: req.Body, Kind: req.Kind, Timezone: req.Timezone, IntervalSeconds: req.IntervalSeconds, LocalTime: req.LocalTime, Weekday: req.Weekday, CronExpr: req.Cron, NextRunAt: req.At}
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
		message := "invalid schedule, target, time, or room"
		if detail := err.Error(); detail != store.ErrScheduleInvalid.Error() {
			message = detail // names the cron field that is wrong
		}
		apiError(w, 400, "invalid_schedule", message)
	case errors.Is(err, store.ErrScheduleLimit):
		apiError(w, 409, "schedule_limit", "room has 20 active schedules")
	case errors.Is(err, store.ErrStaleRev):
		apiError(w, 409, "stale_rev", "schedule changed")
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "an agent may delete only a schedule an agent created")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "schedule not found in this room")
	default:
		fail(w, err)
	}
}

// agentScheduleRoom checks that the caller may see a room's schedules: a task or home room of its own project that it belongs to.
func (s *Server) agentScheduleRoom(w http.ResponseWriter, r *http.Request, caller store.AgentToolIdentity) (store.Room, bool) {
	roomID := r.PathValue("room")
	room, err := s.Store.RoomByID(r.Context(), roomID)
	if err != nil {
		fail(w, err)
		return store.Room{}, false
	}
	if room.ProjectID != caller.ProjectID || !store.SchedulableRoomKind(room.Kind) {
		apiError(w, 403, "forbidden", "task or home room in your project required")
		return store.Room{}, false
	}
	joined, err := s.Store.AgentInRoom(r.Context(), caller.ID, roomID)
	if err != nil {
		fail(w, err)
		return store.Room{}, false
	}
	if !joined {
		apiError(w, 403, "forbidden", "room membership required")
		return store.Room{}, false
	}
	return room, true
}

// scheduleManager is the active lead or an active flow coordinator (the dispatcher in the leader-dispatcher flow): the agents that may
// create and delete schedules in the rooms they belong to.
func scheduleManager(w http.ResponseWriter, caller store.AgentToolIdentity) bool {
	if (caller.IsLead || caller.CanManage) && caller.Desired == "active" {
		return true
	}
	apiError(w, 403, "forbidden", "active lead or coordinator required")
	return false
}

// agentToolDeleteSchedule is the delete_schedule tool. An agent removes only schedules agents created; a person's schedule stays theirs.
func (s *Server) agentToolDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	room, ok := s.agentScheduleRoom(w, r, caller)
	if !ok || !scheduleManager(w, caller) {
		return
	}
	id := r.PathValue("id")
	if err := s.Store.Tx(r.Context(), func(tx *store.Tx) error { return tx.DeleteScheduleAsAgent(id, room.ID) }); err != nil {
		scheduleError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) agentToolSchedules(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	room, ok := s.agentScheduleRoom(w, r, caller)
	if !ok {
		return
	}
	roomID := room.ID
	if r.Method == http.MethodGet {
		items, err := s.Store.ListSchedules(r.Context(), roomID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"schedules": items})
		return
	}
	if !scheduleManager(w, caller) {
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
	item := store.Schedule{RoomID: roomID, ProjectID: room.ProjectID, CreatorKind: "agent", CreatorID: caller.ID, TargetAgentID: req.TargetAgentID, TargetLead: req.TargetLead, Body: req.Body, Kind: req.Kind, Timezone: req.Timezone, IntervalSeconds: req.IntervalSeconds, LocalTime: req.LocalTime, Weekday: req.Weekday, CronExpr: req.Cron, NextRunAt: req.At}
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

// cronPreview describes a cron expression and lists its next runs in a zone, for the schedule form as the person types. It reads no room
// and writes nothing; a bad expression or zone is a 200 with `error` set, so the form can show it inline.
func (s *Server) cronPreview(w http.ResponseWriter, r *http.Request) {
	expr, zone := r.URL.Query().Get("expr"), r.URL.Query().Get("timezone")
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" || zone == "Local" {
		writeJSON(w, 200, map[string]any{"error": "unknown time zone"})
		return
	}
	spec, err := cron.Parse(expr)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	next := []string{}
	for _, at := range spec.Upcoming(time.Now(), loc, 5) {
		next = append(next, at.UTC().Format(time.RFC3339))
	}
	if len(next) == 0 {
		writeJSON(w, 200, map[string]any{"error": "this expression never runs"})
		return
	}
	writeJSON(w, 200, map[string]any{"cron": spec.String(), "description": spec.Describe(), "next": next})
}
