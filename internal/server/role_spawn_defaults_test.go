package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestSpawnUsesRoleDefaultAndReplaysSameChoice(t *testing.T) {
	f := newFixture(t)
	f.seed()
	codex := f.harness(t, "codex", "native")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE machine_launcher SET enabled=1 WHERE id=?`, codex.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE role SET spawn_defaults_json=? WHERE role_key='worker'`,
			`[{"launcher_id":"`+codex.ID+`","model":"gpt-6-sol"}]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 64), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "codex", Environment: adapter.Environment{Variant: "native"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	wrongMachine, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents",
		strings.NewReader(`{"role_key":"worker","machine_id":"mch_other"}`))
	wrongMachine.Header.Set("Content-Type", "application/json")
	wrongMachine.Header.Set("X-CSRF-Token", f.csrf)
	wrongMachine.Header.Set("Idempotency-Key", "role-default-wrong-machine")
	wrongMachine.AddCookie(f.cookie)
	wrongResponse, err := http.DefaultClient.Do(wrongMachine)
	if err != nil {
		t.Fatal(err)
	}
	wrongResponse.Body.Close()
	if wrongResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("requested machine must not be replaced by default: %d", wrongResponse.StatusCode)
	}
	spawn := func() (int, store.AgentCard) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents", strings.NewReader(`{"role_key":"worker"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", "role-default-spawn")
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var card store.AgentCard
		if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, card
	}
	status, first := spawn()
	if status != http.StatusCreated || first.ID == "" || !first.DefaultUsed || first.Model != "gpt-6-sol" || first.DefaultSelection == nil || first.DefaultSelection.LauncherID != codex.ID {
		t.Fatalf("default spawn: %d %+v", status, first)
	}
	status, replay := spawn()
	if status != http.StatusCreated || replay.ID != first.ID || !replay.DefaultUsed || replay.DefaultSelection == nil || replay.DefaultSelection.LauncherID != codex.ID {
		t.Fatalf("keyed replay: %d %+v", status, replay)
	}
	defaults, err := f.db.RoleSpawnDefaults(t.Context(), f.project.ID, "worker")
	if err != nil || defaults.NextPosition != 1 {
		t.Fatalf("cursor after replay: %+v %v", defaults, err)
	}
	messages, err := f.db.ListMessagesLatest(t.Context(), f.room.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundNote := false
	for _, message := range messages {
		foundNote = foundNote || strings.Contains(message.Body, "spawned per role default: codex/native/gpt-6-sol")
	}
	if !foundNote {
		t.Fatalf("default spawn has no room notice: %+v", messages)
	}
}

func TestSpawnExplicitDefaultWithOmittedModel(t *testing.T) {
	f := newFixture(t)
	f.seed()
	codex := f.harness(t, "codex", "native")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE machine_launcher SET enabled=1 WHERE id=?`, codex.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE role SET spawn_defaults_json=? WHERE role_key='worker'`,
			`[{"launcher_id":"`+codex.ID+`","model":"gpt-6-sol"}]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "codex", Environment: adapter.Environment{Variant: "native"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	request, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents",
		strings.NewReader(`{"role_key":"worker","tool":"codex","variant":"native","launcher_id":"`+codex.ID+`","machine_id":"`+f.machineID+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", f.csrf)
	request.Header.Set("Idempotency-Key", "role-default-omitted-model")
	request.AddCookie(f.cookie)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var card store.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated || !card.DefaultUsed || card.Model != "gpt-6-sol" {
		t.Fatalf("omitted model should use role default: %d %+v body=%s", response.StatusCode, card, body)
	}
	defaults, err := f.db.RoleSpawnDefaults(t.Context(), f.project.ID, "worker")
	if err != nil || defaults.NextPosition != 1 {
		t.Fatalf("cursor after omitted model: %+v %v", defaults, err)
	}
}

func TestLeadSpawnMatchesDefaultByVariantWithoutLauncherID(t *testing.T) {
	f := newFixture(t)
	f.seed()
	codex := f.harness(t, "codex", "native")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE machine_launcher SET enabled=1 WHERE id=?`, codex.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE role SET spawn_defaults_json=? WHERE role_key='reviewer'`,
			`[{"launcher_id":"`+codex.ID+`","model":"gpt-6-sol"}]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "codex", Environment: adapter.Environment{Variant: "native"}},
			{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	_, key := f.createToolAgent(t, "role_leader_default")
	response := f.toolPost(t, "/api/agent/agents", key, "role-default-variant", map[string]string{
		"room_id": f.room.ID, "role_key": "reviewer", "tool": "codex", "variant": "native", "model": "gpt-6-sol",
	})
	defer response.Body.Close()
	var card store.AgentCard
	if err := json.NewDecoder(response.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated || !card.DefaultUsed || card.DefaultSelection == nil ||
		card.DefaultSelection.LauncherID != codex.ID {
		t.Fatalf("lead default without launcher ID: %d %+v", response.StatusCode, card)
	}
}

func TestMatchingRoleDefaultInheritsOnlyOmittedMachine(t *testing.T) {
	f := newFixture(t)
	launcher := f.harness(t, "codex", "native")
	selection := roleSpawnSelection{Request: spawnRequest{MachineID: "mch_remote", Tool: "codex", Variant: "native",
		LauncherID: launcher.ID, Model: "gpt-6-sol"}}
	request := spawnRequest{Tool: "codex", Variant: "native", Model: "gpt-6-sol"}
	if err := f.srv.inheritRoleDefaultMachine(t.Context(), &request, selection); err != nil || request.MachineID != "mch_remote" {
		t.Fatalf("matching choice did not inherit machine: %+v %v", request, err)
	}
	request = spawnRequest{MachineID: "mch_explicit", Tool: "codex", Variant: "native", Model: "gpt-6-sol"}
	if err := f.srv.inheritRoleDefaultMachine(t.Context(), &request, selection); err != nil || request.MachineID != "mch_explicit" {
		t.Fatalf("explicit machine was replaced: %+v %v", request, err)
	}
	request = spawnRequest{Tool: "codex", Variant: "native", Model: "gpt-6-luna"}
	if err := f.srv.inheritRoleDefaultMachine(t.Context(), &request, selection); err != nil || request.MachineID != "" {
		t.Fatalf("different model inherited default machine: %+v %v", request, err)
	}
}

func TestSpawnInheritsMatchingRoleDefaultMachine(t *testing.T) {
	f := newFixture(t)
	f.seed()
	var remote store.Machine
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		remote, err = tx.CreateMachine(store.Machine{Name: "remote-default", KeyHash: "remote-key", Approved: true})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,?,?)`,
			f.project.ID, remote.ID, "/remote/project"); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at)
			SELECT ?,?,id,? FROM user WHERE login='owner'`, f.project.ID, remote.ID, "now")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	launcher, err := f.db.SaveLauncher(t.Context(), "owner", store.Launcher{
		MachineID: remote.ID, Tool: "codex", Name: "native", Command: "codex", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceHarnessModels(t.Context(), "owner", remote.ID, launcher.ID,
		[]store.HarnessModel{{Slug: "gpt-6-sol", Label: "GPT-6 Sol", Default: true}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE role SET spawn_defaults_json=? WHERE role_key='worker'`,
			`[{"launcher_id":"`+launcher.ID+`","model":"gpt-6-sol"}]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: remote.ID, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), bound: map[string]int{},
		supportsPathProbe: true, capabilities: []protocol.CapabilityReport{{Tool: "codex", Environment: adapter.Environment{Variant: "native"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[remote.ID] = machine
	f.srv.machineMu.Unlock()
	t.Cleanup(func() { close(machine.done) })
	go func() {
		for {
			select {
			case request := <-machine.sendQueue:
				if request.Type != protocol.PathProbe {
					continue
				}
				machine.harnessMu.Lock()
				waiter := machine.pathPending[request.ReqID]
				machine.harnessMu.Unlock()
				waiter <- protocol.Frame{V: protocol.Version, Type: protocol.PathResult, ReqID: request.ReqID, CanonicalPath: request.CheckoutPath}
			case <-machine.done:
				return
			}
		}
	}()
	request, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents",
		strings.NewReader(`{"role_key":"worker","tool":"codex","variant":"native","model":"gpt-6-sol"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", f.csrf)
	request.Header.Set("Idempotency-Key", "inherit-remote-default")
	request.AddCookie(f.cookie)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var card store.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated || card.MachineID != remote.ID || !card.DefaultUsed {
		t.Fatalf("matching role default must choose remote machine: %d %+v body=%s", response.StatusCode, card, body)
	}
}

