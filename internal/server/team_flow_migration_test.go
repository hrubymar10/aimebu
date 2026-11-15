package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestTeamFlowMigrationHTTPPreviewApplyCancel(t *testing.T) {
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	target := map[string]any{"machine_id": f.machineID, "tool": "fake", "variant": "test", "model": "test"}
	setup := f.request(http.MethodPost, "/api/projects/"+f.project.ID+"/team-setup/preview", map[string]any{"flow_key": "leader-workers", "flow_rev": 0, "leader": target}, true)
	setup.Body.Close()
	if setup.StatusCode != 200 {
		t.Fatalf("setup preview: %d", setup.StatusCode)
	}
	createBody, _ := json.Marshal(map[string]any{"flow_key": "leader-workers", "flow_rev": 0, "leader": target})
	createReq, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/projects/"+f.project.ID+"/team-setup", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-CSRF-Token", f.csrf)
	createReq.Header.Set("Idempotency-Key", "setup-migration")
	createReq.AddCookie(f.cookie)
	created, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if created.StatusCode != 201 {
		t.Fatalf("setup: %d", created.StatusCode)
	}
	recommendations := f.request(http.MethodGet, "/api/team-flows/recommended-models", nil, false)
	var models struct {
		Rows []any `json:"rows"`
	}
	if err := json.NewDecoder(recommendations.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	recommendations.Body.Close()
	if recommendations.StatusCode != 200 || len(models.Rows) != 22 {
		t.Fatalf("recommendations: %d %+v", recommendations.StatusCode, models)
	}
	claude := f.harness(t, "claude", "native")
	f.setHarnessModels(t, claude,
		store.HarnessModel{Slug: "claude-opus-5-5", Label: "Opus 5.5", Default: true},
		store.HarnessModel{Slug: "claude-sonnet-5-5", Label: "Sonnet 5.5"},
		store.HarnessModel{Slug: "claude-fable-5-2", Label: "Fable 5.2"})
	filtered := f.request(http.MethodGet, "/api/team-flows/recommended-models?machine_id="+f.machineID+"&launcher_id="+claude.ID, nil, false)
	var available struct {
		Rows []struct {
			RoleKey string `json:"role_key"`
			FlowKey string `json:"flow_key"`
			Tool    string `json:"tool"`
			Models  []struct {
				Model string `json:"model"`
			} `json:"models"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(filtered.Body).Decode(&available); err != nil {
		t.Fatal(err)
	}
	filtered.Body.Close()
	if filtered.StatusCode != 200 || len(available.Rows) != 11 {
		t.Fatalf("filtered recommendations: %d %+v", filtered.StatusCode, available)
	}
	for _, row := range available.Rows {
		if row.Tool != "claude" {
			t.Fatalf("wrong harness row: %+v", row)
		}
		if row.FlowKey == "leader-advisor" && row.RoleKey == "advisor" && len(row.Models) != 2 {
			t.Fatalf("advisor catalog recommendations: %+v", row)
		}
	}
	path := "/api/projects/" + f.project.ID + "/team-flow"
	plan := f.request(http.MethodPost, path+"/migration/preview", map[string]any{"flow_key": "leader-dispatcher", "flow_rev": 1}, true)
	var planned struct {
		CanApply bool `json:"can_apply"`
		Roles    []struct {
			RoleKey string `json:"role_key"`
			Status  string `json:"status"`
		} `json:"roles"`
	}
	if err := json.NewDecoder(plan.Body).Decode(&planned); err != nil {
		t.Fatal(err)
	}
	plan.Body.Close()
	if plan.StatusCode != 200 || planned.CanApply || len(planned.Roles) != 4 || planned.Roles[1].Status != "new" {
		t.Fatalf("plan: %d %+v", plan.StatusCode, planned)
	}
	migrate := map[string]any{"flow_key": "leader-dispatcher", "flow_rev": 1, "dispatcher": target}
	preview := f.request(http.MethodPost, path+"/migration/preview", migrate, true)
	var details struct {
		FromKey  string `json:"from_key"`
		ToKey    string `json:"to_key"`
		CanApply bool   `json:"can_apply"`
		Roles    []struct {
			RoleKey string `json:"role_key"`
			Status  string `json:"status"`
		} `json:"roles"`
	}
	if err := json.NewDecoder(preview.Body).Decode(&details); err != nil {
		t.Fatal(err)
	}
	preview.Body.Close()
	if preview.StatusCode != 200 || !details.CanApply || len(details.Roles) != 4 || details.Roles[1].Status != "new" {
		t.Fatalf("preview %d %+v", preview.StatusCode, details)
	}
	data, _ := json.Marshal(migrate)
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+path+"/migrate", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.Header.Set("Idempotency-Key", "migration-1")
	req.AddCookie(f.cookie)
	started, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.NewDecoder(started.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	started.Body.Close()
	if started.StatusCode != 202 || response["migration"] == nil {
		t.Fatalf("start %d %+v", started.StatusCode, response)
	}
	progress := f.request(http.MethodGet, path+"/migration", nil, false)
	progress.Body.Close()
	if progress.StatusCode != 200 {
		t.Fatalf("progress: %d", progress.StatusCode)
	}
	cancel := f.request(http.MethodPost, path+"/migration/cancel", map[string]any{}, true)
	var cancelled struct{ Migration struct{ State string } }
	if err := json.NewDecoder(cancel.Body).Decode(&cancelled); err != nil {
		t.Fatal(err)
	}
	cancel.Body.Close()
	if cancel.StatusCode != 200 || cancelled.Migration.State != "cancelled" {
		t.Fatalf("cancel %d %+v", cancel.StatusCode, cancelled)
	}
}
