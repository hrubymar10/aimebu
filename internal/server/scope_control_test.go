package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
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
