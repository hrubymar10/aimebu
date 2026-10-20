package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

// The "Auto-refresh tokens" setting end to end over the real server, switcher,
// refresher and reconciler. Everything is fake credentials under t.TempDir(); the
// CLI is a fake and the real ~/.claude, ~/.codex and ~/.aimebu* are never read.

type settingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *settingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *settingClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// settingEnv is startupEnv plus a usage reader (the setting lives in its config)
// and a stubbed executor check, with the setting's loops started.
func settingEnv(t *testing.T, left time.Duration, clock *settingClock, runningCodex ...func() ([]string, string, error)) (f *fixture, m *switcher.Manager, live string, cli *startupCLI) {
	t.Helper()
	f, m, live = startupEnv(t, left)
	if len(runningCodex) > 0 {
		f.srv.tokenSettingRunningCodex = runningCodex[0]
		codexLive := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
		if err := os.MkdirAll(filepath.Dir(codexLive), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(codexLive, []byte(fullCodexCreds("cx0", "ref-cx0", time.Now().Add(6*time.Hour), time.Now())), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := m.Import(switcher.ToolCodex, "main"); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Usages = reader
	f.srv.TokenAutoRefreshAvailable = func() bool { return true }
	if clock != nil {
		f.srv.TokenSettingClock = clock.Now
	}
	cli = &startupCLI{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetExecutor("codex", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	return f, m, live, cli
}

func TestTokenSettingCodexWaitingReasonInResponse(t *testing.T) {
	f, _, _, _ := settingEnv(t, 6*time.Hour, nil, func() ([]string, string, error) {
		return []string{"pid 4242", "container abc: 7"}, "host and docker", nil
	})
	postSettings(t, f, map[string]any{"token_auto_refresh": true})
	reason := waitState(t, f, "codex", "waiting")
	want := "waiting for running codex sessions to end (2 found); retries automatically"
	if reason != want {
		t.Fatalf("settings reason = %q, want %q", reason, want)
	}
}

func postSettings(t *testing.T, f *fixture, body map[string]any) map[string]any {
	t.Helper()
	resp := f.request("POST", "/api/usages/settings", body, true)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST settings %v: %d %s", body, resp.StatusCode, raw)
	}
	var out struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.Settings
}

func toolState(settings map[string]any, tool string) (status, reason string) {
	states, _ := settings["token_auto_refresh_state"].(map[string]any)
	one, _ := states[tool].(map[string]any)
	status, _ = one["status"].(string)
	reason, _ = one["reason"].(string)
	return status, reason
}

func waitState(t *testing.T, f *fixture, tool, want string) (reason string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(25 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		resp := f.request("GET", "/api/usages", nil, false)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var body struct {
			Settings map[string]any `json:"settings"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("GET /api/usages: %v %s", err, raw)
		}
		got, reason = toolState(body.Settings, tool)
		if got == want {
			return reason
		}
	}
	t.Fatalf("%s state = %q (%s), want %q", tool, got, reason, want)
	return ""
}

func fileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTokenSettingMigratesWaitsRetriesAndUnmigrates(t *testing.T) {
	clock := &settingClock{now: time.Now()}
	f, m, live, cli := settingEnv(t, 10*time.Minute, clock) // inside migrate's 15-minute guard

	// The POST answers at once; the work happens on the worker. (That the answer is
	// `pending` is pinned deterministically in the usages package: here the worker can
	// finish before the response is read, so the state is pending or already waiting.)
	settings := postSettings(t, f, map[string]any{"token_auto_refresh": true})
	if settings["token_auto_refresh"] != true || settings["claude_auto_refresh"] != true || settings["token_auto_refresh_available"] != true {
		t.Fatalf("settings after ON = %v", settings)
	}
	if status, _ := toolState(settings, "claude"); status != "pending" && status != "waiting" {
		t.Fatalf("the POST must answer pending (or the worker's quick waiting), got %q", status)
	}
	if reason := waitState(t, f, "claude", "waiting"); !strings.Contains(reason, "15m") {
		t.Fatalf("waiting reason = %q", reason)
	}
	waitState(t, f, "codex", "n/a") // no codex profile: nothing to convert, shown neutrally
	if m.TokenCopiesFor(switcher.ToolClaude) {
		t.Fatal("migrated despite the refusal")
	}

	// The token is renewed (a login) and the retry backoff elapses.
	if err := os.WriteFile(live, []byte(startupClaudeCreds("acc-A0", "ref-A0", time.Now().Add(6*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	waitState(t, f, "claude", "migrated")
	if !m.TokenCopiesFor(switcher.ToolClaude) || refreshTokenInFile.MatchString(fileString(t, live)) {
		t.Fatal("migration did not strip the live file")
	}

	postSettings(t, f, map[string]any{"token_auto_refresh": false})
	waitState(t, f, "claude", "off")
	if m.TokenCopiesFor(switcher.ToolClaude) {
		t.Fatal("the marker survived OFF")
	}
	if got := fileString(t, live); !strings.Contains(got, `"refreshToken":"ref-A0"`) {
		t.Fatalf("OFF did not put the full credential back in the live file: %s", got)
	}
	if info, err := os.Lstat(m.StoredCredPath(switcher.ToolClaude, "A")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the active profile was not re-linked: %v %v", info, err)
	}
	if total, twice := cli.calls(); len(twice) != 0 || total > 1 {
		t.Fatalf("spend-once broken: total=%d twice=%v", total, twice)
	}
}

// Must-have: MCP logins accumulated in the live Claude file survive every
// step of the setting's life, a switch and its return included.
func TestTokenSettingKeepsMCPLoginsByteIdentical(t *testing.T) {
	const mcp = `{"plane":{"accessToken":"plane-mcp"},"incident":{"accessToken":"incident-mcp"}}`
	f, m, live, _ := settingEnv(t, 6*time.Hour, nil)
	if err := m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "B"), []byte(startupClaudeCreds("acc-B0", "ref-B0", time.Now().Add(6*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	// Put the MCP logins into the live file and into the stored full credential
	// (what a worker's write would leave while the link model is in force).
	withMCP := strings.TrimSuffix(startupClaudeCreds("acc-A0", "ref-A0", time.Now().Add(6*time.Hour)), "}") + `,"mcpOAuth":` + mcp + `}`
	if err := os.WriteFile(live, []byte(withMCP), 0o600); err != nil {
		t.Fatal(err)
	}
	mcpOf := func(step string) {
		t.Helper()
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(fileString(t, live)), &doc); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if string(doc["mcpOAuth"]) != mcp {
			t.Fatalf("%s: mcpOAuth = %s, want %s", step, doc["mcpOAuth"], mcp)
		}
	}

	postSettings(t, f, map[string]any{"token_auto_refresh": true})
	waitState(t, f, "claude", "migrated")
	mcpOf("after migrate")
	if _, _, err := m.Switch(switcher.ToolClaude, "B"); err != nil {
		t.Fatalf("switch A->B: %v", err)
	}
	mcpOf("after switch A->B")
	if _, _, err := m.Switch(switcher.ToolClaude, "A"); err != nil {
		t.Fatalf("switch B->A: %v", err)
	}
	mcpOf("after switch back")

	postSettings(t, f, map[string]any{"token_auto_refresh": false})
	waitState(t, f, "claude", "off")
	mcpOf("after un-migrate")
	if got := fileString(t, live); !strings.Contains(got, `"refreshToken":"ref-A0"`) {
		t.Fatalf("OFF: the live file lacks the full credential: %s", got)
	}
}

func TestTokenSettingLegacyKeyAndPrecedence(t *testing.T) {
	f, _, _, _ := settingEnv(t, 6*time.Hour, nil)
	for _, tc := range []struct {
		body map[string]any
		want bool
	}{
		{map[string]any{"claude_auto_refresh": true}, true},
		{map[string]any{"claude_auto_refresh": true, "token_auto_refresh": false}, false},
		{map[string]any{"token_auto_refresh": true}, true},
	} {
		s := postSettings(t, f, tc.body)
		if s["token_auto_refresh"] != tc.want || s["claude_auto_refresh"] != tc.want {
			t.Fatalf("POST %v => %v, want %v", tc.body, s, tc.want)
		}
	}
	postSettings(t, f, map[string]any{"token_auto_refresh": false})
	waitState(t, f, "claude", "off")
}

// An upgrade from a build whose toggle was `claude_auto_refresh`: ON carries over,
// owns the cleanup and the tool migrates, then OFF takes it back.
func TestTokenSettingLegacyOnCarriesOverAndCanBeTurnedOff(t *testing.T) {
	f, m, live, _ := settingEnv(t, 6*time.Hour, nil)
	store := f.srv.Usages.ExtraStore()
	if err := os.WriteFile(filepath.Join(store.Root, "config.json"), []byte(`{"claude_auto_refresh":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig()
	if err != nil || !cfg.TokenAutoRefresh || !cfg.TokenAutoRefreshManaged {
		t.Fatalf("legacy ON did not carry ownership: %+v %v", cfg, err)
	}
	f.srv.tokenSetting.Request()
	waitState(t, f, "claude", "migrated")
	postSettings(t, f, map[string]any{"token_auto_refresh": false})
	waitState(t, f, "claude", "off")
	if m.TokenCopiesFor(switcher.ToolClaude) || !strings.Contains(fileString(t, live), `"refreshToken":"ref-A0"`) {
		t.Fatal("OFF after a legacy ON did not restore the full credential")
	}
}

// OFF on a tool that was never migrated shows off even when the link repair it
// runs cannot work (corrupt switcher state): the failure is logged, not shown.
func TestTokenSettingOffStaysOffWhenLinkRepairFails(t *testing.T) {
	f, m, _, _ := settingEnv(t, 6*time.Hour, nil)
	// The setting is off but still owns cleanup (managed), the tool was never
	// migrated, and the state file is corrupt so the link repair fails.
	if _, err := f.srv.Usages.ExtraStore().UpdateConfig(func(c *providers.Config) error {
		c.TokenAutoRefresh, c.TokenAutoRefreshManaged = false, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Root(), "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.tokenSetting.Request()
	// (GET /api/usages needs a readable switcher state, so read the snapshot.)
	for _, tool := range []string{"claude", "codex"} {
		var got string
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
			got = f.srv.Usages.Snapshot().Settings.TokenAutoRefreshState[tool].Status
			if got == "off" {
				break
			}
		}
		if got != "off" {
			t.Fatalf("%s state = %q, want off (a link-repair error must not read as waiting)", tool, got)
		}
	}
	// The pass settled: the setting no longer owns anything.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if cfg, err := f.srv.Usages.ExtraStore().LoadConfig(); err == nil && !cfg.TokenAutoRefreshManaged {
			return
		}
	}
	t.Fatal("the managed flag was never cleared")
}
