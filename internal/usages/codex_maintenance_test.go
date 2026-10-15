package usages

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

type fakeCodexExecutor struct {
	inspect func(string) error
	write   string
	flag    string
	err     error
}

func (f *fakeCodexExecutor) Refresh(_ context.Context, dir, flag string) error {
	f.flag = flag
	if f.inspect != nil {
		if err := f.inspect(dir); err != nil {
			return err
		}
	}
	if f.write != "" {
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(f.write), 0o600); err != nil {
			return err
		}
	}
	return f.err
}

func codexAuth(access, refresh, last string) string {
	return `{"tokens":{"access_token":"` + access + `","refresh_token":"` + refresh + `"},"last_refresh":"` + last + `"}`
}

func testCodexMaintainer(exec refreshExecutor, now time.Time) *codexMaintainer {
	return &codexMaintainer{executor: exec, available: func() bool { return true }, reachable: func(context.Context) bool { return true }, clock: &fakeClock{now: now}, refresh: map[string]*codexMaintenanceState{}, warm: map[string]*codexMaintenanceState{}}
}

func TestCodexRefreshForceExpiresAndCopiesBack(t *testing.T) {
	now := time.Unix(9_000_000, 0).UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	next := codexAuth("new", "r2", now.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := &fakeCodexExecutor{write: next, inspect: func(dir string) error {
		creds, _, err := loadCodexAuth(filepath.Join(dir, "auth.json"))
		if err == nil || creds.AccessToken != "" {
			t.Fatalf("temp access token was not blanked")
		}
		return nil
	}}
	m := testCodexMaintainer(exec, now)
	p := ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}
	attempted, err := m.refreshSync(context.Background(), p, "--model gpt-5.6-luna")
	if !attempted || err != nil {
		t.Fatalf("refresh = %v %v", attempted, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != next {
		t.Fatalf("copy-back=%s", got)
	}
	if exec.flag != "--model gpt-5.6-luna" {
		t.Fatalf("flag=%q", exec.flag)
	}
}

func TestCodexRefreshCopiesBackRotationDespiteExecutorError(t *testing.T) {
	now := time.Unix(9_100_000, 0).UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	next := codexAuth("new", "r2", now.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := &fakeCodexExecutor{write: next, err: errors.New("model turn rate limited")}
	m := testCodexMaintainer(exec, now)

	attempted, err := m.refreshSync(context.Background(), ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}, DefaultCodexModelFlag)
	if !attempted || err != nil {
		t.Fatalf("refresh = attempted %v, err %v; want successful rotated copy-back", attempted, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != next {
		t.Fatalf("copy-back=%s, want %s", got, next)
	}
}

func TestCodexRefreshReturnsExecutorErrorWithoutRotation(t *testing.T) {
	now := time.Unix(9_200_000, 0).UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	executorErr := errors.New("model turn rate limited")
	m := testCodexMaintainer(&fakeCodexExecutor{write: old, err: executorErr}, now)

	_, err := m.refreshSync(context.Background(), ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}, DefaultCodexModelFlag)
	if !errors.Is(err, executorErr) {
		t.Fatalf("refresh error = %v, want original executor error", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != old {
		t.Fatalf("stored auth changed without rotation: %s", got)
	}
}

func TestCodexRefreshCASRejectsChangedProfile(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	changed := codexAuth("external", "r3", now.Format(time.RFC3339))
	next := codexAuth("new", "r2", now.Add(time.Minute).Format(time.RFC3339))
	os.WriteFile(path, []byte(old), 0o600)
	exec := &fakeCodexExecutor{write: next, inspect: func(string) error { return os.WriteFile(path, []byte(changed), 0o600) }}
	m := testCodexMaintainer(exec, now)
	m.rescueRoot = t.TempDir()
	_, err := m.refreshSync(context.Background(), ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}, DefaultCodexModelFlag)
	if !errors.Is(err, errClaudeRefreshProfileChanged) {
		t.Fatalf("err=%v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != changed {
		t.Fatal("mid-flight credentials overwritten")
	}
	assertRotatedRescue(t, m.rescueRoot, "codex", "work", next)
}

func TestCodexRotationSurvivesSwitchAway(t *testing.T) {
	now := time.Now().UTC()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	live := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	next := codexAuth("new", "r2", now.Format(time.RFC3339))
	if err := os.WriteFile(live, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	switchRoot := filepath.Join(root, "switcher")
	runSwitcherDriver(t, "setup", switchRoot, home, "codex")
	stored := filepath.Join(switchRoot, "profiles", "codex", "main", "auth.json")
	exec := &fakeCodexExecutor{write: next, inspect: func(string) error {
		runSwitcherDriver(t, "switch", switchRoot, home, "codex")
		return nil
	}}
	m := testCodexMaintainer(exec, now)
	m.rescueRoot = switchRoot
	m.listProfiles = func() ([]ProfileInfo, error) {
		_, err := os.Stat(stored)
		return []ProfileInfo{{Tool: "codex", Name: "main", CredPath: stored, HasCredentials: err == nil}}, nil
	}
	p := ProfileInfo{Tool: "codex", Name: "main", Active: true, CredPath: live, HasCredentials: true}
	if attempted, err := m.refreshSync(context.Background(), p, ""); !attempted || err != nil {
		t.Fatalf("refresh = %v %v", attempted, err)
	}
	got, err := os.ReadFile(stored)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != next {
		t.Fatalf("stored auth = %s, want %s", got, next)
	}
}

func TestCodexCopyBackPreservesUnrelatedAuthKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	started := []byte(`{"tokens":{"access_token":"old","refresh_token":"r1"},"last_refresh":"2026-01-01T00:00:00Z","account":"before"}`)
	current := []byte(`{"tokens":{"access_token":"old","refresh_token":"r1"},"last_refresh":"2026-01-01T00:00:00Z","account":"updated"}`)
	rotated := []byte(`{"tokens":{"access_token":"new","refresh_token":"r2"},"last_refresh":"2026-01-02T00:00:00Z"}`)
	if err := os.WriteFile(path, current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commitCodexAuthCopyBack(nil, nil, "work", path, started, rotated, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["account"]) != `"updated"` || string(got["tokens"]) != `{"access_token":"new","refresh_token":"r2"}` {
		t.Fatalf("copy-back lost concurrent keys or rotated OAuth: %s", data)
	}
}

func TestCodexCopyBackAcceptsCamelRefreshToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	started := []byte(`{"tokens":{"access_token":"old","refreshToken":"r1"}}`)
	rotated := []byte(`{"tokens":{"access_token":"new","refreshToken":"r2"}}`)
	if err := os.WriteFile(path, started, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commitCodexAuthCopyBack(nil, nil, "work", path, started, rotated, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(rotated) {
		t.Fatalf("copy-back = %s, %v; want %s", got, err, rotated)
	}
}

func TestCodexCopyBackRescuesRotationWhenStartingTokenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	started := []byte(`{"tokens":{"access_token":"old"}}`)
	rotated := []byte(`{"tokens":{"access_token":"new","refresh_token":"r2"}}`)
	if err := os.WriteFile(path, started, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := commitCodexAuthCopyBack(nil, nil, "work", path, started, rotated, root); err == nil {
		t.Fatal("copy-back accepted a missing starting refresh token")
	}
	assertRotatedRescue(t, root, "codex", "work", string(rotated))
}

func TestCodexCopyBackOmitsAbsentLastRefresh(t *testing.T) {
	current := []byte(`{"tokens":{"access_token":"old","refresh_token":"r1"},"account":"keep"}`)
	rotated := []byte(`{"tokens":{"access_token":"new","refresh_token":"r2"}}`)
	merged, err := mergeRotatedCredentials("codex", current, rotated)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["last_refresh"]; exists {
		t.Fatalf("absent last_refresh became %s", got["last_refresh"])
	}
	if string(got["account"]) != `"keep"` {
		t.Fatalf("unrelated account field lost: %s", merged)
	}
}

func TestCodexCopyBackRescuesUnprovenProfileRemoval(t *testing.T) {
	root := t.TempDir()
	err := commitCodexAuthCopyBack(nil, func() ([]ProfileInfo, error) { return nil, nil },
		"work", filepath.Join(root, "auth.json"), []byte(`{"tokens":{"refresh_token":"r1"}}`),
		[]byte(`{"tokens":{"refresh_token":"r2"}}`), root)
	if !errors.Is(err, errClaudeRefreshProfileGone) {
		t.Fatalf("removed profile error = %v", err)
	}
	assertRotatedRescue(t, root, "codex", "work", `{"tokens":{"refresh_token":"r2"}}`)
}

func TestCodexCopyBackKeepsImplicitLocalProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	started := []byte(codexAuth("old", "r1", "2026-01-01T00:00:00Z"))
	rotated := []byte(codexAuth("new", "r2", "2026-01-02T00:00:00Z"))
	if err := os.WriteFile(path, started, 0o600); err != nil {
		t.Fatal(err)
	}
	list := func() ([]ProfileInfo, error) {
		return listProfilesOrLocal(func(string) ([]ProfileInfo, error) { return nil, nil }, "codex",
			ProfileInfo{Tool: "codex", Name: "local", Active: true, CredPath: path, HasCredentials: true})
	}
	if err := commitCodexAuthCopyBack(nil, list, "local", path, started, rotated, t.TempDir()); err != nil {
		t.Fatalf("local copy-back: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(rotated) {
		t.Fatalf("local credentials = %s, %v; want %s", got, err, rotated)
	}
}

func TestCodexWarmupCopiesRotatedCredentialsBack(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	next := codexAuth("new", "r2", now.Format(time.RFC3339))
	os.WriteFile(path, []byte(old), 0o600)
	exec := &fakeCodexExecutor{write: next}
	m := testCodexMaintainer(exec, now)
	attempted, err := m.warmSync(context.Background(), ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}, DefaultCodexModelFlag)
	if !attempted || err != nil {
		t.Fatalf("warm=%v %v", attempted, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != next {
		t.Fatalf("copy-back=%s", got)
	}
}

func TestManagerCodexSmartSpacingExcludesWeeklyCappedProfileAndCount(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	now := time.Unix(10_000_000, 0).UTC()
	clock := &fakeClock{now: now}
	profiles := make([]ProfileInfo, 0, 3)
	entries := make([]CacheEntry, 0, 3)
	for i, name := range []string{"oldest", "middle", "newest"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "auth.json")
		if err := os.WriteFile(path, []byte(codexAuth(name, "refresh", now.Format(time.RFC3339))), 0o600); err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, ProfileInfo{Tool: "codex", Name: name, CredPath: path, HasCredentials: true})
		reset := now.Add(time.Duration(i-3) * time.Hour)
		windows := []Window{sessionWindow(&reset)}
		if name == "oldest" {
			weeklyReset := now.Add(24 * time.Hour)
			windows = append(windows, Window{Key: "codex_spark_weekly", PercentUsed: 100, ResetAt: &weeklyReset})
		}
		entries = append(entries, CacheEntry{Profile: Profile{ProfileName: name, Snapshot: Snapshot{Status: StatusOK, Windows: windows}}})
	}
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ClaudeAutoRefresh = true
	cfg.WarmupMode = WarmupModeSmart
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cache := EmptyCache()
	cache.Snapshots[ProviderCodex] = entries
	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, EmptyRegistry())
	m.SetClock(clock)
	m.claudeAvailableFn = func() bool { return true }
	m.SetProfileLister(func(tool string) ([]ProfileInfo, error) {
		if tool == "codex" {
			return profiles, nil
		}
		return nil, nil
	})
	m.codexMaintainer = testCodexMaintainer(&fakeCodexExecutor{}, now)
	m.codexMaintainer.clock = clock

	m.triggerCodexMaintenance(context.Background())
	if codexWarmupWasAttempted(m.codexMaintainer, "oldest") || !codexWarmupWasAttempted(m.codexMaintainer, "middle") {
		t.Fatal("weekly-capped codex profile was not excluded from smart warmup candidates")
	}
	clock.now = clock.now.Add(claudeSessionWindowDuration / 2)
	m.triggerCodexMaintenance(context.Background())
	if !codexWarmupWasAttempted(m.codexMaintainer, "newest") {
		t.Fatal("weekly-capped codex profile still inflated the smart-spacing count")
	}
}

func codexWarmupWasAttempted(m *codexMaintainer, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.warm[name]
	return state != nil && !state.lastAttempt.IsZero()
}

func TestCodexProfileNeedsRefreshUsesAccessJWTExpiry(t *testing.T) {
	now := time.Now().UTC()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10) + `}`))
	token := "x." + payload + ".y"
	expiry := CodexTokenExpiry(token)
	if expiry == nil {
		t.Fatal("missing expiry")
	}
	p := ProfileInfo{Tool: "codex", HasCredentials: true, ExpiresAt: expiry}
	if !codexProfileNeedsRefresh(p, now) {
		t.Fatal("near-expiry JWT was not eligible")
	}
}

func TestCodexRefreshArgs(t *testing.T) {
	args := codexRefreshArgs("/tmp/p", "--model gpt-5.6-luna")
	want := []string{"codex", "exec", "--skip-git-repo-check", codexMaintenancePrompt, "--model", "gpt-5.6-luna"}
	if got := args[len(args)-len(want):]; !reflect.DeepEqual(got, want) {
		t.Fatalf("tail=%#v", got)
	}
}
