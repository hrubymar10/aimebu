package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// Creating a project makes its home room and a "general" task room beside it.
func TestProjectCreationMakesGeneralTaskRoom(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	resp := f.request(http.MethodPost, "/api/projects", map[string]string{"name": "fresh", "folder": t.TempDir()}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("project: %d", resp.StatusCode)
	}
	var project store.Project
	_ = json.NewDecoder(resp.Body).Decode(&project)
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/projects/"+project.ID+"/rooms", nil, false)
	var rooms []store.Room
	_ = json.NewDecoder(resp.Body).Decode(&rooms)
	resp.Body.Close()
	var home, general store.Room
	for _, room := range rooms {
		switch room.Kind {
		case "home":
			home = room
		case "task":
			general = room
		}
	}
	if len(rooms) != 2 || home.ID == "" || general.Title != "general" || general.ParentID != home.ID || general.Lifecycle != "open" {
		t.Fatalf("rooms of a new project: %+v", rooms)
	}
}

// "Home room: lead only" is on for a new project: the home room refuses a worker with home_is_lead_only and takes the lead, a task room
// takes the worker, and with the setting off the home room takes a worker again.
func TestHomeRoomLeadOnlySpawn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	fake.SetEchoDefault(true)
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not connect")
	}
	settingsPath := "/api/projects/" + f.project.ID + "/settings"
	resp := f.request(http.MethodGet, settingsPath, nil, false)
	var settings store.ProjectRecoverySettings
	_ = json.NewDecoder(resp.Body).Decode(&settings)
	resp.Body.Close()
	if !settings.HomeLeadOnly {
		t.Fatalf("a new project must start lead only: %+v", settings)
	}
	resp = f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil, false)
	var rooms []store.Room
	_ = json.NewDecoder(resp.Body).Decode(&rooms)
	resp.Body.Close()
	task := f.room
	for _, room := range rooms {
		if room.Kind == "home" {
			f.room = room
		}
	}
	if status, _, raw := f.spawnRequest(t, "home-worker", "worker"); status != 409 || raw["error"] != "home_is_lead_only" {
		t.Fatalf("worker into the home room: %d %+v", status, raw)
	}
	if status, card, raw := f.spawnRequest(t, "home-lead", "leader"); status != 201 || card.ID == "" {
		t.Fatalf("lead into the home room: %d %+v", status, raw)
	}
	resp = f.request(http.MethodPut, settingsPath, map[string]any{"home_lead_only": false}, true)
	_ = json.NewDecoder(resp.Body).Decode(&settings)
	resp.Body.Close()
	if resp.StatusCode != 200 || settings.HomeLeadOnly {
		t.Fatalf("turn off: %d %+v", resp.StatusCode, settings)
	}
	if status, card, raw := f.spawnRequest(t, "home-worker-off", "reviewer"); status != 201 || card.ID == "" {
		t.Fatalf("reviewer into the home room with the setting off: %d %+v", status, raw)
	}
	f.room = task
	resp = f.request(http.MethodPut, settingsPath, map[string]any{"home_lead_only": true}, true)
	resp.Body.Close()
	if status, _, raw := f.spawnRequest(t, "task-worker", "worker"); status == 409 && raw["error"] == "home_is_lead_only" {
		t.Fatalf("a task room must take a worker: %+v", raw)
	}
}
