package usages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaintenanceGuardIsPerToolAndAccount(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	if err := os.WriteFile(a, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"r-a","expiresAt":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"r-b","expiresAt":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newMaintenanceGuard()
	work := ProfileInfo{Tool: "claude", Name: "work", CredPath: a}
	releaseWork, ok := g.acquire("claude", work)
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, ok := g.acquire("claude", work); ok {
		t.Fatal("second acquire of the same account succeeded")
	}
	// A rename keeps the account (same refresh token), so it must NOT free a slot.
	renamed := ProfileInfo{Tool: "claude", Name: "renamed", CredPath: a}
	if _, ok := g.acquire("claude", renamed); ok {
		t.Fatal("a renamed profile with the same refresh token got a free slot")
	}
	if _, ok := g.acquire("claude", ProfileInfo{Tool: "claude", Name: "other", CredPath: b}); !ok {
		t.Fatal("a different account was blocked")
	}
	if _, ok := g.acquire("codex", ProfileInfo{Tool: "codex", Name: "work", CredPath: a}); !ok {
		t.Fatal("a different tool was blocked")
	}
	releaseWork()
	releaseRenamed, ok := g.acquire("claude", renamed)
	if !ok {
		t.Fatal("acquire after release failed")
	}
	releaseRenamed()
	var nilGuard *maintenanceGuard
	if _, ok := nilGuard.acquire("claude", work); !ok {
		t.Fatal("a nil guard must never block")
	}
	if _, ok := nilGuard.acquire("claude", work); !ok {
		t.Fatal("a nil guard must never block")
	}
}

// With a switcher root the exclusion is a flock, so it also holds against a
// second guard (another process in production) and a switch can hold it.
func TestMaintenanceGuardExcludesAcrossGuardsAndHolders(t *testing.T) {
	root := t.TempDir()
	cred := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(cred, []byte(`{"tokens":{"access_token":"x","refresh_token":"r1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := ProfileInfo{Tool: "codex", Name: "work", CredPath: cred}
	first, second := newMaintenanceGuard(), newMaintenanceGuard()
	first.root, second.root = root, root
	releaseFirst, ok := first.acquire("codex", profile)
	if !ok {
		t.Fatal("first guard could not acquire")
	}
	if _, ok := second.acquire("codex", profile); ok {
		t.Fatal("a second guard (second process) acquired the same account")
	}
	if _, err := HoldMaintenance(root, "codex", "work", cred, 150*time.Millisecond); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("hold while a run is in flight = %v, want ErrMaintenanceBusy", err)
	}
	held := make(chan func(), 1)
	go func() {
		release, err := HoldMaintenance(root, "codex", "work", cred, 5*time.Second)
		if err != nil {
			t.Errorf("hold after release = %v", err)
			held <- func() {}
			return
		}
		held <- release
	}()
	time.Sleep(200 * time.Millisecond)
	releaseFirst()
	releaseHold := <-held
	// While a switch holds the account, no run may start for it.
	if _, ok := second.acquire("codex", profile); ok {
		t.Fatal("a run started while a switch held the account")
	}
	releaseHold()
	releaseSecond, ok := second.acquire("codex", profile)
	if !ok {
		t.Fatal("acquire after the switch released failed")
	}
	releaseSecond()
	if release, err := HoldMaintenance("", "codex", "work", cred, time.Millisecond); err != nil || release == nil {
		t.Fatalf("no root must never wait: %v", err)
	}
}

// A token rotation changes the account identity (the hash of the stored
// refresh token) while a run is in flight. The claim's release must belong to
// the claim, not to the profile name, or a second job would release (or leak)
// the wrong lock.
func TestMaintenanceGuardReleaseSurvivesRotationMidRun(t *testing.T) {
	root := t.TempDir()
	cred := filepath.Join(t.TempDir(), "auth.json")
	write := func(refresh string) {
		if err := os.WriteFile(cred, []byte(`{"tokens":{"access_token":"x","refresh_token":"`+refresh+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("r1")
	g := newMaintenanceGuard()
	g.root = root
	profile := ProfileInfo{Tool: "codex", Name: "work", CredPath: cred}
	releaseA, ok := g.acquire("codex", profile)
	if !ok {
		t.Fatal("first acquire failed")
	}
	write("r2") // the run's copy-back stored the rotated token
	releaseB, ok := g.acquire("codex", profile)
	if !ok {
		t.Fatal("the rotated account identity should be a separate claim")
	}
	releaseA()
	// B's claim must still hold: only A's identity was released.
	if _, ok := g.acquire("codex", profile); ok {
		t.Fatal("releasing claim A released claim B")
	}
	releaseB()
	releaseC, ok := g.acquire("codex", profile)
	if !ok {
		t.Fatal("acquire after both releases failed")
	}
	releaseC()
	releaseC() // idempotent
	// No lock files are left behind.
	entries, err := os.ReadDir(filepath.Join(root, maintenanceDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("lock files leaked: %v", entries)
	}
}

// Warmup and auto-refresh both rotate the claude refresh token. While one runs
// for a profile the other must be skipped, not queued behind or beside it.
func TestClaudeWarmupAndRefreshNeverOverlapOnOneProfile(t *testing.T) {
	now := time.Unix(8_100_000, 0)
	clock := &fakeClock{now: now}
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}

	guard := newMaintenanceGuard()
	entered, release := make(chan struct{}), make(chan struct{})
	refreshExec := &fakeExecutor{writeNew: rotated, entered: entered, release: release}
	warmExec := &fakeExecutor{}
	r := newTestRefresher(refreshExec, clock)
	r.guard = guard
	w := newTestWarmer(warmExec, clock)
	w.guard = guard

	done := make(chan error, 1)
	go func() {
		_, err := r.maybeRefreshSync(context.Background(), profile)
		done <- err
	}()
	<-entered // the refresh is inside the executor

	if attempted, _ := w.maybeWarmSync(context.Background(), profile); attempted {
		t.Fatal("warmup started while a refresh was in flight for the same profile")
	}
	if warmExec.callCount() != 0 {
		t.Fatal("warmup executor ran during the refresh")
	}
	// A different account is not blocked.
	_, otherPath := writeStoredProfile(t, `{"claudeAiOauth":{"accessToken":"o","refreshToken":"other-refresh","expiresAt":1}}`)
	other := ProfileInfo{Tool: "claude", Name: "other", CredPath: otherPath, HasCredentials: true}
	if attempted, _ := w.maybeWarmSync(context.Background(), other); !attempted {
		t.Fatal("an unrelated profile was blocked by the guard")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// Once the refresh has finished the profile can be warmed again.
	if attempted, _ := w.maybeWarmSync(context.Background(), profile); !attempted {
		t.Fatal("warmup stayed blocked after the refresh finished")
	}
}

func TestClaudeRefreshAndWarmupReleaseGuardOnEveryExit(t *testing.T) {
	now := time.Unix(8_200_000, 0)
	clock := &fakeClock{now: now}
	_, credPath := writeStoredProfile(t, "not json")
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
	guard := newMaintenanceGuard()
	r := newTestRefresher(&fakeExecutor{}, clock)
	r.guard = guard
	w := newTestWarmer(&fakeExecutor{}, clock)
	w.guard = guard
	// Both fail validating the stored credentials; the guard must still be freed.
	if _, err := r.maybeRefreshSync(context.Background(), profile); err == nil {
		t.Fatal("refresh of invalid credentials succeeded")
	}
	releaseAfterRefresh, ok := guard.acquire("claude", profile)
	if !ok {
		t.Fatal("guard leaked after a failed refresh")
	}
	releaseAfterRefresh()
	if _, err := w.maybeWarmSync(context.Background(), profile); err == nil {
		t.Fatal("warmup of invalid credentials succeeded")
	}
	if _, ok := guard.acquire("claude", profile); !ok {
		t.Fatal("guard leaked after a failed warmup")
	}
}

func TestCodexRefreshAndWarmupNeverOverlapOnOneProfile(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))), 0o600); err != nil {
		t.Fatal(err)
	}
	next := codexAuth("new", "r2", now.Format(time.RFC3339))
	profile := ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}

	entered, release := make(chan struct{}), make(chan struct{})
	blocking := &blockingCodexExecutor{write: next, entered: entered, release: release}
	m := testCodexMaintainer(blocking, now)
	m.guard = newMaintenanceGuard()

	done := make(chan error, 1)
	go func() {
		_, err := m.refreshSync(context.Background(), profile, "")
		done <- err
	}()
	<-entered
	if attempted, _ := m.warmSync(context.Background(), profile, ""); attempted {
		t.Fatal("codex warmup started while a refresh was in flight for the same profile")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("codex refresh: %v", err)
	}
}

type blockingCodexExecutor struct {
	write   string
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCodexExecutor) Refresh(_ context.Context, dir, _ string) error {
	b.entered <- struct{}{}
	<-b.release
	return os.WriteFile(filepath.Join(dir, "auth.json"), []byte(b.write), 0o600)
}

// The server touches only inactive snapshots, including at (and past) expiry.
func TestManagerAutoRefreshSkipsActiveProfileAtExpiry(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	now := time.Unix(9_100_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Hour).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, activePath := writeStoredProfile(t, old)
	_, idlePath := writeStoredProfile(t, old)

	m := NewManager(store, EmptyRegistry())
	m.SetClock(&fakeClock{now: now})
	m.claudeAvailableFn = func() bool { return true }
	m.SetProfileLister(func(tool string) ([]ProfileInfo, error) {
		if tool != "claude" {
			return nil, nil
		}
		expired := ptrTime(now.Add(-time.Hour))
		return []ProfileInfo{
			{Tool: "claude", Name: "live", Active: true, CredPath: activePath, HasCredentials: true, ExpiresAt: expired},
			{Tool: "claude", Name: "idle", CredPath: idlePath, HasCredentials: true, ExpiresAt: expired},
		}, nil
	})
	exec := &fakeExecutor{writeNew: rotated}
	m.claudeRefresher = newTestRefresher(exec, &fakeClock{now: now})
	m.claudeRefresher.listProfiles = func() ([]ProfileInfo, error) { return m.listMaintenanceProfiles("claude") }
	cfg, _ := store.LoadConfig()
	cfg.ClaudeAutoRefresh = true
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	m.triggerClaudeAutoRefresh(context.Background())
	waitForCalls(t, exec, 1) // only the inactive profile
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := os.ReadFile(idlePath); string(got) == rotated {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, _ := os.ReadFile(idlePath); string(got) != rotated {
		t.Fatalf("inactive profile was not refreshed: %s", got)
	}
	if got, _ := os.ReadFile(activePath); string(got) != old {
		t.Fatalf("active profile was touched: %s", got)
	}
}

func TestManagerCodexMaintenanceSkipsActiveProfile(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	now := time.Now().UTC()
	activePath := filepath.Join(t.TempDir(), "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(activePath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.LoadConfig()
	cfg.ClaudeAutoRefresh = true
	cfg.WarmupMode = WarmupModeForce
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cache := EmptyCache()
	past := now.Add(-time.Hour)
	cache.Snapshots[ProviderCodex] = []CacheEntry{{Profile: Profile{ProfileName: "live", Snapshot: Snapshot{Status: StatusOK, Windows: []Window{sessionWindow(&past)}}}}}
	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, EmptyRegistry())
	m.SetClock(&fakeClock{now: now})
	m.claudeAvailableFn = func() bool { return true }
	m.SetProfileLister(func(tool string) ([]ProfileInfo, error) {
		if tool != "codex" {
			return nil, nil
		}
		// Expired AND out of window: both refresh and warmup would pick it if
		// it were inactive.
		return []ProfileInfo{{Tool: "codex", Name: "live", Active: true, CredPath: activePath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Hour))}}, nil
	})
	var ran atomic.Bool
	exec := &fakeCodexExecutor{write: codexAuth("new", "r2", now.Format(time.RFC3339)), inspect: func(string) error { ran.Store(true); return nil }}
	m.codexMaintainer = testCodexMaintainer(exec, now)

	m.triggerCodexMaintenance(context.Background())
	time.Sleep(100 * time.Millisecond)
	if ran.Load() {
		t.Fatal("codex maintenance ran for the active profile")
	}
	if codexWarmupWasAttempted(m.codexMaintainer, "live") {
		t.Fatal("codex warmup was attempted for the active profile")
	}
	if got, _ := os.ReadFile(activePath); string(got) != old {
		t.Fatalf("active codex credentials were touched: %s", got)
	}
}

