package usages

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// codexWarmupFixtureManager builds a Manager with three codex profiles, a seeded
// cache and a warm-everything config, the shape the legacy codex warmup acts on.
func codexWarmupFixtureManager(t *testing.T) *Manager {
	t.Helper()
	store := NewStoreAt(t.TempDir())
	now := time.Unix(10_000_000, 0).UTC()
	clock := &fakeClock{now: now}
	profiles := make([]ProfileInfo, 0, 3)
	entries := make([]CacheEntry, 0, 3)
	for i, name := range []string{"oldest", "middle", "newest"} {
		path := filepath.Join(t.TempDir(), "auth.json")
		if err := os.WriteFile(path, []byte(codexAuth(name, "refresh", now.Format(time.RFC3339))), 0o600); err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, ProfileInfo{Tool: "codex", Name: name, CredPath: path, HasCredentials: true})
		reset := now.Add(time.Duration(i-3) * time.Hour)
		entries = append(entries, CacheEntry{Profile: Profile{ProfileName: name, Snapshot: Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(&reset)}}}})
	}
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ClaudeAutoRefresh = true
	cfg.WarmupMode = WarmupModeForce
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
	return m
}

// While the token refresher owns refresh, no legacy warmup may run: a warmup
// hands a CLI a copy of the FULL stored credential, and the CLI can refresh and
// rotate it outside the refresher's spend-once journal.
func TestLegacyClaudeWarmupStandsDownWhileTheTokenRefresherOwnsRefresh(t *testing.T) {
	m, store, exec, _ := warmupTriggerFixture(t)
	setWarmupSettings(t, store, true, WarmupModeForce)

	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 1) // control: without the refresher the warmup runs

	m2, store2, exec2, _ := warmupTriggerFixture(t)
	setWarmupSettings(t, store2, true, WarmupModeForce)
	tr := NewTargetRefresher(m2)
	tr.SetEnabled(func() bool { return true })
	tr.SetToolEnabled(func(string) bool { return true })
	m2.SetTargetRefresher(tr)
	m2.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec2, 0)

	tr.SetEnabled(func() bool { return false })
	m2.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec2, 1) // a disabled refresher does not own refresh: legacy behaviour returns
}

func TestLegacyCodexWarmupStandsDownWhileTheTokenRefresherOwnsRefresh(t *testing.T) {
	control := codexWarmupFixtureManager(t)
	control.triggerCodexMaintenance(context.Background())
	if !codexWarmupWasAttempted(control.codexMaintainer, "oldest") && !codexWarmupWasAttempted(control.codexMaintainer, "middle") && !codexWarmupWasAttempted(control.codexMaintainer, "newest") {
		t.Fatal("control: without the token refresher a codex warmup should have run")
	}
	m := codexWarmupFixtureManager(t)
	tr := NewTargetRefresher(m)
	tr.SetEnabled(func() bool { return true })
	tr.SetToolEnabled(func(string) bool { return true })
	m.SetTargetRefresher(tr)
	m.triggerCodexMaintenance(context.Background())
	if codexWarmupWasAttempted(m.codexMaintainer, "oldest") || codexWarmupWasAttempted(m.codexMaintainer, "middle") || codexWarmupWasAttempted(m.codexMaintainer, "newest") {
		t.Fatal("a codex warmup ran while the token refresher owns refresh")
	}
	otherTool := codexWarmupFixtureManager(t)
	otherRefresher := NewTargetRefresher(otherTool)
	otherRefresher.SetEnabled(func() bool { return true })
	otherRefresher.SetToolEnabled(func(tool string) bool { return tool == "claude" })
	otherTool.SetTargetRefresher(otherRefresher)
	otherTool.triggerCodexMaintenance(context.Background())
	if !codexWarmupWasAttempted(otherTool.codexMaintainer, "oldest") && !codexWarmupWasAttempted(otherTool.codexMaintainer, "middle") && !codexWarmupWasAttempted(otherTool.codexMaintainer, "newest") {
		t.Fatal("codex warmup stayed down while only claude was migrated")
	}
}