func TestNextRoleSpawnDefaultSkipsUnavailableAndAdvances(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	codex := f.harness(t, "codex", "native")
	claude := f.harness(t, "claude", "native")
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE machine_launcher SET enabled=1 WHERE id IN (?,?)`, codex.ID, claude.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE role SET spawn_defaults_json=?,spawn_pick_mode='in_turn' WHERE role_key='worker'`,
			`[{"launcher_id":"`+claude.ID+`","model":"haiku"},{"launcher_id":"`+codex.ID+`","model":"gpt-6-sol"},{"launcher_id":"`+codex.ID+`","model":"gpt-6-luna"}]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "codex", Environment: adapter.Environment{Variant: "native"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	first, ok, err := f.srv.nextRoleSpawnDefault(ctx, f.project.ID, "worker", "")
	if err != nil || !ok || first.Choice.LauncherID != codex.ID || first.Choice.Model != "gpt-6-sol" || first.Steps != 2 {
		t.Fatalf("first: %+v %t %v", first, ok, err)
	}
	if _, ok, err := f.srv.nextRoleSpawnDefault(ctx, f.project.ID, "worker", "mch_other"); err != nil || ok {
		t.Fatalf("other machine must have no default: %t %v", ok, err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		return tx.AdvanceRoleSpawnCursor(f.project.ID, first.Defaults.RoleID, first.Defaults.NextPosition, first.Steps)
	}); err != nil {
		t.Fatal(err)
	}
	second, ok, err := f.srv.nextRoleSpawnDefault(ctx, f.project.ID, "worker", "")
	if err != nil || !ok || second.Choice.Model != "gpt-6-luna" || second.Steps != 1 {
		t.Fatalf("second: %+v %t %v", second, ok, err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE role SET spawn_pick_mode='randomly' WHERE role_key='worker'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		item, ok, err := f.srv.nextRoleSpawnDefault(ctx, f.project.ID, "worker", "")
		if err != nil || !ok || item.Choice.LauncherID != codex.ID {
			t.Fatalf("random available: %+v %t %v", item, ok, err)
		}
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()
	_, ok, err = f.srv.nextRoleSpawnDefault(ctx, f.project.ID, "worker", "")
	if err != nil || ok {
		t.Fatalf("offline fallback: %t %v", ok, err)
	}
}

func TestSpawnDefaultUnavailableWarnsAndAcceptsManualChoice(t *testing.T) {
	f := newFixture(t)
	f.seed()
	codex := f.harness(t, "codex", "native")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE role SET spawn_defaults_json=? WHERE role_key='worker'`,
			`[{"launcher_id":"`+codex.ID+`","model":"gpt-6-sol"}]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents", strings.NewReader(`{"role_key":"worker"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.Header.Set("Idempotency-Key", "missing-default")
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("omitted unavailable default: %d", resp.StatusCode)
	}
	status, _, raw := f.spawnRequest(t, "manual-after-default", "worker")
	if status != http.StatusCreated {
		t.Fatalf("manual choice: %d %+v", status, raw)
	}
	messages, err := f.db.ListMessagesLatest(t.Context(), f.room.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var unavailable, manual bool
	for _, message := range messages {
		unavailable = unavailable || strings.Contains(message.Body, "No configured default agent for worker is available")
		manual = manual || strings.Contains(message.Body, "manual harness and model were used")
	}
	if !unavailable || !manual {
		t.Fatalf("missing room warnings: %+v", messages)
	}
}

func TestLeadSpawnDeviationRequiresReason(t *testing.T) {
	f := newFixture(t)
	f.seed()
	codex := f.harness(t, "codex", "native")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE machine_launcher SET enabled=1 WHERE id=?`, codex.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE role SET spawn_defaults_json=? WHERE role_key='reviewer'`,
			`[{"launcher_id":"`+codex.ID+`","model":"gpt-6-sol"}]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "codex", Environment: adapter.Environment{Variant: "native"}}, {Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	selection, available, err := f.srv.nextRoleSpawnDefault(t.Context(), f.project.ID, "reviewer", "")
	if err != nil || !available || selection.Choice.LauncherID != codex.ID {
		t.Fatalf("configured default: %+v %t %v", selection, available, err)
	}
	_, key := f.createToolAgent(t, "role_leader_default")
	manual := map[string]string{"room_id": f.room.ID, "role_key": "reviewer", "tool": "fake", "variant": "test", "model": "test"}
	resp := f.toolPost(t, "/api/agent/agents", key, "deviation-no-reason", manual)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing reason: %d", resp.StatusCode)
	}
	manual["reason"] = "A small isolated review is enough"
	resp = f.toolPost(t, "/api/agent/agents", key, "deviation-with-reason", manual)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("explained deviation: %d", resp.StatusCode)
	}
	messages, err := f.db.ListMessagesLatest(t.Context(), f.room.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		found = found || strings.Contains(message.Body, manual["reason"])
	}
	if !found {
		t.Fatalf("deviation reason missing from room: %+v", messages)
	}
}
