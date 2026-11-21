package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
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
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho codex-cli 0.162.1\n"), 0755); err != nil {
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
	if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\necho fixture-version\n"), 0755); err != nil {
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
	resp = spawn("launcher-id-wins", "leader", "docker")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spawn by launcher ID: %d", resp.StatusCode)
	}
	if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
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

// Mistral Vibe is a harness an owner can add: it is validated by the machine's supervisor like the others, gets its own default label (with
// "(Docker)" for a containerized one), and is listed natively and in Docker.
func TestOwnerAddsAMistralVibeHarness(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "vibe")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho 'vibe 2.25.0'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"tool": "vibe", "command": path, "enabled": true, "args": []string{}, "env_refs": map[string]string{}, "path_maps": []any{}, "visible_roots": []string{}, "supports_toolbox": true}
	resp := f.request(http.MethodPost, base, body, true)
	var created struct {
		Config store.Launcher `json:"config"`
	}
	if resp.StatusCode != 200 {
		t.Fatalf("add a vibe harness: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.Config.Tool != "vibe" || created.Config.Label != "Mistral Vibe" || created.Config.Name == "" {
		t.Fatalf("created: %+v", created.Config)
	}
	body["containerized"] = true
	resp = f.request(http.MethodPost, base, body, true)
	var docker struct {
		Config store.Launcher `json:"config"`
	}
	if resp.StatusCode != 200 {
		t.Fatalf("a containerized vibe harness: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&docker); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if docker.Config.Label != "Mistral Vibe (Docker)" || !docker.Config.Containerized {
		t.Fatalf("created: %+v", docker.Config)
	}
	body["tool"], body["containerized"] = "cursor", false
	if resp = f.request(http.MethodPost, base, body, true); resp.StatusCode != 400 {
		t.Fatalf("an unknown tool: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !listedHarness("vibe", "native") || !listedHarness("vibe", "docker") || listedHarness("vibe", "podman") {
		t.Fatal("vibe is listed natively and in Docker")
	}
	resp = f.request(http.MethodGet, "/api/spawn-catalog", nil, false)
	var catalog struct {
		Tools []spawnCatalogTool `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, tool := range catalog.Tools {
		if tool.Tool == "vibe" && tool.Variant == "native" {
			found = tool.Label == "Mistral Vibe"
		}
	}
	if !found {
		t.Fatalf("the spawn catalog lists Mistral Vibe: %+v", catalog.Tools)
	}
}

// OpenCode can run in a container: the owner can add a containerized OpenCode harness (it was refused before), it gets the Docker label,
// and the spawn catalog lists the Docker adapter next to the native one.
func TestOwnerAddsADockerOpenCodeHarness(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "opencode-docker")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho 1.19.4\n"), 0755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"tool": "opencode", "command": path, "enabled": true, "containerized": true, "args": []string{}, "env_refs": map[string]string{},
		"path_maps": []any{}, "visible_roots": []string{}, "supports_toolbox": true}
	resp := f.request(http.MethodPost, base, body, true)
	var created struct {
		Config store.Launcher `json:"config"`
	}
	if resp.StatusCode != 200 {
		t.Fatalf("add a Docker OpenCode harness: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.Config.Tool != "opencode" || !created.Config.Containerized || created.Config.Label != "OpenCode (Docker)" {
		t.Fatalf("created: %+v", created.Config)
	}
	if !listedHarness("opencode", "docker") || !listedHarness("opencode", "native") {
		t.Fatal("OpenCode is listed natively and in Docker")
	}
	resp = f.request(http.MethodGet, "/api/spawn-catalog", nil, false)
	var catalog struct {
		Tools []spawnCatalogTool `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	docker, native := false, false
	for _, tool := range catalog.Tools {
		if tool.Tool == "opencode" && tool.Variant == "docker" && tool.Label == "OpenCode (Docker)" {
			docker = true
		}
		if tool.Tool == "opencode" && tool.Variant == "native" {
			native = true
		}
	}
	if !docker || !native {
		t.Fatalf("the spawn catalog lists OpenCode and OpenCode (Docker): %+v", catalog.Tools)
	}
	if !dockerNeedsReachableBind("opencode", "docker") || dockerNeedsReachableBind("opencode", "native") {
		t.Fatal("a Docker OpenCode reaches the server from its container, so it needs a reachable bind")
	}
}

// A container launcher whose version cannot be asked for says why in the launcher list, instead of a bare "unknown".
func TestLauncherListSaysWhyAContainerVersionIsUnknown(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "claude-docker")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho 2.1.310 (Claude Code)\n"), 0755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"tool": "claude", "command": path, "enabled": true, "containerized": true, "args": []string{}, "env_refs": map[string]string{},
		"path_maps": []any{}, "visible_roots": []string{}, "supports_toolbox": true}
	if resp := f.request(http.MethodPost, base, body, true); resp.StatusCode != 200 {
		t.Fatalf("add a Docker harness: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp := f.request(http.MethodGet, base, nil, false)
	defer resp.Body.Close()
	var listed struct {
		Launchers []map[string]any `json:"launchers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil || len(listed.Launchers) == 0 {
		t.Fatalf("launcher list: %+v %v", listed, err)
	}
	var row map[string]any
	for _, candidate := range listed.Launchers {
		if config, _ := candidate["config"].(map[string]any); config["command"] == path {
			row = candidate
		}
	}
	if row == nil {
		t.Fatalf("the added harness is not listed: %+v", listed.Launchers)
	}
	if row["version"] != "" && row["version"] != nil {
		t.Fatalf("no folder to ask from, so no version: %+v", row)
	}
	if problem, _ := row["version_problem"].(string); !strings.Contains(problem, "no project folder") {
		t.Fatalf("the list does not say why the version is unknown: %+v", row)
	}
}

// A harness's custom OpenCode providers are read and replaced on the same terms as its models: the listing carries providers and
// providers_rev, a stale edit is refused with 409 stale_providers, dropping a provider whose models are listed is refused with
// 409 provider_in_use unless remove_models is sent, and a bad list is a 400 that changes nothing.
func TestHarnessProvidersEditAndRemoval(t *testing.T) {
	f := newFixture(t)
	harness := f.harness(t, "opencode", "native")
	url := "/api/machines/" + f.machineID + "/launchers/" + harness.ID + "/providers"
	type answer struct {
		Code       string `json:"error"`
		Message    string `json:"message"`
		ProviderID string `json:"provider_id"`
		ModelCount int    `json:"model_count"`
		InUse      []struct {
			ProviderID string `json:"provider_id"`
			ModelCount int    `json:"model_count"`
		} `json:"in_use"`
		Providers    []store.HarnessProvider `json:"providers"`
		ProvidersRev string                  `json:"providers_rev"`
		Models       []store.HarnessModel    `json:"models"`
	}
	call := func(method string, body any) (int, answer) {
		t.Helper()
		resp := f.request(method, url, body, method != http.MethodGet)
		defer resp.Body.Close()
		var out answer
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	listed := func() (providers []store.HarnessProvider, rev string) {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/machines/"+f.machineID+"/launchers", nil, false)
		defer resp.Body.Close()
		var body struct {
			Launchers []struct {
				Config       store.Launcher          `json:"config"`
				Providers    []store.HarnessProvider `json:"providers"`
				ProvidersRev string                  `json:"providers_rev"`
			} `json:"launchers"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&body) != nil {
			t.Fatalf("listing: %d", resp.StatusCode)
		}
		for _, row := range body.Launchers {
			if row.Config.ID == harness.ID {
				if row.Providers == nil {
					t.Fatal("a harness without providers lists an empty array, not null")
				}
				return row.Providers, row.ProvidersRev
			}
		}
		t.Fatal("harness missing from the listing")
		return nil, ""
	}
	corp := store.HarnessProvider{ID: "corp-llm", Name: "Corp LLM", NPM: store.HarnessProviderNPM, BaseURL: "https://llm.example.com/v1", KeyEnv: "CORP_LLM_KEY", ContextTokens: 128000, OutputTokens: 8192}
	lab := store.HarnessProvider{ID: "lab-llm", Name: "Lab", NPM: store.HarnessProviderNPM, BaseURL: "http://192.168.1.4:8080/v1"}
	if status, out := call(http.MethodGet, nil); status != 200 || len(out.Providers) != 0 || out.ProvidersRev != store.HarnessProvidersRev(nil) {
		t.Fatalf("initial read: %d %+v", status, out)
	}
	if providers, rev := listed(); len(providers) != 0 || rev != store.HarnessProvidersRev(nil) {
		t.Fatalf("initial listing: %+v %q", providers, rev)
	}
	// Two editors read the empty list; the first saves, and the answer's revision is the listing's.
	_, oldRev := listed()
	status, first := call(http.MethodPut, map[string]any{"providers": []store.HarnessProvider{corp, lab}, "expected_rev": oldRev})
	if status != 200 || len(first.Providers) != 2 || first.ProvidersRev != store.HarnessProvidersRev([]store.HarnessProvider{corp, lab}) {
		t.Fatalf("first edit: %d %+v", status, first)
	}
	if got, rev := listed(); rev != first.ProvidersRev || len(got) != 2 || got[0] != corp {
		t.Fatalf("listing after the edit: %+v %q", got, rev)
	}
	if status, out := call(http.MethodPut, map[string]any{"providers": []store.HarnessProvider{corp}, "expected_rev": oldRev}); status != 409 || out.Code != "stale_providers" {
		t.Fatalf("stale edit: %d %+v", status, out)
	}
	// A bad list is a 400 and changes nothing.
	bad := corp
	bad.NPM = "left-pad"
	if status, out := call(http.MethodPut, map[string]any{"providers": []store.HarnessProvider{bad}}); status != 400 || out.Code != "invalid_providers" || !strings.Contains(out.Message, store.HarnessProviderNPM) {
		t.Fatalf("bad provider: %d %+v", status, out)
	}
	for name, edit := range map[string]func(*store.HarnessProvider){
		"protected variable": func(p *store.HarnessProvider) { p.KeyEnv = "OPENCODE_CONFIG_CONTENT" },
		"plain http, public": func(p *store.HarnessProvider) { p.BaseURL = "http://llm.example.com/v1" },
	} {
		changed := corp
		edit(&changed)
		if status, out := call(http.MethodPut, map[string]any{"providers": []store.HarnessProvider{changed}}); status != 400 || out.Code != "invalid_providers" {
			t.Errorf("%s: %d %+v", name, status, out)
		}
	}
	if got, _ := listed(); len(got) != 2 {
		t.Fatalf("a refused edit must change nothing: %+v", got)
	}
	// Models of a provider keep it from being dropped; the 409 names it and counts them.
	modelsURL := "/api/machines/" + f.machineID + "/launchers/" + harness.ID + "/models"
	resp := f.request(http.MethodPut, modelsURL, map[string]any{"models": []store.HarnessModel{
		{Slug: "corp-llm/fast", Label: "Fast", Default: true}, {Slug: "corp-llm/big", Label: "Big"}, {Slug: "lab-llm/x", Label: "X"}}}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("models: %d", resp.StatusCode)
	}
	status, out := call(http.MethodPut, map[string]any{"providers": []store.HarnessProvider{lab}})
	if status != 409 || out.Code != "provider_in_use" || out.ProviderID != "corp-llm" || out.ModelCount != 2 {
		t.Fatalf("a provider in use: %d %+v", status, out)
	}
	if got, _ := listed(); len(got) != 2 {
		t.Fatalf("a refused removal must change nothing: %+v", got)
	}
	// Dropping both names both: remove_models would delete the models of every dropped provider, so the offer must list them all.
	status, out = call(http.MethodPut, map[string]any{"providers": []store.HarnessProvider{}})
	if status != 409 || out.Code != "provider_in_use" || out.ProviderID != "corp-llm" || out.ModelCount != 2 || len(out.InUse) != 2 ||
		out.InUse[0].ProviderID != "corp-llm" || out.InUse[0].ModelCount != 2 || out.InUse[1].ProviderID != "lab-llm" || out.InUse[1].ModelCount != 1 {
		t.Fatalf("two providers in use: %d %+v", status, out)
	}
	// With remove_models the provider goes together with its models, and the answer carries the models that are left.
	status, out = call(http.MethodPut, map[string]any{"providers": []store.HarnessProvider{lab}, "remove_models": true})
	if status != 200 || len(out.Providers) != 1 || len(out.Models) != 1 || out.Models[0].Slug != "lab-llm/x" || !out.Models[0].Default {
		t.Fatalf("remove with models: %d %+v", status, out)
	}
}

// A turn on a custom OpenCode provider is never sent to a supervisor that does not know providers (it would start OpenCode without the
// endpoint): the dispatcher holds it and says so once in the agent's room, a direct dispatch is refused, and once the supervisor knows
// providers the turn goes out carrying the provider document: names, URL and the model ids, no key.
func TestCustomProviderTurnWaitsForASupervisorThatKnowsProviders(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	harness := f.harness(t, "opencode", "native")
	provider := store.HarnessProvider{ID: "corp-llm", Name: "Corp LLM", NPM: store.HarnessProviderNPM, BaseURL: "https://llm.example.com/v1", KeyEnv: "CORP_LLM_KEY"}
	if err := f.db.ReplaceHarnessProvidersIfRev(ctx, "owner", f.machineID, harness.ID, []store.HarnessProvider{provider}, "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceHarnessModels(ctx, "owner", f.machineID, harness.ID, []store.HarnessModel{{Slug: "corp-llm/fast", Label: "Fast", Default: true}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_prov','worker','Worker','worker','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_prov',?,?,?)`, f.project.ID, f.machineID, t.TempDir())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: "provided", ProjectID: f.project.ID, RoleID: "role_prov", Tool: "opencode", Variant: "native", LauncherID: harness.ID,
		Model: "corp-llm/fast", Provider: "corp-llm", MachineID: f.machineID, CheckoutID: "chk_prov", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_prov',?,0,'fresh_with_history','2026-10-13T00:00:00.000000000Z')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := f.db.MachineByID(ctx, f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: f.machineID, keyHash: stored.KeyHash, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{agent.ID: 0},
		supportsLaunchPath: true, supportsSessionFork: true, supportsLaunchers: true}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	if err := f.srv.Dispatch(ctx, protocol.Delivery{AgentID: agent.ID, Generation: 0, TurnID: "turn_never", Mode: adapter.ModeNormal, Providers: []adapter.CustomProvider{{ID: "corp-llm"}}}); !errors.Is(err, ErrSupervisorUpdateNeeded) {
		t.Fatalf("a delivery with providers to a supervisor that does not know them: %v", err)
	}
	response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "@provided please work"}, true)
	if response.StatusCode != 201 {
		t.Fatalf("post status=%d", response.StatusCode)
	}
	response.Body.Close()
	dispatcher := NewDispatcher(f.srv)
	for i := 0; i < 3; i++ {
		if count, err := dispatcher.Pass(ctx); err != nil || count != 0 {
			t.Fatalf("pass %d dispatched %d: %v", i, count, err)
		}
	}
	if len(machine.sendQueue) != 0 {
		t.Fatalf("a supervisor that does not know providers was sent %d frame(s)", len(machine.sendQueue))
	}
	messages, err := f.db.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, message := range messages {
		if message.AuthorKind == "system" && strings.Contains(message.Body, "provided is held") && strings.Contains(message.Body, "custom OpenCode provider") {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("the hold was said %d times, want once", held)
	}
	machine.supportsProviders = true
	if count, err := dispatcher.Pass(ctx); err != nil || count != 1 {
		t.Fatalf("dispatch after the supervisor knows providers: %d %v", count, err)
	}
	select {
	case frame := <-machine.sendQueue:
		delivery := frame.Delivery()
		if len(delivery.Providers) != 1 || delivery.Providers[0].ID != "corp-llm" || delivery.Providers[0].KeyEnv != "CORP_LLM_KEY" ||
			len(delivery.Providers[0].Models) != 1 || delivery.Providers[0].Models[0].ID != "fast" || delivery.Agent.Model != "corp-llm/fast" {
			t.Fatalf("the turn's provider document: %+v", delivery)
		}
	default:
		t.Fatal("no frame was sent")
	}
}

// The supervisor says in its hello that it knows custom providers, which is what lets the dispatcher send it their turns.
func TestSupervisorHelloSaysItKnowsProviders(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = client.RunOnce(runCtx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not connect")
	}
	f.srv.machineMu.Lock()
	defer f.srv.machineMu.Unlock()
	if conn := f.srv.machines[f.machineID]; conn == nil || !conn.supportsProviders {
		t.Fatalf("the connected supervisor does not say it knows providers: %+v", conn)
	}
}
