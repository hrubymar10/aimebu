package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
)

func TestOwnerEnablesMachineLauncherBeforeCatalogSpawn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := "/api/machines/" + f.machineID + "/launchers"
	journal, err := supervisor.OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go client.RunOnce(ctx)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not connect")
	}
	resp := f.request(http.MethodGet, base, nil, false)
	var listed struct {
		Launchers []struct {
			Config store.Launcher `json:"config"`
		} `json:"launchers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(listed.Launchers) != 7 {
		t.Fatalf("launchers=%+v", listed.Launchers)
	}
	var codex store.Launcher
	for _, row := range listed.Launchers {
		if row.Config.Tool == "codex" && row.Config.Name == "native" {
			codex = row.Config
		}
		if row.Config.Enabled {
			t.Fatalf("new preset enabled: %+v", row.Config)
		}
	}
	if codex.ID == "" {
		t.Fatal("codex native preset missing")
	}
	if _, _, err := f.srv.selectedMachine(t.Context(), f.machineID, "codex", "native", codex.ID); !errors.Is(err, errLauncherUnavailable) {
		t.Fatalf("disabled launcher selected: %v", err)
	}
	path := filepath.Join(t.TempDir(), "codex-wrapper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho codex-cli 0.162.1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	codex.Command, codex.Enabled = path, true
	resp = f.request(http.MethodPut, base+"/"+codex.ID, codex, true)
	if resp.StatusCode != 200 {
		t.Fatalf("launcher PUT=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/spawn-catalog", nil, false)
	var catalog struct {
		Tools []spawnCatalogTool `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, item := range catalog.Tools {
		if item.LauncherID == codex.ID {
			found = true
			if !item.Available || item.CLIVersion != "codex-cli 0.162.1" || strings.Contains(item.Label, "Docker") {
				t.Fatalf("catalog row=%+v", item)
			}
		}
	}
	if !found {
		t.Fatal("enabled launcher missing from catalog")
	}
	if _, _, err := f.srv.selectedMachine(t.Context(), f.machineID, "codex", "native", codex.ID); err != nil {
		t.Fatalf("select launcher by ID: %v", err)
	}
	if _, _, err := f.srv.selectedMachine(t.Context(), f.machineID, "codex", "docker", codex.ID); err != nil {
		t.Fatalf("launcher ID should win over alias: %v", err)
	}
	codex.Containerized = true
	codex.AgentServerURL = "http://host.docker.internal:9897"
	resp = f.request(http.MethodPut, base+"/"+codex.ID, codex, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("container launcher PUT=%d", resp.StatusCode)
	}
	if _, _, err := f.srv.selectedMachine(t.Context(), f.machineID, "codex", "native", codex.ID); err != nil {
		t.Fatalf("agent-reachable URL should admit containerized launcher: %v", err)
	}
	resp = f.request(http.MethodPost, base, store.Launcher{Tool: "codex", Name: "alternate", Command: path}, true)
	var created struct {
		Config store.Launcher `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || created.Config.ID == "" {
		t.Fatalf("custom launcher POST=%d %+v", resp.StatusCode, created)
	}
	created.Config.Name = "native"
	resp = f.request(http.MethodPut, base+"/"+created.Config.ID, created.Config, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate launcher name status=%d", resp.StatusCode)
	}
	resp = f.request(http.MethodGet, base, nil, false)
	var after struct {
		Launchers []struct {
			Config    store.Launcher `json:"config"`
			Available bool           `json:"available"`
		} `json:"launchers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	foundCustom := false
	for _, row := range after.Launchers {
		if row.Config.ID == created.Config.ID {
			foundCustom = true
			if row.Config.Name != "alternate" {
				t.Fatalf("rejected edit applied to supervisor: %+v", row.Config)
			}
		}
	}
	if !foundCustom {
		t.Fatal("custom launcher disappeared after rejected edit")
	}
}

func TestLauncherPreflightRejectsSpawnBeforeAgentCreation(t *testing.T) {
	f := newFixture(t)
	f.seed()
	journal, err := supervisor.OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go client.RunOnce(ctx)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not connect")
	}
	command := filepath.Join(t.TempDir(), "launcher")
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho fixture-version\n"), 0755); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPost, "/api/machines/"+f.machineID+"/launchers", store.Launcher{
		Tool: "claude", Name: "scratch", Command: command, Enabled: true,
		VisibleRoots: []string{f.project.Folder},
	}, true)
	var created struct {
		Config store.Launcher `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || created.Config.ID == "" {
		t.Fatalf("launcher creation: %d %+v", resp.StatusCode, created)
	}
	f.setHarnessModels(t, created.Config, store.HarnessModel{Slug: "haiku", Label: "Haiku", Default: true})
	spawn := func(key, role, variant string) *http.Response {
		body, _ := json.Marshal(map[string]string{
			"role_key": role, "tool": "claude", "variant": variant,
			"launcher_id": created.Config.ID, "model": "haiku",
		})
		req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(f.cookie)
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	resp = spawn("launcher-id-wins", "lead", "docker")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spawn by launcher ID: %d", resp.StatusCode)
	}
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	resp = spawn("preflight-fails", "reviewer", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("failed preflight status=%d", resp.StatusCode)
	}
	resp = f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/agents", nil, false)
	var agents []store.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(agents) != 1 {
		t.Fatalf("preflight created an agent: %+v", agents)
	}
}

func TestPausedAgentCanReactivateWithOfflineEnabledLauncher(t *testing.T) {
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, body := f.spawnRequest(t, "offline-reactivate", "worker")
	if status != http.StatusCreated {
		t.Fatalf("spawn=%d body=%v", status, body)
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()
	launcher, err := f.db.SaveLauncher(t.Context(), "owner", store.Launcher{
		MachineID: f.machineID, Tool: "codex", Name: "offline", Command: "codex", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET tool='codex',variant='offline',launcher_id=?,desired='not_accepting' WHERE id=?`, launcher.ID, card.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response := f.request(http.MethodPost, "/api/agents/"+card.ID+"/control", map[string]any{"desired": "active", "rev": card.DesiredRev}, true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("offline reactivate=%d", response.StatusCode)
	}
	var active store.AgentCard
	if err := json.NewDecoder(response.Body).Decode(&active); err != nil || active.Desired != "active" {
		t.Fatalf("reactivated=%+v err=%v", active, err)
	}
	response = f.request(http.MethodPost, "/api/agents/"+card.ID+"/control", map[string]any{"desired": "not_accepting", "rev": active.DesiredRev}, true)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pause=%d", response.StatusCode)
	}
	var paused store.AgentCard
	if err := json.NewDecoder(response.Body).Decode(&paused); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE machine_launcher SET enabled=0 WHERE id=?`, launcher.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response = f.request(http.MethodPost, "/api/agents/"+card.ID+"/control", map[string]any{"desired": "active", "rev": paused.DesiredRev}, true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("disabled launcher reactivate=%d", response.StatusCode)
	}
}

// The harness listing carries models_rev for each harness, and a models edit that names the revision it was based on is refused with
// 409 stale_models when the list has changed since (two tabs, two people), leaving the list as the other editor left it. Without a
// revision the old behaviour stays: the list is replaced.
func TestHarnessModelsEditOverAStaleRevisionIsRefused(t *testing.T) {
	f := newFixture(t)
	codex := f.harness(t, "codex", "native")
	url := "/api/machines/" + f.machineID + "/launchers/" + codex.ID + "/models"
	listed := func() (models []store.HarnessModel, rev string) {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/machines/"+f.machineID+"/launchers", nil, false)
		defer resp.Body.Close()
		var body struct {
			Launchers []struct {
				Config    store.Launcher       `json:"config"`
				Models    []store.HarnessModel `json:"models"`
				ModelsRev string               `json:"models_rev"`
			} `json:"launchers"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&body) != nil {
			t.Fatalf("listing: %d", resp.StatusCode)
		}
		for _, row := range body.Launchers {
			if row.Config.ID == codex.ID {
				return row.Models, row.ModelsRev
			}
		}
		t.Fatal("harness missing from the listing")
		return nil, ""
	}
	put := func(list []store.HarnessModel, rev string) (int, string, string) {
		t.Helper()
		body := map[string]any{"models": list}
		if rev != "" {
			body["expected_rev"] = rev
		}
		resp := f.request(http.MethodPut, url, body, true)
		defer resp.Body.Close()
		var answer struct {
			Code      string `json:"error"`
			ModelsRev string `json:"models_rev"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&answer)
		return resp.StatusCode, answer.Code, answer.ModelsRev
	}
	models, rev := listed()
	if rev == "" || rev != store.HarnessModelsRev(models) {
		t.Fatalf("listing revision %q for %+v", rev, models)
	}
	// Two editors read the same list. The first saves; its answer carries the new revision, which the listing agrees with.
	mine := []store.HarnessModel{{Slug: "gpt-host", Label: "Host", Default: true}, {Slug: "gpt-mine", Label: "Mine"}}
	status, _, newRev := put(mine, rev)
	if status != 200 || newRev != store.HarnessModelsRev(mine) {
		t.Fatalf("first edit: %d %q", status, newRev)
	}
	if _, now := listed(); now != newRev {
		t.Fatalf("the listing says %q, the edit answered %q", now, newRev)
	}
	// The second still holds the old revision: refused, and the list is the first editor's.
	status, code, _ := put([]store.HarnessModel{{Slug: "gpt-theirs", Label: "Theirs", Default: true}}, rev)
	if status != 409 || code != "stale_models" {
		t.Fatalf("stale edit: %d %q", status, code)
	}
	if kept, _ := listed(); len(kept) != 2 || kept[1].Slug != "gpt-mine" {
		t.Fatalf("a refused edit must change nothing: %+v", kept)
	}
	// After reloading (the new revision) the same edit goes through; one with no revision replaces as before.
	if status, _, _ := put([]store.HarnessModel{{Slug: "gpt-theirs", Label: "Theirs", Default: true}}, newRev); status != 200 {
		t.Fatalf("edit on the current revision: %d", status)
	}
	if status, _, _ := put([]store.HarnessModel{{Slug: "gpt-any", Label: "Any", Default: true}}, ""); status != 200 {
		t.Fatalf("edit without a revision: %d", status)
	}
}
