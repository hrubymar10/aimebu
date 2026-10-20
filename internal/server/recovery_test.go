package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestRecoveryDecisionEndpointIdempotencyAndRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordProcess(ctx, turn.ID, agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	response := f.request("GET", "/api/inbox", nil, false)
	var items []store.InboxEntry
	if err := json.NewDecoder(response.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(items) != 1 || items[0].TurnID != turn.ID {
		t.Fatalf("inbox=%+v", items)
	}
	post := func(key string, rev int) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"choice": "resume", "rev": rev})
		req, _ := http.NewRequest("POST", f.http.URL+"/api/inbox/"+items[0].ID+"/decision", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, result
	}
	status, first := post("resume-one", 0)
	if status != 201 || first["turn_id"] == "" {
		t.Fatalf("decision=%d %+v", status, first)
	}
	status, replay := post("resume-one", 0)
	if status != 200 || replay["turn_id"] != first["turn_id"] {
		t.Fatalf("replay=%d %+v", status, replay)
	}
	status, stale := post("resume-stale", 0)
	if status != 409 || stale["error"] != "stale_rev" {
		t.Fatalf("stale revision=%d %+v", status, stale)
	}
	status, stale = post("resume-two", 1)
	if status != 409 || stale["error"] != "already_resolved" {
		t.Fatalf("resolved=%d %+v", status, stale)
	}
}

func TestProjectRecoverySettingDefaultsOffAndCanBeEnabled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/projects/" + f.project.ID + "/settings"
	response := f.request("GET", path, nil, false)
	var settings store.ProjectRecoverySettings
	if err := json.NewDecoder(response.Body).Decode(&settings); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || settings.AutoResumeAfterCleanDiagnostic {
		t.Fatalf("default setting=%d %+v", response.StatusCode, settings)
	}
	response = f.request("PUT", path, map[string]bool{"auto_resume_after_clean_diagnostic": true}, true)
	if err := json.NewDecoder(response.Body).Decode(&settings); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || !settings.AutoResumeAfterCleanDiagnostic {
		t.Fatalf("updated setting=%d %+v", response.StatusCode, settings)
	}
}

func TestLostRecoveryWaitingItemOverrideEndpoint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ReconcileMachineHello(ctx, f.machineID, "replacement", []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	inbox := func() []store.InboxEntry {
		t.Helper()
		response := f.request("GET", "/api/inbox", nil, false)
		defer response.Body.Close()
		var items []store.InboxEntry
		if err := json.NewDecoder(response.Body).Decode(&items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	post := func(id, key, choice string, rev int64) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"choice": choice, "rev": rev})
		req, _ := http.NewRequest("POST", f.http.URL+"/api/inbox/"+id+"/decision", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	items := inbox()
	if len(items) != 1 || post(items[0].ID, "lost-resume-api", "resume", items[0].Rev) != 201 {
		t.Fatalf("initial recovery items=%+v", items)
	}
	items = inbox()
	if len(items) != 1 || items[0].RecoveryPhase != "quiescence_wait" || len(items[0].Options) != 2 {
		t.Fatalf("waiting response=%+v", items)
	}
	if status := post(items[0].ID, "lost-override-api", "override", items[0].Rev); status != 201 {
		t.Fatalf("override status=%d", status)
	}
	if items = inbox(); len(items) != 0 {
		t.Fatalf("override left item=%+v", items)
	}
}
