package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func (f *fixture) patchRoomPreference(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	resp := f.request(http.MethodPatch, "/api/rooms/"+f.room.ID+"/preferences", body, true)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *fixture) roomDetail(t *testing.T) store.Room {
	t.Helper()
	resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID, nil, false)
	defer resp.Body.Close()
	var room store.Room
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil || resp.StatusCode != 200 {
		t.Fatalf("room detail: %d %v", resp.StatusCode, err)
	}
	return room
}

func TestRoomNotificationPreferenceEndpoint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if room := f.roomDetail(t); room.Notify != "default" || room.MutedUntil != "" {
		t.Fatalf("a fresh room: notify=%q muted_until=%q", room.Notify, room.MutedUntil)
	}
	until := time.Now().Add(8 * time.Hour).UTC().Format(time.RFC3339)
	status, out := f.patchRoomPreference(t, map[string]any{"notify": "mentions", "muted_until": until})
	if status != 200 || out["notify"] != "mentions" || out["muted_until"] != until || out["hidden"] != false {
		t.Fatalf("set: %d %+v", status, out)
	}
	if room := f.roomDetail(t); room.Notify != "mentions" || room.MutedUntil != until {
		t.Fatalf("detail after set: %+v", room)
	}
	// Hiding keeps the notification settings, and an empty time ends the mute.
	if status, out = f.patchRoomPreference(t, map[string]any{"hidden": true}); status != 200 || out["notify"] != "mentions" || out["muted_until"] != until || out["hidden"] != true {
		t.Fatalf("hide: %d %+v", status, out)
	}
	if status, out = f.patchRoomPreference(t, map[string]any{"muted_until": ""}); status != 200 || out["muted_until"] != nil || out["notify"] != "mentions" {
		t.Fatalf("unmute: %d %+v", status, out)
	}
}

func TestRoomNotificationPreferenceRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for name, body := range map[string]map[string]any{
		"nothing to change": {},
		"unknown level":     {"notify": "loud"},
		"mute in the past":  {"muted_until": past},
		"not a time":        {"muted_until": "tomorrow"},
	} {
		if status, out := f.patchRoomPreference(t, body); status != 400 {
			t.Fatalf("%s: %d %+v, want 400", name, status, out)
		}
	}
	if room := f.roomDetail(t); room.Notify != "default" || room.MutedUntil != "" {
		t.Fatalf("a refused request changed the room: %+v", room)
	}
}
