package usages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// newTestWarmer builds a warmer wired to a fake executor with no docker gating,
// so tests exercise the cooldown / in-flight guard / eligibility in isolation.
func newTestWarmer(exec refreshExecutor, clock Clock) *claudeWarmer {
	return &claudeWarmer{
		executor:  exec,
		available: func() bool { return true },
		reachable: func(context.Context) bool { return true },
		clock:     clock,
		cooldown:  claudeWarmupCooldown,
		state:     map[string]*claudeWarmupState{},
	}
}

func sessionWindow(reset *time.Time) Window {
	return Window{Key: "session", ResetAt: reset, WindowDurationSeconds: 5 * 3600}
}

// ── Eligibility: switcher-side identity gate ─────────────────────────────

func TestClaudeProfileEligibleForWarmup(t *testing.T) {
	cases := []struct {
		name    string
		profile ProfileInfo
		want    bool
	}{
		{"inactive claude with creds", ProfileInfo{Tool: "claude", HasCredentials: true}, true},
		{"active with credentials", ProfileInfo{Tool: "claude", Active: true, HasCredentials: true}, true},
		{"no credentials", ProfileInfo{Tool: "claude", HasCredentials: false}, false},
		{"non-claude tool", ProfileInfo{Tool: "codex", HasCredentials: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeProfileEligibleForWarmup(tc.profile); got != tc.want {
				t.Fatalf("claudeProfileEligibleForWarmup = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── Out-of-window detection: the chosen 5h-window semantic ───────────────

func TestClaudeSnapshotOutOfWindow(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	future := now.Add(2 * time.Hour)
	past := now.Add(-2 * time.Hour)

	cases := []struct {
		name string
		snap Snapshot
		want bool
	}{
		{
			name: "no session window at all -> out of window",
			snap: Snapshot{Status: StatusOK, Windows: []Window{{Key: "weekly"}}},
			want: true,
		},
		{
			name: "session with future reset -> in window",
			snap: Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(&future)}},
			want: false,
		},
		{
			name: "fresh session near 0% but future reset -> in window",
			snap: Snapshot{Status: StatusOK, Windows: []Window{{Key: "session", PercentUsed: 0, ResetAt: &future}}},
			want: false,
		},
		{
			name: "session with past reset -> out of window",
			snap: Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(&past)}},
			want: true,
		},
		{
			name: "session with nil reset -> out of window",
			snap: Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(nil)}},
			want: true,
		},
		{
			name: "non-OK snapshot -> never treated as out of window",
			snap: Snapshot{Status: StatusAuthMissing, Windows: []Window{{Key: "weekly"}}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeSnapshotOutOfWindow(tc.snap, now); got != tc.want {
				t.Fatalf("claudeSnapshotOutOfWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── Warm runs the executor and persists rotated credentials ──────────────

// When the container does NOT rotate credentials (no refresh happened), warmup
// still succeeds and leaves the stored profile untouched.
func TestClaudeWarmRunsExecutorNoRotation(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	body := claudeCredsWithExpiry("tok", now.Add(time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, body)

	exec := &fakeExecutor{}
	w := newTestWarmer(exec, &fakeClock{now: now})
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true}

	attempted, err := w.maybeWarmSync(context.Background(), profile)
	if !attempted {
		t.Fatal("expected warmup to be attempted")
	}
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if exec.callCount() != 1 {
		t.Fatalf("executor called %d times, want 1", exec.callCount())
	}
	// No rotation ⇒ no copy-back ⇒ stored profile unchanged.
	got, _ := os.ReadFile(credPath)
	if string(got) != body {
		t.Fatalf("unchanged run must leave stored credentials untouched:\n got=%s\nwant=%s", got, body)
	}
}

// CRITICAL: when the container rotates the OAuth tokens (refresh-on-use for an
// idle/aged account), warmup MUST copy the rotated credentials back — otherwise
// the stored profile keeps a dead refresh token. Note warmup does NOT require
// the expiry to advance the way auto-refresh does.
func TestClaudeWarmupCopiesBackRotatedCreds(t *testing.T) {
	now := time.Unix(2_500_000, 0)
	// Aged account: access token already expired, so the container refreshes.
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)

	exec := &fakeExecutor{writeNew: rotated}
	w := newTestWarmer(exec, &fakeClock{now: now})
	w.reresolve = func(name string) (ProfileInfo, bool) {
		return ProfileInfo{Tool: "claude", Name: name, CredPath: credPath, HasCredentials: true}, true
	}
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true}

	attempted, err := w.maybeWarmSync(context.Background(), profile)
	if !attempted || err != nil {
		t.Fatalf("warmup should run and succeed: attempted=%v err=%v", attempted, err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != rotated {
		t.Fatalf("rotated credentials not copied back:\n got=%s\nwant=%s", got, rotated)
	}
}

// ── CAS identity guard on warmup copy-back ───────────────────────────────

func TestClaudeWarmupCASAllowsActiveProfile(t *testing.T) {
	now := time.Unix(2_600_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)

	exec := &fakeExecutor{writeNew: rotated}
	w := newTestWarmer(exec, &fakeClock{now: now})
	// Active profiles are eligible warmup targets, so copy-back is intentional.
	w.reresolve = func(name string) (ProfileInfo, bool) {
		return ProfileInfo{Tool: "claude", Name: name, Active: true, CredPath: credPath, HasCredentials: true}, true
	}
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true}

	_, err := w.maybeWarmSync(context.Background(), profile)
	if err != nil {
		t.Fatalf("active-profile warmup failed: %v", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != rotated {
		t.Fatalf("active-profile rotated credentials not copied back, got=%s", got)
	}
}

func TestClaudeWarmupCASAbortsWhenCredentialPathChanged(t *testing.T) {
	now := time.Unix(2_650_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)
	_, otherPath := writeStoredProfile(t, old)

	w := newTestWarmer(&fakeExecutor{writeNew: rotated}, &fakeClock{now: now})
	w.reresolve = func(name string) (ProfileInfo, bool) {
		return ProfileInfo{Tool: "claude", Name: name, Active: true, CredPath: otherPath, HasCredentials: true}, true
	}
	profile := ProfileInfo{Tool: "claude", Name: "work", Active: true, CredPath: credPath, HasCredentials: true}

	_, err := w.maybeWarmSync(context.Background(), profile)
	if !errors.Is(err, errClaudeRefreshProfileChanged) {
		t.Fatalf("expected changed-path CAS abort, got %v", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != old {
		t.Fatalf("stored credentials must be unchanged after path change, got=%s", got)
	}
}

func TestClaudeWarmupCASAbortsWhenStoredFileChanged(t *testing.T) {
	now := time.Unix(2_700_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	changed := claudeCredsWithExpiry("switched-in-tok", now.Add(9*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)

	// Executor mutates the real stored file mid-warmup (a switch capturing into
	// it) before returning the rotated temp file.
	w := newTestWarmer(&mutatingExecutor{storedPath: credPath, mutateTo: changed, writeNew: rotated}, &fakeClock{now: now})
	w.reresolve = func(name string) (ProfileInfo, bool) {
		return ProfileInfo{Tool: "claude", Name: name, CredPath: credPath, HasCredentials: true}, true
	}
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true}

	_, err := w.maybeWarmSync(context.Background(), profile)
	if !errors.Is(err, errClaudeRefreshProfileChanged) {
		t.Fatalf("expected changed-file CAS abort, got %v", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != changed {
		t.Fatalf("stored credentials must retain the mid-warmup change, got=%s", got)
	}
}

func TestClaudeWarmupCASAbortsWhenProfileRemoved(t *testing.T) {
	now := time.Unix(2_800_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)

	w := newTestWarmer(&removingExecutor{storedPath: credPath, writeNew: rotated}, &fakeClock{now: now})
	w.reresolve = func(string) (ProfileInfo, bool) { return ProfileInfo{}, false }
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true}

	_, err := w.maybeWarmSync(context.Background(), profile)
	if !errors.Is(err, errClaudeRefreshProfileGone) {
		t.Fatalf("expected profile-gone CAS abort, got %v", err)
	}
	if _, statErr := os.Stat(credPath); !os.IsNotExist(statErr) {
		t.Fatalf("stored credentials should stay removed, stat err=%v", statErr)
	}
}

// ── 1h cooldown: every attempt, regardless of outcome ────────────────────

func TestClaudeWarmupCooldownAppliesRegardlessOfOutcome(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	clock := &fakeClock{now: now}
	body := claudeCredsWithExpiry("tok", now.Add(time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, body)

	// Executor SUCCEEDS every time — the cooldown must still block retries within
	// the hour, because success is unobservable inline.
	exec := &fakeExecutor{}
	w := newTestWarmer(exec, clock)
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true}

	attempted, err := w.maybeWarmSync(context.Background(), profile)
	if !attempted || err != nil {
		t.Fatalf("first attempt should run and succeed: attempted=%v err=%v", attempted, err)
	}
	// Retry inside the cooldown window: skipped.
	if attempted, _ := w.maybeWarmSync(context.Background(), profile); attempted {
		t.Fatal("second attempt within 1h cooldown should be skipped")
	}
	// Just before the hour elapses: still skipped.
	clock.now = now.Add(claudeWarmupCooldown - time.Second)
	if attempted, _ := w.maybeWarmSync(context.Background(), profile); attempted {
		t.Fatal("attempt just before cooldown expiry should be skipped")
	}
	if exec.callCount() != 1 {
		t.Fatalf("executor called %d times during cooldown, want 1", exec.callCount())
	}
	// After the hour: allowed again.
	clock.now = now.Add(claudeWarmupCooldown + time.Second)
	if attempted, _ := w.maybeWarmSync(context.Background(), profile); !attempted {
		t.Fatal("attempt after cooldown should run")
	}
	if exec.callCount() != 2 {
		t.Fatalf("executor called %d times after cooldown, want 2", exec.callCount())
	}
}

// A failing warmup is also cooldown-limited (the 1h floor is outcome-agnostic).
func TestClaudeWarmupCooldownAfterFailure(t *testing.T) {
	now := time.Unix(3_500_000, 0)
	clock := &fakeClock{now: now}
	body := claudeCredsWithExpiry("tok", now.Add(time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, body)

	exec := &fakeExecutor{err: errors.New("boom")}
	w := newTestWarmer(exec, clock)
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true}

	if attempted, err := w.maybeWarmSync(context.Background(), profile); !attempted || err == nil {
		t.Fatalf("first attempt should run and fail: attempted=%v err=%v", attempted, err)
	}
	if attempted, _ := w.maybeWarmSync(context.Background(), profile); attempted {
		t.Fatal("retry within cooldown after failure should be skipped")
	}
	if exec.callCount() != 1 {
		t.Fatalf("executor called %d times, want 1 during cooldown", exec.callCount())
	}
}

// ── In-flight guard: no two concurrent warmups for one account ───────────

func TestClaudeWarmupInFlightGuard(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	clock := &fakeClock{now: now}
	body := claudeCredsWithExpiry("tok", now.Add(time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, body)

	entered := make(chan struct{})
	release := make(chan struct{})
	exec := &fakeExecutor{entered: entered, release: release}
	w := newTestWarmer(exec, clock)
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true}

	done := make(chan error, 1)
	go func() {
		_, err := w.maybeWarmSync(context.Background(), profile)
		done <- err
	}()
	<-entered // first warmup is inside the executor, holding the in-flight slot

	if attempted, _ := w.maybeWarmSync(context.Background(), profile); attempted {
		t.Fatal("second concurrent attempt should be skipped by the in-flight guard")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first warmup failed: %v", err)
	}
	if exec.callCount() != 1 {
		t.Fatalf("executor called %d times, want 1", exec.callCount())
	}
}

// ── Manager auto-warmup poller trigger ───────────────────────────────────

// warmupTriggerFixture wires a Manager with three claude profiles (an idle
// inactive one out of window, a busy inactive one in window, and an active one
// out of window) plus a seeded cache, so triggerClaudeAutoWarmup can be
// exercised against a realistic profile/snapshot set.
func warmupTriggerFixture(t *testing.T) (*Manager, *Store, *fakeExecutor, time.Time) {
	t.Helper()
	store := NewStoreAt(t.TempDir())
	now := time.Unix(5_000_000, 0)

	idleBody := claudeCredsWithExpiry("idle-tok", now.Add(time.Hour).UnixMilli())
	_, idlePath := writeStoredProfile(t, idleBody)
	busyBody := claudeCredsWithExpiry("busy-tok", now.Add(time.Hour).UnixMilli())
	_, busyPath := writeStoredProfile(t, busyBody)
	activeBody := claudeCredsWithExpiry("active-tok", now.Add(time.Hour).UnixMilli())
	_, activePath := writeStoredProfile(t, activeBody)

	m := NewManager(store, EmptyRegistry())
	m.SetClock(&fakeClock{now: now})
	m.claudeAvailableFn = func() bool { return true }
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{
			{Tool: "claude", Name: "idle", CredPath: idlePath, HasCredentials: true},
			{Tool: "claude", Name: "busy", CredPath: busyPath, HasCredentials: true},
			{Tool: "claude", Name: "live", Active: true, CredPath: activePath, HasCredentials: true},
		}
	})
	exec := &fakeExecutor{}
	m.claudeWarmer = newTestWarmer(exec, &fakeClock{now: now})

	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	if err := store.WithLock(func() error {
		cache := EmptyCache()
		cache.Snapshots[ProviderClaudeCode] = []CacheEntry{
			{Profile: Profile{ProfileName: "idle", Snapshot: Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(&past)}}}},
			{Profile: Profile{ProfileName: "busy", Snapshot: Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(&future)}}}},
			{Profile: Profile{ProfileName: "live", Snapshot: Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(&past)}}}},
		}
		return store.SaveCache(cache)
	}); err != nil {
		t.Fatal(err)
	}
	return m, store, exec, now
}

func setWarmupSettings(t *testing.T, store *Store, autoRefresh bool, mode string) {
	t.Helper()
	cfg, _ := store.LoadConfig()
	cfg.ClaudeAutoRefresh = autoRefresh
	cfg.WarmupMode = mode
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// Both auto-refresh AND auto-warmup on → only the idle, inactive, out-of-window
// account is warmed; the in-window and active profiles are left alone. A second
// tick is cooldown-limited.
func TestManagerTriggerClaudeAutoWarmupForceMode(t *testing.T) {
	m, store, exec, now := warmupTriggerFixture(t)
	setWarmupSettings(t, store, true, WarmupModeForce)
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	nonmatching := now.Add(time.Minute)
	cfg.ClaudeModelFlag = "--model claude-sonnet-4-6"
	cfg.WarmupSchedule = fmt.Sprintf("%d %d * * *", nonmatching.Minute(), nonmatching.Hour())
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 2)
	if got := exec.lastModelFlag(); got != "--model claude-sonnet-4-6" {
		t.Fatalf("warmup model flag = %q, want configured flag", got)
	}
	if !warmupWasAttempted(m.claudeWarmer, "live") {
		t.Fatal("active out-of-window profile was not warmed")
	}

	// A second immediate tick is cooldown-limited → no new warmup.
	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 2)
}

// Auto-warmup off (auto-refresh on) → no-op.
func TestManagerTriggerClaudeAutoWarmupOffMode(t *testing.T) {
	m, store, exec, _ := warmupTriggerFixture(t)
	setWarmupSettings(t, store, true, WarmupModeOff)

	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 0)
}

// Auto-refresh off (auto-warmup on) → no-op: warmup is chained to auto-refresh.
func TestManagerTriggerClaudeAutoWarmupRefreshOff(t *testing.T) {
	m, store, exec, _ := warmupTriggerFixture(t)
	setWarmupSettings(t, store, false, WarmupModeForce)

	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 0)
}

