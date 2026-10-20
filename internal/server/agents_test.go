package server

import (
	"bytes"
	"context"
	"fmt"
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

func (f *fixture) spawnRequest(t *testing.T, key, role string) (int, store.AgentCard, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"role_key": role, "tool": "fake", "variant": "test", "model": "test"})
	req, _ := http.NewRequest("POST", f.http.URL+"/api/rooms/"+f.room.ID+"/agents", bytes.NewReader(body))
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
	var card store.AgentCard
	_ = json.Unmarshal(data, &card)
	return resp.StatusCode, card, raw
}

func TestListedAdapter(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	f := newFixture(t)
	listedAdapter := func(req spawnRequest) bool {
		ok, err := f.srv.listedAdapter(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !listedAdapter(spawnRequest{Tool: "claude", Variant: "native", Model: "haiku", Provider: "anthropic"}) {
		t.Fatal("Claude adapter was rejected")
	}
	if !listedAdapter(spawnRequest{Tool: "codex", Variant: "native", Model: "gpt-6-luna", Provider: "openai"}) {
		t.Fatal("Codex adapter was rejected")
	}
	if listedAdapter(spawnRequest{Tool: "claude", Variant: "native", Provider: "openai"}) || listedAdapter(spawnRequest{Tool: "nonesuch", Variant: "native"}) {
		t.Fatal("an unsupported provider or tool was accepted")
	}
	if !listedAdapter(spawnRequest{Tool: "claude", Variant: "docker", Model: "sonnet"}) || !listedAdapter(spawnRequest{Tool: "codex", Variant: "docker", Model: "gpt-6-sol"}) {
		t.Fatal("Docker adapters were rejected")
	}
}

func TestNativeCatalogDoesNotUseLegacyEnvGate(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "")
	f := newFixture(t)
	if ok, err := f.srv.listedAdapter(context.Background(), spawnRequest{Tool: "claude", Variant: "native", Model: "haiku"}); err != nil || !ok {
		t.Fatal("native Claude disappeared from the server-approved model list")
	}
	resp := f.request(http.MethodGet, "/api/spawn-catalog", nil, false)
	defer resp.Body.Close()
	var catalog struct {
		Tools []spawnCatalogTool `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	for _, item := range catalog.Tools {
		if item.Variant == "native" && item.Available {
			t.Fatalf("offline machine advertised native launcher: %+v", item)
		}
	}
}

func TestAgentSwitchEndpointIdempotencyAndCatalog(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}, {Tool: "claude", Environment: adapter.Environment{Variant: "native"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "switch-spawn", "worker")
	if status != 201 || card.ID == "" {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	originalCheckout, err := f.db.AgentCheckoutPath(t.Context(), card.ID)
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.project.Folder, "init", "-q")
	gitTest(t, f.project.Folder, "config", "user.name", "Test")
	gitTest(t, f.project.Folder, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(f.project.Folder, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.project.Folder, "add", "tracked.txt")
	gitTest(t, f.project.Folder, "commit", "-qm", "base")
	optIn := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/worktree", map[string]any{"enabled": true}, true)
	optIn.Body.Close()
	if optIn.StatusCode != http.StatusOK {
		t.Fatalf("worktree opt-in status=%d", optIn.StatusCode)
	}
	switchPost := func(key, model string, rev int64) (int, store.AgentCard, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"room_id": f.room.ID, "tool": "claude", "variant": "native", "model": model, "rev": rev})
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/agents/"+card.ID+"/switch", bytes.NewReader(body))
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
		var got store.AgentCard
		_ = json.Unmarshal(data, &got)
		return resp.StatusCode, got, raw
	}
	harness := f.harness(t, "claude", "native")
	if status, _, raw := switchPost("bad-model", "unlisted", card.DesiredRev); status != 400 || raw["error"] != "model_unlisted" {
		t.Fatalf("unlisted %d %+v", status, raw)
	}
	machine.capabilities = machine.capabilities[:1]
	if status, _, raw := switchPost("unavailable", "haiku", card.DesiredRev); status != 503 || raw["error"] != "machine_unavailable" {
		t.Fatalf("unavailable %d %+v", status, raw)
	}
	if unchanged, err := f.db.AgentCardByID(context.Background(), card.ID); err != nil || unchanged.Desired != "active" || unchanged.Switch != nil {
		t.Fatalf("unavailable mutated agent %+v %v", unchanged, err)
	}
	machine.capabilities = append(machine.capabilities, protocol.CapabilityReport{Tool: "claude", Environment: adapter.Environment{Variant: "native"}})
	status, pending, raw := switchPost("switch-1", "haiku", card.DesiredRev)
	if status != 202 || pending.ID != card.ID || pending.Switch == nil || pending.Switch.State != "draining" || pending.Desired != "not_accepting" {
		t.Fatalf("request %d %+v %+v", status, pending, raw)
	}
	if checkout, err := f.db.AgentCheckoutPath(t.Context(), card.ID); err != nil || checkout != originalCheckout {
		t.Fatalf("same-machine switch moved checkout to %q from %q: %v", checkout, originalCheckout, err)
	}
	status, replay, _ := switchPost("switch-1", "haiku", card.DesiredRev)
	if status != 200 || replay.DesiredRev != pending.DesiredRev || replay.Switch == nil {
		t.Fatalf("replay %d %+v", status, replay)
	}
	if status, _, raw := switchPost("switch-1", "sonnet", card.DesiredRev); status != 409 || raw["error"] != "idempotency_conflict" {
		t.Fatalf("key conflict %d %+v", status, raw)
	}
	if status, _, raw := switchPost("switch-2", "haiku", card.DesiredRev); status != 409 || raw["error"] != "stale_rev" {
		t.Fatalf("stale rev %d %+v", status, raw)
	}
	controls, err := f.db.AdvanceAgentSwitches(context.Background())
	if err != nil || len(controls) != 1 || controls[0].Generation != 1 {
		t.Fatalf("advance %+v %v", controls, err)
	}
	if err := f.db.FailSpawn(context.Background(), card.ID, 1, "private supervisor path"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AdvanceAgentSwitches(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed, err := f.db.AgentCardByID(context.Background(), card.ID)
	if err != nil || failed.Switch == nil || failed.Switch.State != "failed" || strings.Contains(failed.Switch.Error, "private supervisor path") {
		t.Fatalf("failed card %+v %v", failed, err)
	}
	if status, replay, _ := switchPost("switch-1", "haiku", card.DesiredRev); status != 200 || replay.Switch == nil || replay.Switch.State != "failed" {
		t.Fatalf("late replay %d %+v", status, replay)
	}
	f.setHarnessModels(t, harness, store.HarnessModel{Slug: "sonnet", Label: "Sonnet", Default: true})
	if status, replay, _ := switchPost("switch-1", "haiku", card.DesiredRev); status != 200 || replay.Switch == nil {
		t.Fatalf("a keyed retry must replay the stored switch after the model left the harness's list: %d %+v", status, replay)
	}
	if status, _, raw := switchPost("switch-new", "haiku", card.DesiredRev); status != 400 || raw["error"] != "model_unlisted" {
		t.Fatalf("a new request for a removed model must be rejected: %d %+v", status, raw)
	}
}

func TestReactivateQueuesSpawnWithoutWaitingForMachinePing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	receive := func() protocol.Frame {
		t.Helper()
		select {
		case frame := <-machine.sendQueue:
			return frame
		case <-time.After(time.Second):
			t.Fatal("expected immediate machine frame")
			return protocol.Frame{}
		}
	}
	status, card, raw := f.spawnRequest(t, "reactivate-spawn", "worker")
	if status != 201 || card.ID == "" {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	receive() // Initial spawn.
	stop := f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "stopped", "rev": card.DesiredRev}, true)
	if stop.StatusCode != 200 {
		t.Fatalf("stop %d", stop.StatusCode)
	}
	var stopped store.AgentCard
	if err := json.NewDecoder(stop.Body).Decode(&stopped); err != nil {
		t.Fatal(err)
	}
	stop.Body.Close()
	if stopped.Observed != "stopped" {
		t.Fatalf("stop %+v", stopped)
	}
	receive() // Stop control.
	resume := f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "active", "rev": stopped.DesiredRev}, true)
	if resume.StatusCode != 200 {
		t.Fatalf("reactivate %d", resume.StatusCode)
	}
	var active store.AgentCard
	if err := json.NewDecoder(resume.Body).Decode(&active); err != nil {
		t.Fatal(err)
	}
	resume.Body.Close()
	if active.Generation != 1 || active.Desired != "active" || active.Observed != "idle" {
		t.Fatalf("reactivate %+v", active)
	}
	control, spawn := receive(), receive()
	if control.Type != protocol.Control || control.Generation != 1 || control.DesiredState != "active" || spawn.Type != protocol.Spawn || spawn.Generation != 1 || spawn.AgentID != card.ID {
		t.Fatalf("immediate frames: control=%+v spawn=%+v", control, spawn)
	}
}

func TestPausedSwitchSendsControlAfterSpawnConfirmation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "paused-switch-spawn", "worker")
	if status != 201 || card.ID == "" {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	<-machine.sendQueue // Initial spawn.
	owner, err := f.db.UserByLogin(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.SetAgentDesired(card.ID, owner.ID, "not_accepting", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.BeginAgentSwitch(card.ID, owner.ID, f.room.ID, "fake", "test", "other", "fake", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(f.srv)
	if _, err := dispatcher.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-machine.sendQueue:
		if frame.Type != protocol.Spawn || frame.Generation != 1 {
			t.Fatalf("switch spawn %+v", frame)
		}
	default:
		t.Fatal("switch spawn was not queued")
	}
	if err := f.db.ConfirmSpawn(ctx, card.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-machine.sendQueue:
		if frame.Type != protocol.Control || frame.Generation != 1 || frame.DesiredState != "not_accepting" {
			t.Fatalf("pause control %+v", frame)
		}
	default:
		t.Fatal("pause control was not queued")
	}
}

func TestSpawnCatalogSeparatesCLIVersionFromModels(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// The host's supervisor is connected but predates launchers.
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{}}
	f.srv.machineMu.Unlock()
	resp := f.request(http.MethodGet, "/api/spawn-catalog", nil, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var catalog struct {
		Tools                  []spawnCatalogTool `json:"tools"`
		SupervisorUpdateNeeded bool               `json:"supervisor_update_needed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if !catalog.SupervisorUpdateNeeded {
		t.Fatal("a connected supervisor without launcher support is told to update")
	}
	if len(catalog.Tools) != 7 || catalog.Tools[0].Tool != "claude" || catalog.Tools[0].Variant != "docker" || catalog.Tools[0].Available || catalog.Tools[0].CLIVersion != "" {
		t.Fatalf("catalog %+v", catalog.Tools)
	}
	if got := catalog.Tools[0].Models[0].ID; got != "haiku" {
		t.Fatalf("Claude model %q", got)
	}
	if got := catalog.Tools[1].Models[1].ID; got != "gpt-6-luna" {
		t.Fatalf("Codex model %q", got)
	}
	if got := catalog.Tools[2]; got.Tool != "pi" || got.Variant != "docker" || got.Toolbox || got.ToolboxReason == "" || got.Models[0].ID != "ollama-cloud/gemma4:31b" {
		t.Fatalf("Pi Docker catalog %+v", got)
	}
	if got := catalog.Tools[5]; got.Tool != "pi" || got.Variant != "native" || !got.Toolbox {
		t.Fatalf("Pi native catalog %+v", got)
	}
}

func TestPiDockerRequiresReachableBindForCatalogSpawnSwitchAndReactivate(t *testing.T) {
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}, {Tool: "pi", Environment: adapter.Environment{Variant: "docker"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	post := func(path, key string, body any) (int, map[string]any) {
		t.Helper()
		encoded, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
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
	pi := map[string]any{"role_key": "worker", "tool": "pi", "variant": "docker", "model": "ollama-cloud/gemma4:31b"}
	t.Setenv("AIMEBU2_BIND", "")
	resp := f.request(http.MethodGet, "/api/spawn-catalog", nil, false)
	var catalog struct {
		Tools []spawnCatalogTool `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := catalog.Tools[2]; got.Available || got.UnavailableReason != dockerBindUnavailableMessage {
		t.Fatalf("Pi Docker catalog: %+v", got)
	}
	if status, got := post("/api/rooms/"+f.room.ID+"/agents", "pi-blocked", pi); status != 409 || got["message"] != dockerBindUnavailableMessage {
		t.Fatalf("spawn: %d %+v", status, got)
	}
	status, fake, _ := f.spawnRequest(t, "fake-for-switch", "reviewer")
	if status != 201 {
		t.Fatalf("fake spawn: %d", status)
	}
	if status, got := post("/api/agents/"+fake.ID+"/switch", "pi-switch", map[string]any{"room_id": f.room.ID, "tool": "pi", "variant": "docker", "model": "ollama-cloud/gemma4:31b", "rev": fake.DesiredRev}); status != 409 || got["message"] != dockerBindUnavailableMessage {
		t.Fatalf("switch: %d %+v", status, got)
	}
	// Create a Pi Docker agent while the bind is reachable, then restore the
	// default and ensure a direct reactivation request cannot bypass the gate.
	t.Setenv("AIMEBU2_BIND", "0.0.0.0")
	status, created := post("/api/rooms/"+f.room.ID+"/agents", "pi-reachable", pi)
	if status != 201 {
		t.Fatalf("reachable spawn: %d %+v", status, created)
	}
	piID, _ := created["id"].(string)
	if piID == "" {
		t.Fatalf("missing Pi ID: %+v", created)
	}
	rev := int64(created["desired_rev"].(float64))
	status, stopped := post("/api/agents/"+piID+"/control", "", map[string]any{"desired": "stopped", "rev": rev})
	if status != 200 {
		t.Fatalf("stop: %d %+v", status, stopped)
	}
	t.Setenv("AIMEBU2_BIND", "")
	status, blocked := post("/api/agents/"+piID+"/control", "", map[string]any{"desired": "active", "rev": int64(stopped["desired_rev"].(float64))})
	if status != 409 || blocked["message"] != dockerBindUnavailableMessage {
		t.Fatalf("reactivate: %d %+v", status, blocked)
	}
}

func waitFor(t *testing.T, test func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if test() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for state")
}

func TestSpawnEchoIdempotencyWriterAndControl(t *testing.T) {
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
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not connect")
	}
	status, card, raw := f.spawnRequest(t, "same", "worker")
	if status != 201 || card.ID == "" || card.Observed != "idle" {
		t.Fatalf("spawn %d %+v %+v", status, card, raw)
	}
	status, again, _ := f.spawnRequest(t, "same", "worker")
	if status != 201 || again.ID != card.ID {
		t.Fatalf("duplicate spawn %d %+v", status, again)
	}
	status, _, raw = f.spawnRequest(t, "second", "worker")
	if status != 409 || raw["error"] != "writer_occupied" {
		t.Fatalf("second writer %d %+v", status, raw)
	}
	response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "Hello demo"}, true)
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("human message %d", response.StatusCode)
	}
	dispatcher := NewDispatcher(f.srv)
	waitFor(t, func() bool {
		_, _ = dispatcher.Pass(context.Background())
		messages, err := f.db.ListMessagesAfter(context.Background(), f.room.ID, 0)
		if err != nil {
			return false
		}
		for _, m := range messages {
			if m.AuthorID == card.ID && strings.Contains(m.Body, "Hello demo") {
				return true
			}
		}
		return false
	})
	waitFor(t, func() bool {
		agent, err := f.db.AgentRuntimeByID(context.Background(), card.ID)
		if err != nil || agent.Observed != "idle" {
			return false
		}
		open, err := f.db.AgentOpenTurnCount(context.Background(), card.ID)
		return err == nil && open == 0
	})
	current, err := f.db.AgentCardByID(context.Background(), card.ID)
	if err != nil {
		t.Fatal(err)
	}
	response = f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "stopped", "rev": current.DesiredRev}, true)
	if response.StatusCode != 200 {
		t.Fatalf("stop %d", response.StatusCode)
	}
	var stopped store.AgentCard
	json.NewDecoder(response.Body).Decode(&stopped)
	response.Body.Close()
	if stopped.Observed != "stopped" {
		t.Fatalf("idle stop %+v", stopped)
	}
	response = f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "active", "rev": stopped.DesiredRev}, true)
	if response.StatusCode != 200 {
		t.Fatalf("reactivate %d", response.StatusCode)
	}
	var active store.AgentCard
	json.NewDecoder(response.Body).Decode(&active)
	response.Body.Close()
	if active.Generation != 1 || active.Observed != "idle" {
		t.Fatalf("reactivation %+v", active)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestStopRunningAgentConfirmsExit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	fake.ScriptDefault(adapter.Step{Kind: adapter.StepHang})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not connect")
	}
	status, card, raw := f.spawnRequest(t, "running", "worker")
	if status != 201 {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "do work"}, true)
	response.Body.Close()
	dispatcher := NewDispatcher(f.srv)
	waitFor(t, func() bool {
		_, _ = dispatcher.Pass(context.Background())
		agent, err := f.db.AgentRuntimeByID(context.Background(), card.ID)
		return err == nil && agent.Observed == "in_turn"
	})
	current, err := f.db.AgentCardByID(context.Background(), card.ID)
	if err != nil {
		t.Fatal(err)
	}
	response = f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "stopped", "rev": current.DesiredRev}, true)
	if response.StatusCode != 200 {
		t.Fatalf("stop %d", response.StatusCode)
	}
	var interrupt store.AgentCard
	json.NewDecoder(response.Body).Decode(&interrupt)
	response.Body.Close()
	if interrupt.Observed != "interrupting" {
		t.Fatalf("interrupt request %+v", interrupt)
	}
	waitFor(t, func() bool {
		agent, err := f.db.AgentRuntimeByID(context.Background(), card.ID)
		return err == nil && agent.Observed == "stopped"
	})
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestStopPendingBeforeDispatchSettlesAndReleasesWriter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE checkout SET writer_agent_id=?,writer_state='reserved' WHERE id=?`, agent.ID, agent.CheckoutID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response := f.request("POST", "/api/agents/"+agent.ID+"/control", map[string]any{"desired": "stopped", "rev": 0}, true)
	if response.StatusCode != 200 {
		t.Fatalf("stop %d", response.StatusCode)
	}
	var card store.AgentCard
	json.NewDecoder(response.Body).Decode(&card)
	response.Body.Close()
	if card.Observed != "stopped" {
		t.Fatalf("stop card=%+v", card)
	}
	state, err := f.db.DeliveryState(context.Background(), turn.ID)
	if err != nil || state.Exec != "killed" || state.Settle != "settled" || state.NoteState != "queued" {
		t.Fatalf("turn=%+v %v", state, err)
	}
	queued, err := f.db.NoteForAgentMessage(context.Background(), agent.ID, note.MessageID)
	if err != nil || queued.State != "queued" {
		t.Fatalf("note=%+v %v", queued, err)
	}
	assertWriterReleased(t, f, agent.CheckoutID)
	response = f.request("POST", "/api/agents/"+agent.ID+"/control", map[string]any{"desired": "active", "rev": 0}, true)
	if response.StatusCode != 409 {
		t.Fatalf("stale control status=%d", response.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	if body["error"] != "stale_rev" {
		t.Fatalf("stale control=%+v", body)
	}
}

func TestStopPendingAfterDispatchWaitsForNeverStartedProof(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE checkout SET writer_agent_id=?,writer_state='reserved' WHERE id=?`, agent.ID, agent.CheckoutID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ReconcileMachineHello(ctx, f.machineID, "stable-journal", []byte("[]"), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "persisted"); err != nil {
		t.Fatal(err)
	}
	response := f.request("POST", "/api/agents/"+agent.ID+"/control", map[string]any{"desired": "stopped", "rev": 0}, true)
	if response.StatusCode != 200 {
		t.Fatalf("stop %d", response.StatusCode)
	}
	var card store.AgentCard
	json.NewDecoder(response.Body).Decode(&card)
	response.Body.Close()
	if card.Observed != "idle" {
		t.Fatalf("pending stop=%+v", card)
	}
	state, err := f.db.DeliveryState(ctx, turn.ID)
	if err != nil || state.Exec != "pending" {
		t.Fatalf("before proof=%+v %v", state, err)
	}
	if _, err := f.db.ReconcileMachineHello(ctx, f.machineID, "stable-journal", []byte("[]"), nil); err != nil {
		t.Fatal(err)
	}
	state, err = f.db.DeliveryState(ctx, turn.ID)
	if err != nil || state.Exec != "killed" || state.Settle != "settled" || state.NoteState != "queued" {
		t.Fatalf("after proof=%+v %v", state, err)
	}
	card, err = f.db.AgentCardByID(ctx, agent.ID)
	if err != nil || card.Observed != "stopped" {
		t.Fatalf("confirmed stop=%+v %v", card, err)
	}
	assertWriterReleased(t, f, agent.CheckoutID)
}

func assertWriterReleased(t *testing.T, f *fixture, checkoutID string) {
	t.Helper()
	state, owner, err := f.db.CheckoutWriterState(context.Background(), checkoutID)
	if err != nil {
		t.Fatal(err)
	}
	if state != "none" || owner != "" {
		t.Fatalf("writer state=%q owner=%q", state, owner)
	}
}

func TestStopLostKeepsWriterAndReactivateRespectsLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE turn SET exec='lost',outcome='unknown' WHERE id=?`, turn.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE agent SET observed='lost',recovery_block=1 WHERE id=?`, agent.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE checkout SET writer_agent_id=?,writer_state='reserved' WHERE id=?`, agent.ID, agent.CheckoutID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response := f.request("POST", "/api/agents/"+agent.ID+"/control", map[string]any{"desired": "stopped", "rev": 0}, true)
	if response.StatusCode != 409 {
		t.Fatalf("lost stop %d", response.StatusCode)
	}
	response.Body.Close()
	writerState, _, err := f.db.CheckoutWriterState(ctx, agent.CheckoutID)
	if err != nil {
		t.Fatal(err)
	}
	if writerState != "reserved" {
		t.Fatalf("lost writer=%s", writerState)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE turn SET exec='killed',settle='settled' WHERE id=?`, turn.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE agent SET observed='idle',recovery_block=0 WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response = f.request("POST", "/api/agents/"+agent.ID+"/control", map[string]any{"desired": "stopped", "rev": 0}, true)
	if response.StatusCode != 200 {
		t.Fatalf("stop %d", response.StatusCode)
	}
	response.Body.Close()
	other, err := f.db.CreateAgent(ctx, store.Agent{Slug: "other", ProjectID: f.project.ID, RoleID: agent.RoleID, Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: agent.CheckoutID, KeyHash: "hash", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	_ = other
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE settings_project SET max_live_agents=1 WHERE project_id=?`, f.project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response = f.request("POST", "/api/agents/"+agent.ID+"/control", map[string]any{"desired": "active", "rev": 1}, true)
	if response.StatusCode != 429 {
		t.Fatalf("reactivate over limit %d", response.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	if body["error"] != "spawn_limit" {
		t.Fatalf("limit error=%+v", body)
	}
}

func TestSpawnRetryAfterDeliveryFailureResendsUntilConfirmed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	closed := make(chan struct{})
	close(closed)
	dummy := &machineConn{id: f.machineID, done: closed, sendQueue: make(chan protocol.Frame, 1), bound: map[string]int{}, capabilities: []protocol.CapabilityReport{{Tool: "fake", Version: "test", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = dummy
	f.srv.machineMu.Unlock()
	status, _, raw := f.spawnRequest(t, "lost-send", "worker")
	if status != 503 || raw["error"] != "machine_unavailable" {
		t.Fatalf("initial send=%d %+v", status, raw)
	}
	status, _, raw = f.spawnRequest(t, "lost-send", "worker")
	if status != 503 || raw["error"] != "machine_unavailable" {
		t.Fatalf("keyed resend before reconnect=%d %+v", status, raw)
	}
	f.srv.spawnMu.Lock()
	var issuedKey string
	for _, key := range f.srv.spawnKeys {
		issuedKey = key
	}
	f.srv.spawnMu.Unlock()
	if !strings.HasPrefix(issuedKey, "aak_") {
		t.Fatalf("expected a cached agent key, got %q", issuedKey)
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()
	journalDir := t.TempDir()
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(journalDir, "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	fake.SetEchoDefault(true)
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	var supervisorLog bytes.Buffer
	client.OnError = func(err error) { fmt.Fprintln(&supervisorLog, err) }
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not connect")
	}
	waitFor(t, func() bool {
		pending, err := f.db.PendingSpawnsForMachine(context.Background(), f.machineID)
		return err == nil && len(pending) == 0
	})
	status, card, raw := f.spawnRequest(t, "lost-send", "worker")
	if status != 201 || card.ID == "" {
		t.Fatalf("replay=%d %+v %+v", status, card, raw)
	}
	cards, err := f.db.AgentCardsForRoom(context.Background(), f.room.ID)
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards=%+v %v", cards, err)
	}
	response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "journal leakage probe"}, true)
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("probe message %d", response.StatusCode)
	}
	dispatcher := NewDispatcher(f.srv)
	waitFor(t, func() bool {
		_, _ = dispatcher.Pass(context.Background())
		messages, err := f.db.ListMessagesAfter(context.Background(), f.room.ID, 0)
		if err != nil {
			return false
		}
		for _, message := range messages {
			if message.AuthorID == card.ID && strings.Contains(message.Body, "journal leakage probe") {
				return true
			}
		}
		return false
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintln(&supervisorLog, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	if strings.Contains(supervisorLog.String(), "aak_") || strings.Contains(supervisorLog.String(), issuedKey) {
		t.Fatal("raw agent key leaked to supervisor log")
	}
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() { // per-agent MCP configs intentionally hold the key outside the journal
			continue
		}
		contents, err := os.ReadFile(filepath.Join(journalDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, []byte("aak_")) || bytes.Contains(contents, []byte(issuedKey)) {
			t.Fatalf("raw agent key leaked to journal file %s", entry.Name())
		}
	}
}

func TestHarnessModelsOwnerEditDrivesCatalogAndSpawnChecks(t *testing.T) {
	f := newFixture(t)
	otherCookie, otherCSRF := registerSecondUser(t, f, "second")
	codex := f.harness(t, "codex", "native")
	url := "/api/machines/" + f.machineID + "/launchers/" + codex.ID + "/models"
	edit := map[string]any{"models": []store.HarnessModel{{Slug: "gpt-host", Label: "Host default", Note: "the machine's own default", Default: true}, {Slug: "gpt-other", Label: "Other"}}}
	// The owner's list is on the harness listing.
	read := func() []store.HarnessModel {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/machines/"+f.machineID+"/launchers", nil, false)
		defer resp.Body.Close()
		var listed struct {
			Launchers []struct {
				Config store.Launcher       `json:"config"`
				Models []store.HarnessModel `json:"models"`
			} `json:"launchers"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&listed) != nil {
			t.Fatalf("listing: %d", resp.StatusCode)
		}
		for _, row := range listed.Launchers {
			if row.Config.ID == codex.ID {
				return row.Models
			}
		}
		t.Fatal("harness missing from the listing")
		return nil
	}
	if got := read(); len(got) != 3 || got[0].Slug != "gpt-6-sol" || !got[0].Default {
		t.Fatalf("starting list: %+v", got)
	}
	resp := f.requestAs(t, otherCookie, otherCSRF, http.MethodPut, url, edit)
	if resp.StatusCode != 403 {
		t.Fatalf("non-owner edit: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPut, url, edit, false)
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF: %d", resp.StatusCode)
	}
	resp.Body.Close()
	for name, body := range map[string]any{
		"bad id":     map[string]any{"models": []store.HarnessModel{{Slug: "bad id", Label: "x", Default: true}}},
		"no default": map[string]any{"models": []store.HarnessModel{{Slug: "gpt-x", Label: "x"}}},
	} {
		resp = f.request(http.MethodPut, url, body, true)
		if resp.StatusCode != 400 {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if got := read(); len(got) != 3 {
		t.Fatalf("a refused list must change nothing: %+v", got)
	}
	resp = f.request(http.MethodPut, "/api/machines/"+f.machineID+"/launchers/mln_missing/models", edit, true)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown harness: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPut, url, edit, true)
	if resp.StatusCode != 200 {
		t.Fatalf("owner edit: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := read(); len(got) != 2 || got[0].Note != "the machine's own default" || !got[0].Default {
		t.Fatalf("saved list: %+v", got)
	}
	// The spawn check follows this harness's own list; the pairing of tool and provider is fixed in code.
	listed := func(model string) bool {
		_, ok, err := f.srv.checkSpawnModel(context.Background(), f.machineID, "codex", "native", "", model)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !listed("gpt-host") || listed("gpt-6-sol") {
		t.Fatal("the spawn check must follow the edited list")
	}
	if model, ok, _ := f.srv.checkSpawnModel(context.Background(), f.machineID, "codex", "native", "", ""); !ok || model != "gpt-host" {
		t.Fatalf("an empty model means the harness default: %q %v", model, ok)
	}
	if ok, _ := f.srv.listedAdapter(context.Background(), spawnRequest{Tool: "codex", Provider: "anthropic"}); ok {
		t.Fatal("a provider that does not belong to the tool is not listed")
	}
}

func TestRemovedModelKeepsKeyedSpawnReplayButRejectsNewRequests(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "claude", Environment: adapter.Environment{Variant: "native"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	spawn := func(key string) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"role_key": "worker", "tool": "claude", "variant": "native", "model": "haiku"})
		req, _ := http.NewRequest("POST", f.http.URL+"/api/rooms/"+f.room.ID+"/agents", bytes.NewReader(body))
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
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		return resp.StatusCode, raw
	}
	status, first := spawn("keyed-spawn")
	if status != 201 {
		t.Fatalf("first spawn %d %+v", status, first)
	}
	f.setHarnessModels(t, f.harness(t, "claude", "native"), store.HarnessModel{Slug: "sonnet", Label: "Sonnet", Default: true})
	if status, replay := spawn("keyed-spawn"); status != 201 || replay["id"] != first["id"] {
		t.Fatalf("a keyed retry must replay the stored spawn after the model left the harness's list: %d %+v", status, replay)
	}
	if status, raw := spawn("new-spawn"); status != 400 || raw["error"] != "model_unlisted" {
		t.Fatalf("a new request for a removed model must be rejected: %d %+v", status, raw)
	}
}
