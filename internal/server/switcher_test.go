package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

func TestSwitcherRoutesShowImpactAndRefuseRunningTurn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(root, ".aimebu2"))
	f := newFixture(t)
	m, err := switcher.New(filepath.Join(root, ".aimebu2", "switcher"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(root)
	f.srv.Switcher = m
	f.srv.Usages, err = usages.New(filepath.Join(root, "usage-cache"))
	if err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(root, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(live), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"tokens":{"access_token":"a","refresh_token":"r"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"PUT", "/api/switcher", map[string]bool{"enabled": true}, 200},
		{"POST", "/api/switcher/codex/import", map[string]string{"name": "main"}, 201},
		{"POST", "/api/switcher/codex/profiles", map[string]string{"name": "empty"}, 201},
	} {
		resp := f.request(step.method, step.path, step.body, true)
		resp.Body.Close()
		if resp.StatusCode != step.want {
			t.Fatalf("%s %s = %d", step.method, step.path, resp.StatusCode)
		}
	}
	usageResponse := f.request(http.MethodGet, "/api/usages", nil, false)
	var usage struct {
		Providers []usages.Provider `json:"providers"`
	}
	if err := json.NewDecoder(usageResponse.Body).Decode(&usage); err != nil {
		t.Fatal(err)
	}
	usageResponse.Body.Close()
	if usageResponse.StatusCode != 200 || len(usage.Providers) < 1 || len(usage.Providers[0].Profiles) != 2 {
		t.Fatalf("configured profiles absent from usage response: %+v", usage)
	}
	agent, turn, _, _ := f.execution(t)
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE agent SET tool='codex',observed='in_turn' WHERE id=?`, agent.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE turn SET exec='running' WHERE id=?`, turn.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodGet, "/api/switcher", nil, false)
	var status struct {
		Affected map[string][]store.SwitcherAgent `json:"affected_agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(status.Affected["codex"]) != 1 || !status.Affected["codex"][0].Running {
		t.Fatalf("impact=%+v status=%d", status, resp.StatusCode)
	}
	resp = f.request(http.MethodPost, "/api/switcher/codex/use", map[string]string{"name": "empty"}, true)
	var blocked struct {
		Error  string                `json:"error"`
		Agents []store.SwitcherAgent `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&blocked); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || blocked.Error != "agents_running" || len(blocked.Agents) != 1 || blocked.Agents[0].ID != agent.ID {
		t.Fatalf("blocked=%+v status=%d", blocked, resp.StatusCode)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("blocked switch changed live file: %v", err)
	}
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, turn.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodPost, "/api/switcher/codex/use", map[string]string{"name": "empty"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("quiescent switch status=%d", resp.StatusCode)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("empty target did not clear temp live credentials: %v", err)
	}
}

func TestSwitcherUseForceRequiresStoredCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(root, ".aimebu2"))
	f := newFixture(t)
	m, err := switcher.New(filepath.Join(root, ".aimebu2", "switcher"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(root)
	f.srv.Switcher = m
	live := filepath.Join(root, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"tokens":{"access_token":"main","refresh_token":"main-refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := m.Import("codex", "main"); err != nil {
		t.Fatal(err)
	}
	if err := m.Add("codex", "target"); err != nil {
		t.Fatal(err)
	}
	request := func() *http.Response {
		return f.request(http.MethodPost, "/api/switcher/codex/use", map[string]any{"name": "target", "force": true}, true)
	}
	resp := request()
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("force to empty target = %d", resp.StatusCode)
	}
	stored := filepath.Join(root, ".aimebu2", "switcher", "profiles", "codex", "target", "auth.json")
	if err := os.WriteFile(stored, []byte(`{"tokens":{"access_token":"target","refresh_token":"target-refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resp = request()
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("force with stored credentials = %d", resp.StatusCode)
	}
	if got, err := os.ReadFile(live); err != nil || !strings.Contains(string(got), "target-refresh") {
		t.Fatalf("live = %s, %v", got, err)
	}
}

func TestCLIProfileRemovalNotifiesOpenUsagesView(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(root, "aimebu2"))
	f := newFixture(t)
	switcherRoot := filepath.Join(root, "aimebu2", "switcher")
	m, err := switcher.New(switcherRoot)
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(root)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	expired := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":%d}}`, time.Now().Add(-time.Hour).UnixMilli())
	live := filepath.Join(root, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import("claude", "main"); err != nil {
		t.Fatal(err)
	}
	if err := m.Add("claude", "old"); err != nil {
		t.Fatal(err)
	}
	stored := m.StoredCredPath("claude", "old")
	if err := os.WriteFile(stored, []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := usages.New(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	reader.SetProfiles(m.UsageProfiles, m.WithLock)
	f.srv.Usages, f.srv.Switcher = reader, m
	reader.SetUpdateHook(f.srv.usagesChanged)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { reader.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		if _, ok := reader.StoredProfile("claude-code", "old"); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("initial stored profile was not published")
		case <-time.After(20 * time.Millisecond):
		}
	}
	updates, unwatch := f.srv.watchUsages()
	defer unwatch()
	cli, err := switcher.New(switcherRoot)
	if err != nil {
		t.Fatal(err)
	}
	cli.SetHomeDir(root)
	if err := cli.Remove("claude", "old", false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-updates:
	case <-time.After(15 * time.Second):
		t.Fatal("CLI removal did not nudge open Usages views")
	}
	if _, ok := reader.StoredProfile("claude-code", "old"); ok {
		t.Fatal("removed profile stayed in the usage snapshot")
	}
	resp := f.request(http.MethodGet, "/api/switcher", nil, true)
	defer resp.Body.Close()
	var state struct {
		Profiles []switcher.Profile `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("switcher status = %d", resp.StatusCode)
	}
	for _, profile := range state.Profiles {
		if profile.Tool == "claude" && profile.Name == "old" {
			t.Fatal("removed profile stayed in Settings status")
		}
	}
}

func TestSwitcherRouteReadsV1TreeInPlace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(home, ".aimebu"))
	f := newFixture(t)
	root := filepath.Join(home, ".aimebu", "switcher")
	profile := filepath.Join(root, "profiles", "codex", "main", "auth.json")
	live := filepath.Join(home, ".codex", "auth.json")
	for _, dir := range []string{filepath.Dir(profile), filepath.Dir(live)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"version":1,"enabled":true,"active":{"codex":"main"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"tokens":{"access_token":"synthetic","refresh_token":"synthetic-refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(live, profile); err != nil {
		t.Fatal(err)
	}
	m, err := switcher.New(root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(home)
	f.srv.Switcher = m
	response := f.request(http.MethodGet, "/api/switcher", nil, false)
	defer response.Body.Close()
	var result struct {
		Enabled  bool `json:"enabled"`
		Profiles []struct {
			Tool   string `json:"tool"`
			Name   string `json:"name"`
			Active bool   `json:"active"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !result.Enabled || len(result.Profiles) != 1 ||
		result.Profiles[0].Tool != "codex" || result.Profiles[0].Name != "main" || !result.Profiles[0].Active {
		t.Fatalf("v1 tree status=%d result=%+v", response.StatusCode, result)
	}
}

func TestSwitcherRenameRoute(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(root, ".aimebu2"))
	f := newFixture(t)
	m, err := switcher.New(filepath.Join(root, ".aimebu2", "switcher"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(root)
	f.srv.Switcher = m
	live := filepath.Join(root, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(live), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"tokens":{"access_token":"a","refresh_token":"r"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"PUT", "/api/switcher", map[string]bool{"enabled": true}, 200},
		{"POST", "/api/switcher/codex/import", map[string]string{"name": "main"}, 201},
		{"POST", "/api/switcher/codex/profiles", map[string]string{"name": "spare"}, 201},
		{"POST", "/api/switcher/codex/profiles/spare/rename", map[string]string{"name": "main"}, 409},
		{"POST", "/api/switcher/codex/profiles/missing/rename", map[string]string{"name": "other"}, 404},
		{"POST", "/api/switcher/codex/profiles/spare/rename", map[string]string{"name": "a/b"}, 400},
		{"POST", "/api/switcher/codex/profiles/spare/rename", map[string]string{"name": "reserve"}, 200},
		{"POST", "/api/switcher/codex/profiles/main/rename", map[string]string{"name": "primary"}, 200},
	} {
		resp := f.request(step.method, step.path, step.body, true)
		resp.Body.Close()
		if resp.StatusCode != step.want {
			t.Fatalf("%s %s %v = %d, want %d", step.method, step.path, step.body, resp.StatusCode, step.want)
		}
	}
	active, err := m.Active("codex")
	if err != nil || active != "primary" {
		t.Fatalf("active after renaming it = %q, %v", active, err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("rename touched the live credential file: %v", err)
	}
	resp := f.request(http.MethodPost, "/api/switcher/codex/profiles/reserve/rename", map[string]string{"name": "x"}, false)
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("non-owner rename = %d", resp.StatusCode)
	}
}

// Token-copy refusals are states that clear on their own or by one more step,
// never server faults: each maps to 409 with its own message, and a disabled
// migration is the caller's mistake (400).
func TestSwitcherErrorMapsTokenCopyRefusalsToConflict(t *testing.T) {
	for name, err := range map[string]error{
		"active changed":     switcher.ErrActiveChanged,
		"maintenance busy":   switcher.ErrMaintenanceBusy,
		"login pending":      switcher.ErrLoginPending,
		"incoming refresh":   switcher.ErrIncomingRefresh,
		"linked profile":     switcher.ErrLinkedProfile,
		"migration raced":    switcher.ErrMigrationRaced,
		"codex running":      switcher.ErrCodexRunning,
		"token-only copy":    switcher.ErrTargetCopy,
		"invalid credential": switcher.ErrInvalidCredentials,
	} {
		rec := httptest.NewRecorder()
		switcherError(rec, fmt.Errorf("wrapped: %w", err))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), err.Error()) {
			t.Errorf("%s: status %d body %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	switcherError(rec, switcher.ErrMigrateNotEnabled)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("migrate not enabled: status %d", rec.Code)
	}
}