// Feature unavailable (harness-docker-ctrl absent) → no-op even with both on.
func TestManagerTriggerClaudeAutoWarmupUnavailable(t *testing.T) {
	m, store, exec, _ := warmupTriggerFixture(t)
	m.claudeAvailableFn = func() bool { return false }
	setWarmupSettings(t, store, true, WarmupModeForce)

	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 0)
}

func smartSpacingTriggerFixture(t *testing.T) (*Manager, *Store, *fakeExecutor, *fakeClock) {
	t.Helper()
	store := NewStoreAt(t.TempDir())
	now := time.Unix(6_000_000, 0)
	clock := &fakeClock{now: now}
	exec := &fakeExecutor{}
	profiles := make([]ProfileInfo, 0, 3)
	entries := make([]CacheEntry, 0, 3)
	for i, name := range []string{"oldest", "middle", "newest"} {
		body := claudeCredsWithExpiry(name+"-tok", now.Add(time.Hour).UnixMilli())
		_, credPath := writeStoredProfile(t, body)
		profiles = append(profiles, ProfileInfo{Tool: "claude", Name: name, Active: name == "newest", CredPath: credPath, HasCredentials: true})
		reset := now.Add(time.Duration(i-3) * time.Hour)
		entries = append(entries, CacheEntry{Profile: Profile{
			ProfileName: name,
			Snapshot:    Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(&reset)}},
		}})
	}

	m := NewManager(store, EmptyRegistry())
	m.SetClock(clock)
	m.claudeAvailableFn = func() bool { return true }
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return profiles
	})
	m.claudeWarmer = newTestWarmer(exec, clock)
	if err := store.WithLock(func() error {
		cache := EmptyCache()
		cache.Snapshots[ProviderClaudeCode] = entries
		return store.SaveCache(cache)
	}); err != nil {
		t.Fatal(err)
	}
	setWarmupSettings(t, store, true, WarmupModeForce)
	return m, store, exec, clock
}

