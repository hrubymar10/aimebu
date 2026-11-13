package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestTeamSetupPreviewCreateAndReplay(t *testing.T) {
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{}, capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	path := "/api/projects/" + f.project.ID + "/team-setup"
	body := map[string]any{"flow_key": "leader-workers", "flow_rev": 0, "leader": map[string]any{"machine_id": f.machineID, "tool": "fake", "variant": "test", "model": "test"}, "settings": map[string]any{"worktree_mode": "off", "memory_enabled": true, "max_live_agents": 3}}
	preview := f.request(http.MethodPost, path+"/preview", body, true)
	var previewData map[string]any
	if err := json.NewDecoder(preview.Body).Decode(&previewData); err != nil {
		t.Fatal(err)
	}
	preview.Body.Close()
	if preview.StatusCode != 200 || previewData["can_create"] != true {
		t.Fatalf("preview %d %+v", preview.StatusCode, previewData)
	}
	noSettings := map[string]any{"flow_key": body["flow_key"], "flow_rev": body["flow_rev"], "leader": body["leader"]}
	defaultPreview := f.request(http.MethodPost, path+"/preview", noSettings, true)
	var defaults map[string]any
	if err := json.NewDecoder(defaultPreview.Body).Decode(&defaults); err != nil {
		t.Fatal(err)
	}
	defaultPreview.Body.Close()
	values, _ := defaults["settings"].(map[string]any)
	if defaultPreview.StatusCode != 200 || values["worktree_mode"] != "auto" || values["memory_enabled"] != false || values["max_live_agents"] != float64(10) {
		t.Fatalf("effective defaults %d %+v", defaultPreview.StatusCode, defaults)
	}
	owner, err := f.db.UserByLogin(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.db.TeamFlowState(t.Context(), f.project.ID, owner.ID)
	if err != nil || before.Rev != 0 || before.Key != "" {
		t.Fatalf("preview changed flow %+v %v", before, err)
	}
	request := func(key string, payload any) (int, store.TeamSetupResult, map[string]any) {
		t.Helper()
		encoded, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(raw)
		var result store.TeamSetupResult
		_ = json.Unmarshal(data, &result)
		return resp.StatusCode, result, raw
	}
	status, created, raw := request("team-1", body)
	if status != 201 || created.Flow.Rev != 1 || len(created.Agents) != 1 || created.Agents[0].Role != "leader" {
		t.Fatalf("create %d %+v %+v", status, created, raw)
	}
	status, replay, raw := request("team-1", body)
	if status != 200 || len(replay.Agents) != 1 || replay.Agents[0].AgentID != created.Agents[0].AgentID {
		t.Fatalf("replay %d %+v %+v", status, replay, raw)
	}
	changed := map[string]any{"flow_key": "leader-dispatcher", "flow_rev": 0, "leader": body["leader"], "dispatcher": body["leader"]}
	if status, _, raw := request("team-1", changed); status != 409 || raw["error"] != "idempotency_conflict" {
		t.Fatalf("conflicting key %d %+v", status, raw)
	}
	settingsResp := f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/settings", map[string]any{"max_live_agents": 1}, true)
	var settings map[string]any
	if err := json.NewDecoder(settingsResp.Body).Decode(&settings); err != nil {
		t.Fatal(err)
	}
	settingsResp.Body.Close()
	if settingsResp.StatusCode != 200 || settings["max_live_agents"] != float64(1) {
		t.Fatalf("project limit %d %+v", settingsResp.StatusCode, settings)
	}
	invalidLimit := f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/settings", map[string]any{"max_live_agents": 0}, true)
	invalidLimit.Body.Close()
	if invalidLimit.StatusCode != 400 {
		t.Fatalf("invalid project limit status=%d", invalidLimit.StatusCode)
	}
}
