package usages

import (
	"context"
	"encoding/base64"
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
	_, err := m.refreshSync(context.Background(), ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}, DefaultCodexModelFlag)
	if !errors.Is(err, errClaudeRefreshProfileChanged) {
		t.Fatalf("err=%v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != changed {
		t.Fatal("mid-flight credentials overwritten")
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
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool == "codex" {
			return profiles
		}
		return nil
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