func TestEnableClaudeAutoRefreshSharesOneGuard(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	m.EnableClaudeAutoRefresh(nil, t.TempDir())
	g := m.claudeRefresher.guard
	if g == nil || m.claudeWarmer.guard != g || m.codexMaintainer.guard != g {
		t.Fatal("refresher, warmer and codex maintainer must share one guard")
	}
	if !errors.Is(maintenanceTargetBlocked(ProfileInfo{Active: true}), errMaintenanceActiveProfile) {
		t.Fatal("an active profile must be blocked")
	}
}

// The account lock is keyed by the stored refresh token, and a run's copy-back
// replaces that token. A switch waiting on the old key must not return when the
// old lock frees if the token changed meanwhile and another job already holds
// the new key: it would activate the profile in the middle of that job.
func TestHoldMaintenanceRekeysWhenTokenRotatedWhileWaiting(t *testing.T) {
	root := t.TempDir()
	cred := filepath.Join(t.TempDir(), "auth.json")
	write := func(refresh string) {
		if err := os.WriteFile(cred, []byte(`{"tokens":{"access_token":"x","refresh_token":"`+refresh+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("old-refresh")
	profile := ProfileInfo{Tool: "codex", Name: "work", CredPath: cred}
	runA, runB := newMaintenanceGuard(), newMaintenanceGuard()
	runA.root, runB.root = root, root
	releaseA, ok := runA.acquire("codex", profile)
	if !ok {
		t.Fatal("run A could not acquire")
	}

	type result struct {
		release func()
		err     error
	}
	held := make(chan result, 1)
	go func() {
		release, err := HoldMaintenance(root, "codex", "work", cred, 10*time.Second)
		held <- result{release, err}
	}()
	time.Sleep(250 * time.Millisecond) // the switch is now waiting on the old key

	// Run A finishes: its copy-back stores the rotated token, then a second job
	// claims the NEW key, and only then does A release the old one.
	write("new-refresh")
	releaseB, ok := runB.acquire("codex", profile)
	if !ok {
		t.Fatal("job B could not claim the rotated account")
	}
	releaseA()

	select {
	case r := <-held:
		t.Fatalf("the switch took the account while job B held it (err=%v)", r.err)
	case <-time.After(400 * time.Millisecond):
	}
	releaseB()
	select {
	case r := <-held:
		if r.err != nil {
			t.Fatalf("hold after job B finished = %v", r.err)
		}
		if _, ok := runB.acquire("codex", profile); ok {
			t.Fatal("a job started while the switch held the rotated account")
		}
		r.release()
	case <-time.After(5 * time.Second):
		t.Fatal("the switch never got the account after job B finished")
	}
}

// Failing to take the lock must refuse, not degrade to "no locking": a switch
// could otherwise go live in the middle of a rotation.
func TestMaintenanceLockFailsClosed(t *testing.T) {
	root := t.TempDir()
	// A regular file where the lock directory must be makes it unusable.
	if err := os.WriteFile(filepath.Join(root, maintenanceDirName), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(cred, []byte(`{"tokens":{"access_token":"x","refresh_token":"r1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if release, err := HoldMaintenance(root, "codex", "work", cred, time.Second); err == nil || errors.Is(err, ErrMaintenanceBusy) || release != nil {
		t.Fatalf("HoldMaintenance with an unusable lock dir = (%v, %v), want a hard error", release != nil, err)
	}
	g := newMaintenanceGuard()
	g.root = root
	if _, ok := g.acquire("codex", ProfileInfo{Tool: "codex", Name: "work", CredPath: cred}); ok {
		t.Fatal("a job started without the cross-process lock")
	}
}

func TestMaintenanceGraceAfterSwitchAway(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	away := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	base := ProfileInfo{Tool: "claude", HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}

	recent := base
	recent.SwitchedAwayAt = away(2 * time.Hour)
	if claudeProfileNeedsRefresh(recent, now, 10*time.Minute) {
		t.Fatal("refresh targeted a profile switched away from 2h ago")
	}
	if claudeProfileEligibleForWarmup(recent, now) {
		t.Fatal("warmup targeted a profile switched away from 2h ago")
	}
	old := base
	old.SwitchedAwayAt = away(maintenanceSwitchAwayGrace + time.Minute)
	if !claudeProfileNeedsRefresh(old, now, 10*time.Minute) || !claudeProfileEligibleForWarmup(old, now) {
		t.Fatal("a profile switched away from beyond the grace was not eligible")
	}
	never := base
	if !claudeProfileNeedsRefresh(never, now, 10*time.Minute) {
		t.Fatal("a profile with no recorded switch-away must stay eligible")
	}
	codexRecent := ProfileInfo{Tool: "codex", HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute)), SwitchedAwayAt: away(time.Hour)}
	if codexProfileNeedsRefresh(codexRecent, now) || codexProfileEligibleForWarmup(codexRecent, now) {
		t.Fatal("codex maintenance targeted a recently switched-away profile")
	}

	// A run that starts anyway (stale list) re-checks and does not run.
	_, credPath := writeStoredProfile(t, claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli()))
	exec := &fakeExecutor{writeNew: claudeCredsWithExpiry("new-tok", now.Add(time.Hour).UnixMilli())}
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute)), SwitchedAwayAt: away(time.Hour)}
	if _, err := newTestRefresher(exec, &fakeClock{now: now}).maybeRefreshSync(context.Background(), profile); !errors.Is(err, errMaintenanceGrace) {
		t.Fatalf("refresh = %v, want errMaintenanceGrace", err)
	}
	if _, err := newTestWarmer(exec, &fakeClock{now: now}).maybeWarmSync(context.Background(), profile); !errors.Is(err, errMaintenanceGrace) {
		t.Fatalf("warmup = %v, want errMaintenanceGrace", err)
	}
	if exec.callCount() != 0 {
		t.Fatalf("an executor ran inside the grace period (%d calls)", exec.callCount())
	}
}
