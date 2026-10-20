package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestScheduleHTTPCRUDAndValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID + "/schedules"
	body := map[string]any{"target_lead": true, "body": "review the room", "kind": "interval", "timezone": "UTC", "interval_seconds": 300}
	resp := f.request(http.MethodPost, path, body, true)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	var created store.Schedule
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !created.Enabled || created.NextRunAt == "" || created.CreatorKind != "user" {
		t.Fatalf("created=%+v", created)
	}
	resp = f.request(http.MethodGet, path, nil, false)
	var listed struct {
		Schedules []store.Schedule `json:"schedules"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(listed.Schedules) != 1 {
		t.Fatalf("list status=%d items=%d", resp.StatusCode, len(listed.Schedules))
	}
	resp = f.request(http.MethodPatch, path+"/"+created.ID, map[string]any{"enabled": false, "rev": created.Rev}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("pause status=%d", resp.StatusCode)
	}
	var paused store.Schedule
	_ = json.NewDecoder(resp.Body).Decode(&paused)
	resp.Body.Close()
	if paused.Enabled {
		t.Fatal("still enabled")
	}
	resp = f.request(http.MethodPatch, path+"/"+created.ID, map[string]any{"enabled": true, "rev": created.Rev}, true)
	if resp.StatusCode != 409 {
		t.Fatalf("stale revision status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodDelete, path+"/"+created.ID, nil, true)
	if resp.StatusCode != 204 {
		t.Fatalf("delete status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPost, path, map[string]any{"target_lead": true, "body": "too frequent", "kind": "interval", "timezone": "UTC", "interval_seconds": 60}, true)
	if resp.StatusCode != 400 {
		t.Fatalf("short interval status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPost, path, map[string]any{"target_lead": true, "body": "once", "kind": "once", "timezone": "UTC", "at": time.Now().Add(time.Hour).Format(time.RFC3339)}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("once status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLeadToolScheduleIsScopedAndIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_lead_default")
	path := "/api/agent/rooms/" + f.room.ID + "/schedules"
	body := map[string]any{"target_lead": true, "body": "check status", "kind": "interval", "timezone": "UTC", "interval_seconds": 300}
	first := f.toolPost(t, path, key, "schedule-key", body)
	if first.StatusCode != 201 {
		t.Fatalf("first status=%d", first.StatusCode)
	}
	var item store.Schedule
	_ = json.NewDecoder(first.Body).Decode(&item)
	first.Body.Close()
	if item.CreatorID != lead.ID || item.CreatorKind != "agent" {
		t.Fatalf("creator=%+v", item)
	}
	replay := f.toolPost(t, path, key, "schedule-key", body)
	if replay.StatusCode != 200 {
		t.Fatalf("replay status=%d", replay.StatusCode)
	}
	replay.Body.Close()
	items := f.toolRequest(t, path, key)
	var listed struct {
		Schedules []store.Schedule `json:"schedules"`
	}
	_ = json.NewDecoder(items.Body).Decode(&listed)
	items.Body.Close()
	if items.StatusCode != 200 || len(listed.Schedules) != 1 {
		t.Fatalf("list status=%d count=%d", items.StatusCode, len(listed.Schedules))
	}
	noKey := f.toolPost(t, path, key, "", body)
	if noKey.StatusCode != 400 {
		t.Fatalf("missing key status=%d", noKey.StatusCode)
	}
	noKey.Body.Close()
}