func enableSmartSpacing(t *testing.T, store *Store) {
	t.Helper()
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.WarmupMode = WarmupModeSmart
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func warmupWasAttempted(w *claudeWarmer, name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	state := w.state[name]
	return state != nil && !state.lastAttempt.IsZero()
}

func TestManagerSmartSpacingPicksLongestIdleAndEnforcesGap(t *testing.T) {
	m, store, exec, clock := smartSpacingTriggerFixture(t)
	enableSmartSpacing(t, store)

	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 1)
	if !warmupWasAttempted(m.claudeWarmer, "oldest") {
		t.Fatal("first smart-spaced scan did not pick the longest-idle profile")
	}

	// All three credential-bearing profiles count toward N, including active
	// "newest". If active profiles were excluded this would be 5h/2 and the
	// boundary assertion below would not start the second account.
	gap := claudeSessionWindowDuration / 3
	clock.now = clock.now.Add(gap - time.Minute)
	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 1)
	if warmupWasAttempted(m.claudeWarmer, "middle") {
		t.Fatal("different profile warmed before the spacing gap elapsed")
	}

	clock.now = clock.now.Add(time.Minute)
	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 2)
	if !warmupWasAttempted(m.claudeWarmer, "middle") {
		t.Fatal("second smart-spaced scan did not pick the next longest-idle profile")
	}
}

func TestManagerSmartSpacingOffWarmsAllCandidates(t *testing.T) {
	m, _, exec, _ := smartSpacingTriggerFixture(t)
	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 3)
}
