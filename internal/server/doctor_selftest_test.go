package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

func selfTestRequest(t *testing.T, f *fixture, key string) (int, protocol.DoctorSelfTest) {
	status, answer, _ := selfTestRequestRaw(t, f, key)
	return status, answer
}

func selfTestRequestRaw(t *testing.T, f *fixture, key string) (int, protocol.DoctorSelfTest, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/doctor/self-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var answer protocol.DoctorSelfTest
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, answer, string(raw)
}

func TestDoctorSelfTestListsEnabledDockerLauncherModelsBehindTheDoctorGuard(t *testing.T) {
	f := newFixture(t)
	f.seed()
	var err error
	if f.srv.Usages, err = usages.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := f.srv.EnableDoctorKey(root); err != nil {
		t.Fatal(err)
	}
	key := readDoctorKeyFile(t, root)
	if status, _ := selfTestRequest(t, f, ""); status != 403 {
		t.Fatalf("no key: %d", status)
	}
	if status, _ := selfTestRequest(t, f, "not-the-key"); status != 403 {
		t.Fatalf("wrong key: %d", status)
	}
	// The local machine has a Docker launcher with two models, a native one and a Docker one with no model, and a disabled Docker one.
	for _, q := range []string{
		`INSERT INTO machine_launcher(id,machine_id,tool,name,label,command,enabled,containerized) VALUES ('lch_a',?,'claude','st-a','Claude','claude-docker',1,1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,label,command,enabled,containerized) VALUES ('lch_native',?,'pi','st-native','Pi','pi',1,0)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,label,command,enabled,containerized) VALUES ('lch_empty',?,'codex','st-empty','Codex','codex-docker',1,1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,label,command,enabled,containerized) VALUES ('lch_off',?,'vibe','st-off','Vibe','vibe-docker',0,1)`,
	} {
		if _, err := f.exec(q, f.machineID); err != nil {
			t.Fatal(err)
		}
	}
	models := []store.HarnessModel{{Slug: "claude-sonnet-5-5", Label: "Sonnet", Default: true}, {Slug: "claude/odd:model", Label: "Odd"}}
	if err := f.db.ReplaceHarnessModels(t.Context(), "owner", f.machineID, "lch_a", models); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceHarnessModels(t.Context(), "owner", f.machineID, "lch_off", []store.HarnessModel{{Slug: "m", Label: "M", Default: true}}); err != nil {
		t.Fatal(err)
	}
	def := func(id, tool string, enabled, docker bool) protocol.LauncherRow {
		return protocol.LauncherRow{LauncherDefinition: protocol.LauncherDefinition{ID: id, Tool: tool, Name: "st-" + strings.TrimPrefix(id, "lch_"), Enabled: enabled, Containerized: docker}, ResolvedCommand: "/bin/" + id, Available: true}
	}
	machine := &machineConn{id: f.machineID, done: make(chan struct{}), supportsLaunchers: true,
		launchers: []protocol.LauncherRow{def("lch_a", "claude", true, true), def("lch_native", "pi", true, false), def("lch_empty", "codex", true, true), def("lch_off", "vibe", false, true)}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()

	// Not the local supervisor's machine yet: nothing to list.
	status, answer := selfTestRequest(t, f, key)
	if status != 200 || len(answer.Targets) != 0 || len(answer.Launchers) != 0 || len(answer.Usages) == 0 {
		t.Fatalf("a machine that is not the local supervisor: %d %+v", status, answer)
	}
	f.srv.SetLocalSupervisorID(f.machineID)
	status, answer = selfTestRequest(t, f, key)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	if len(answer.Skipped) != 1 || answer.Skipped[0].ID != "lch_native" || answer.Skipped[0].Tool != "pi" || answer.Skipped[0].Name != "st-native" || answer.Skipped[0].Reason != "not a Docker launcher" {
		t.Fatalf("an enabled native launcher is reported as skipped, a disabled or empty Docker one is not: %+v", answer.Skipped)
	}
	if len(answer.Launchers) != 1 || answer.Launchers[0].ID != "lch_a" || answer.Launchers[0].ResolvedCommand != "/bin/lch_a" {
		t.Fatalf("launchers: %+v", answer.Launchers)
	}
	if len(answer.Targets) != 2 || answer.Targets[0].Name != "claude-st-a-claude-sonnet-5-5" || answer.Targets[0].Model != "claude-sonnet-5-5" ||
		answer.Targets[1].Name != "claude-st-a-claude-odd-model" || answer.Targets[1].LauncherID != "lch_a" || answer.Targets[1].Tool != "claude" {
		t.Fatalf("targets: %+v", answer.Targets)
	}
	// A disconnected supervisor lists nothing.
	close(machine.done)
	if _, answer = selfTestRequest(t, f, key); len(answer.Targets) != 0 {
		t.Fatalf("a disconnected machine: %+v", answer)
	}
}

func TestDoctorSelfTestAnswersNullUsagesWithoutASnapshotAndLeaksNoEnvironmentValue(t *testing.T) {
	const secret = "sekrit-value-0123456789"
	t.Setenv("CORP_SELFTEST_KEY", secret)
	f := newFixture(t)
	f.seed()
	f.srv.Usages = nil // a fresh install: no snapshot yet
	root := t.TempDir()
	if _, err := f.srv.EnableDoctorKey(root); err != nil {
		t.Fatal(err)
	}
	key := readDoctorKeyFile(t, root)
	if _, err := f.exec(`INSERT INTO machine_launcher(id,machine_id,tool,name,label,command,enabled,containerized) VALUES ('lch_o',?,'opencode','st-o','OpenCode','opencode-docker',1,1)`, f.machineID); err != nil {
		t.Fatal(err)
	}
	provider := store.HarnessProvider{ID: "corp", Name: "Corp", NPM: store.HarnessProviderNPM, BaseURL: "https://llm.example.com/v1", KeyEnv: "CORP_SELFTEST_KEY"}
	if err := f.db.ReplaceHarnessProvidersIfRev(t.Context(), "owner", f.machineID, "lch_o", []store.HarnessProvider{provider}, "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceHarnessModels(t.Context(), "owner", f.machineID, "lch_o", []store.HarnessModel{{Slug: "corp/m1", Label: "M1", Default: true}}); err != nil {
		t.Fatal(err)
	}
	row := protocol.LauncherRow{LauncherDefinition: protocol.LauncherDefinition{ID: "lch_o", Tool: "opencode", Name: "st-o", Enabled: true, Containerized: true,
		Args: []string{"--flag"}, EnvRefs: map[string]string{"TOKEN": "CORP_SELFTEST_KEY"}}, ResolvedCommand: "/bin/o", Available: true}
	machine := &machineConn{id: f.machineID, done: make(chan struct{}), supportsLaunchers: true, launchers: []protocol.LauncherRow{row}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	f.srv.SetLocalSupervisorID(f.machineID)
	status, answer, raw := selfTestRequestRaw(t, f, key)
	if status != 200 || string(answer.Usages) != "null" || len(answer.Targets) != 1 || answer.Targets[0].Providers[0].KeyEnv != "CORP_SELFTEST_KEY" {
		t.Fatalf("status %d usages %q targets %+v", status, answer.Usages, answer.Targets)
	}
	if answer.Launchers[0].EnvRefs["TOKEN"] != "CORP_SELFTEST_KEY" || len(answer.Launchers[0].Args) != 1 {
		t.Fatalf("args and env_refs are names the route returns: %+v", answer.Launchers[0])
	}
	if strings.Contains(raw, secret) {
		t.Fatalf("the answer carries a value from the environment: %s", raw)
	}
}
