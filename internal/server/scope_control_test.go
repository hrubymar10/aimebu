package server

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
)

func TestProjectControlRequiresOwnerAndReplaysWithoutDuplicateNotice(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/projects/" + f.project.ID + "/control"
	post := func(key string, rev int64) (int, store.ScopeControl) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"action": "pause", "rev": rev})
		req, err := http.NewRequest(http.MethodPost, f.http.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result store.ScopeControl
		_ = json.NewDecoder(resp.Body).Decode(&result)
		return resp.StatusCode, result
	}
	status, first := post("scope-1", 0)
	if status != 200 || first.Desired != "paused" || first.Rev != 1 {
		t.Fatalf("first status=%d result=%+v", status, first)
	}
	status, replay := post("scope-1", 0)
	if status != 200 || replay.Rev != first.Rev {
		t.Fatalf("replay status=%d result=%+v", status, replay)
	}
	status, _ = post("scope-2", 0)
	if status != 409 {
		t.Fatalf("stale status=%d", status)
	}
	rooms, err := f.db.RoomsForProject(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	homeID := ""
	for _, room := range rooms {
		if room.Kind == "home" {
			homeID = room.ID
		}
	}
	messages, err := f.db.ListMessagesAfter(context.Background(), homeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, high, err := f.db.EventBounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.EventsAfter(context.Background(), f.room.CreatedBy, 0, high)
	if err != nil {
		t.Fatal(err)
	}
	var notices, controls int
	for _, message := range messages {
		if message.AuthorKind == "system" && message.Body == "project "+f.project.ID+" paused by owner/operator" {
			notices++
		}
	}
	for _, event := range events {
		if event.Kind == "scope_control_requested" {
			controls++
		}
	}
	if notices != 1 || controls != 1 {
		t.Fatalf("notices=%d events=%d", notices, controls)
	}
}

// Resuming a room whose stopped writer finds its folder taken by another writer does not fail the whole resume: the writer is started in a
// worktree of its own, made before the resume.
func TestRoomResumeStartsABlockedWriterInItsOwnWorktree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	initRepo(t, f.project.Folder)
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
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
	control := func(key, action string) (int, store.ScopeControl) {
		t.Helper()
		state, err := f.db.ScopeControl(t.Context(), "room", f.room.ID)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"action": action, "rev": state.Rev})
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/control", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result store.ScopeControl
		_ = json.NewDecoder(resp.Body).Decode(&result)
		return resp.StatusCode, result
	}
	_, first, _ := f.spawnRequest(t, "one", "worker")
	roomPath, err := f.db.AgentCheckoutPath(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := control("stop-room", "stop"); status != 200 {
		t.Fatalf("stop: %d", status)
	}
	// Another writer takes the room's folder while the room is stopped.
	status, late, raw := f.spawnRequest(t, "two", "worker")
	if status != 201 {
		t.Fatalf("late writer %d %+v", status, raw)
	}
	if path, _ := f.db.AgentCheckoutPath(t.Context(), late.ID); path != roomPath {
		t.Fatalf("the late writer should take the room's checkout, got %q", path)
	}
	// A resume without its idempotency key is refused before any worktree is made.
	missing, _ := f.db.ScopeControl(t.Context(), "room", f.room.ID)
	noKey, _ := json.Marshal(map[string]any{"action": "resume", "rev": missing.Rev})
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/control", bytes.NewReader(noKey))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.AddCookie(f.cookie)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 400 {
		t.Fatalf("a resume without a key: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	if _, err := os.Stat(filepath.Join(f.project.Folder, ".aimebu", "worktrees", f.room.ID+"-"+first.Slug)); !os.IsNotExist(err) {
		t.Fatalf("a refused resume must not leave a worktree behind: %v", err)
	}
	status, _ = control("resume-room", "resume")
	if status != 200 {
		t.Fatalf("resume should not fail on the blocked writer: %d", status)
	}
	want := filepath.Join(f.project.Folder, ".aimebu", "worktrees", f.room.ID+"-"+first.Slug)
	if path, err := f.db.AgentCheckoutPath(t.Context(), first.ID); err != nil || path != want {
		t.Fatalf("the blocked writer should be started in its own worktree: %q err=%v", path, err)
	}
	if card, err := f.db.AgentCardByID(t.Context(), first.ID); err != nil || card.Desired != "active" {
		t.Fatalf("the blocked writer should be active again: %+v err=%v", card, err)
	}
	if path, _ := f.db.AgentCheckoutPath(t.Context(), late.ID); path != roomPath {
		t.Fatalf("the other writer keeps the room's checkout, got %q", path)
	}
}
