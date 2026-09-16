package usages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// claudeCredsWithExpiry returns a well-formed claude .credentials.json body with
// the given expiresAt in milliseconds.
func claudeCredsWithExpiry(accessToken string, expiresAtMs int64) string {
	return fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"refresh-tok","expiresAt":%d}}`,
		accessToken, expiresAtMs,
	)
}

func ptrTime(t time.Time) *time.Time { return &t }

// fakeExecutor writes a caller-supplied credentials body into the profile dir,
// simulating claude rotating the tokens inside the container.
type fakeExecutor struct {
	mu       sync.Mutex
	calls    int
	err      error
	writeNew string // if non-empty, written to <dir>/.credentials.json on each call
	inspect  func(profileDir string) error
	entered  chan struct{}
	release  chan struct{}
}

func (f *fakeExecutor) Refresh(ctx context.Context, profileDir string) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}
	if f.err != nil {
		return f.err
	}
	if f.inspect != nil {
		if err := f.inspect(profileDir); err != nil {
			return err
		}
	}
	if f.writeNew != "" {
		return os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(f.writeNew), 0o600)
	}
	return nil
}

func (f *fakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestRefresher builds a refresher wired to a fake executor with no docker
// gating and no locking, so tests exercise the copy-back / CAS / rate-limiter in
// isolation.
func newTestRefresher(exec refreshExecutor, clock Clock) *claudeRefresher {
	return &claudeRefresher{
		executor:  exec,
		available: func() bool { return true },
		reachable: func(context.Context) bool { return true },
		clock:     clock,
		backoff:   claudeRefreshFailBackoff,
		state:     map[string]*claudeRefreshState{},
	}
}

// ── Eligibility ──────────────────────────────────────────────────────────

func TestClaudeProfileNeedsRefresh(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	window := 10 * time.Minute
	base := ProfileInfo{Tool: "claude", HasCredentials: true, ExpiresAt: ptrTime(now.Add(time.Hour))}

	cases := []struct {
		name    string
		profile ProfileInfo
		want    bool
	}{
		{
			name:    "inactive already expired",
			profile: ProfileInfo{Tool: "claude", HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))},
			want:    true,
		},
		{
			name:    "inactive within window",
			profile: ProfileInfo{Tool: "claude", HasCredentials: true, ExpiresAt: ptrTime(now.Add(5 * time.Minute))},
			want:    true,
		},
		{
			name:    "inactive at exact window edge",
			profile: ProfileInfo{Tool: "claude", HasCredentials: true, ExpiresAt: ptrTime(now.Add(window))},
			want:    true,
		},
		{
			name:    "inactive far from expiry",
			profile: base,
			want:    false,
		},
		{
			name:    "active near/at expiry is eligible",
			profile: ProfileInfo{Tool: "claude", Active: true, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))},
			want:    true,
		},
		{
			name:    "active far from expiry not eligible",
			profile: ProfileInfo{Tool: "claude", Active: true, HasCredentials: true, ExpiresAt: ptrTime(now.Add(time.Hour))},
			want:    false,
		},
		{
			name:    "no credentials",
			profile: ProfileInfo{Tool: "claude", HasCredentials: false, ExpiresAt: ptrTime(now.Add(-time.Minute))},
			want:    false,
		},
		{
			name:    "nil expiry",
			profile: ProfileInfo{Tool: "claude", HasCredentials: true},
			want:    false,
		},
		{
			name:    "non-claude tool",
			profile: ProfileInfo{Tool: "codex", HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeProfileNeedsRefresh(tc.profile, now, window); got != tc.want {
				t.Fatalf("claudeProfileNeedsRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── Copy-back success ────────────────────────────────────────────────────

func writeStoredProfile(t *testing.T, body string) (dir, credPath string) {
	t.Helper()
	dir = t.TempDir()
	credPath = filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, credPath
}

func TestClaudeRefreshSuccessCopiesBackWhenExpiryAdvances(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	oldExpiryMs := now.Add(-time.Minute).UnixMilli()
	newExpiryMs := now.Add(11 * time.Hour).UnixMilli()
	old := claudeCredsWithExpiry("old-tok", oldExpiryMs)
	rotated := claudeCredsWithExpiry("new-tok", newExpiryMs)

	_, credPath := writeStoredProfile(t, old)
	exec := &fakeExecutor{writeNew: rotated}
	r := newTestRefresher(exec, &fakeClock{now: now})

	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
	attempted, err := r.maybeRefreshSync(context.Background(), profile)
	if !attempted {
		t.Fatal("expected refresh to be attempted")
	}
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != rotated {
		t.Fatalf("stored credentials not copied back:\n got=%s\nwant=%s", got, rotated)
	}
}

func TestForceExpireClaudeTempCredentialsPreservesShape(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "camel", body: `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"refresh","expiresAt":1893456000000,"extra":"keep"},"top":"keep"}`},
		{name: "snake", body: `{"claude_ai_oauth":{"access_token":"tok","refresh_token":"refresh","expires_at":1893456000000,"extra":"keep"},"top":"keep"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, path := writeStoredProfile(t, tc.body)
			if err := forceExpireClaudeTempCredentials(path); err != nil {
				t.Fatal(err)
			}
			expires, err := ValidateClaudeCredentials(path)
			if err != nil {
				t.Fatal(err)
			}
			if !expires.Before(time.Unix(1, 0)) {
				t.Fatalf("forced expiry = %s, want near epoch", expires)
			}
			var got map[string]any
			data, _ := os.ReadFile(path)
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got["top"] != "keep" {
				t.Fatalf("unrelated top-level field lost: %s", data)
			}
		})
	}
}

func TestClaudeRefreshForceExpiresTempBeforeExecutor(t *testing.T) {
	now := time.Unix(2_500_000, 0)
	oldExpiry := now.Add(5 * time.Minute)
	old := claudeCredsWithExpiry("old-tok", oldExpiry.UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(8*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)

	exec := &fakeExecutor{writeNew: rotated}
	exec.inspect = func(profileDir string) error {
		tempExpiry, err := ValidateClaudeCredentials(filepath.Join(profileDir, ".credentials.json"))
		if err != nil {
			return err
		}
		if !tempExpiry.Before(now) {
			return fmt.Errorf("temp expiry = %s, want before %s", tempExpiry, now)
		}
		stored, err := os.ReadFile(credPath)
		if err != nil {
			return err
		}
		if string(stored) != old {
			return fmt.Errorf("stored credentials changed before executor: %s", stored)
		}
		return nil
	}
	r := newTestRefresher(exec, &fakeClock{now: now})
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(oldExpiry)}
	attempted, err := r.maybeRefreshSync(context.Background(), profile)
	if !attempted || err != nil {
		t.Fatalf("pre-emptive rotation: attempted=%v err=%v", attempted, err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != rotated {
		t.Fatalf("rotated credentials not copied back:\n got=%s\nwant=%s", got, rotated)
	}
}

func TestClaudeRefreshNoopWhenExpiryDidNotAdvance(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	clock := &fakeClock{now: now}
	expiry := now.Add(2 * time.Minute) // pre-emptive refresh inside the 10-min window
	oldExpiryMs := expiry.UnixMilli()
	old := claudeCredsWithExpiry("old-tok", oldExpiryMs)
	// Safety net: even though refresh force-expires the temp copy, an executor
	// that returns credentials whose expiry did not advance is still a benign
	// no-op, NOT a failure. The stored file must be left untouched.
	rotated := claudeCredsWithExpiry("new-tok", oldExpiryMs)

	_, credPath := writeStoredProfile(t, old)
	exec := &fakeExecutor{writeNew: rotated}
	r := newTestRefresher(exec, clock)

	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(expiry)}
	attempted, err := r.maybeRefreshSync(context.Background(), profile)
	if !attempted {
		t.Fatal("expected the no-op refresh to be attempted")
	}
	if !errors.Is(err, errClaudeRefreshNoop) {
		t.Fatalf("expected errClaudeRefreshNoop when expiry did not advance, got %v", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != old {
		t.Fatalf("stored credentials must be unchanged on a no-op, got=%s", got)
	}

	// A no-op must NOT incur the 5-min failure backoff, but it must also NOT be
	// retried every tick while the token is still valid. Immediately after and
	// after 4 minutes (well past the 5-min failure backoff but still before the
	// token's real expiry) the attempt is deferred by retryAfter.
	if attempted, _ := r.maybeRefreshSync(context.Background(), profile); attempted {
		t.Fatal("no-op should defer the next attempt while the token is still valid")
	}
	clock.now = now.Add(4 * time.Minute) // > failBackoff(5m)? no; but > token expiry window? still before expiry+? expiry=now+2m
	// At now+4m the token has actually expired (expiry was now+2m) → retryAfter
	// has passed → the attempt is allowed again.
	if attempted, _ := r.maybeRefreshSync(context.Background(), profile); !attempted {
		t.Fatal("after the token's real expiry the no-op deferral should lift and allow a retry")
	}
	if exec.callCount() != 2 {
		t.Fatalf("executor called %d times, want 2 (initial no-op + retry after real expiry)", exec.callCount())
	}
}

// ── CAS identity guard ───────────────────────────────────────────────────

func TestClaudeRefreshCopiesBackToActiveProfile(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())

	_, credPath := writeStoredProfile(t, old)
	exec := &fakeExecutor{writeNew: rotated}
	r := newTestRefresher(exec, &fakeClock{now: now})
	// The re-resolve reports the profile is (and stays) active: refreshing an
	// active claude profile is intended now, so copy-back must NOT abort.
	r.reresolve = func(name string) (ProfileInfo, bool) {
		return ProfileInfo{Tool: "claude", Name: name, Active: true, CredPath: credPath, HasCredentials: true}, true
	}

	profile := ProfileInfo{Tool: "claude", Name: "work", Active: true, CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
	attempted, err := r.maybeRefreshSync(context.Background(), profile)
	if !attempted {
		t.Fatal("expected refresh of the active profile to be attempted")
	}
	if err != nil {
		t.Fatalf("refresh of active profile: %v", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != rotated {
		t.Fatalf("rotated credentials must be copied back to the active profile:\n got=%s\nwant=%s", got, rotated)
	}
}

func TestClaudeRefreshCASAbortsWhenStoredFileChanged(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	changed := claudeCredsWithExpiry("switched-in-tok", now.Add(9*time.Hour).UnixMilli())

	_, credPath := writeStoredProfile(t, old)
	// The executor mutates the real stored file mid-refresh (simulating a switch
	// capturing into it) before returning the rotated temp file.
	exec := &fakeExecutor{}
	exec.err = nil
	r := newTestRefresher(&mutatingExecutor{inner: exec, storedPath: credPath, mutateTo: changed, writeNew: rotated}, &fakeClock{now: now})

	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
	_, err := r.maybeRefreshSync(context.Background(), profile)
	if !errors.Is(err, errClaudeRefreshProfileChanged) {
		t.Fatalf("expected changed-file CAS abort, got %v", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != changed {
		t.Fatalf("stored credentials must retain the mid-refresh change, got=%s", got)
	}
}

// mutatingExecutor overwrites the real stored credentials file (to simulate a
// concurrent switch) and then writes the rotated body into the temp dir.
type mutatingExecutor struct {
	inner      *fakeExecutor
	storedPath string
	mutateTo   string
	writeNew   string
}

func (m *mutatingExecutor) Refresh(ctx context.Context, profileDir string) error {
	if err := os.WriteFile(m.storedPath, []byte(m.mutateTo), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(m.writeNew), 0o600)
}

func TestClaudeRefreshCASAbortsWhenProfileRemoved(t *testing.T) {
	now := time.Unix(6_000_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())

	dir, credPath := writeStoredProfile(t, old)
	exec := &fakeExecutor{writeNew: rotated}
	r := newTestRefresher(exec, &fakeClock{now: now})
	// Re-resolve says the profile is gone.
	r.reresolve = func(string) (ProfileInfo, bool) { return ProfileInfo{}, false }
	// And the stored file is removed mid-refresh.
	r.executor = &removingExecutor{storedPath: credPath, writeNew: rotated}

	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
	_, err := r.maybeRefreshSync(context.Background(), profile)
	if !errors.Is(err, errClaudeRefreshProfileGone) {
		t.Fatalf("expected profile-gone CAS abort, got %v", err)
	}
	if _, statErr := os.Stat(credPath); !os.IsNotExist(statErr) {
		t.Fatalf("stored credentials should stay removed, stat err=%v", statErr)
	}
	_ = dir
}

type removingExecutor struct {
	storedPath string
	writeNew   string
}

func (e *removingExecutor) Refresh(ctx context.Context, profileDir string) error {
	if err := os.Remove(e.storedPath); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(e.writeNew), 0o600)
}

// ── Rate limiter ─────────────────────────────────────────────────────────

func TestClaudeRefreshBackoffAfterFailure(t *testing.T) {
	now := time.Unix(7_000_000, 0)
	clock := &fakeClock{now: now}
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	_, credPath := writeStoredProfile(t, old)
	exec := &fakeExecutor{err: errors.New("boom")}
	r := newTestRefresher(exec, clock)

	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}

	attempted, err := r.maybeRefreshSync(context.Background(), profile)
	if !attempted || err == nil {
		t.Fatalf("first attempt should run and fail: attempted=%v err=%v", attempted, err)
	}
	// Immediately retry: must be skipped by backoff.
	attempted, _ = r.maybeRefreshSync(context.Background(), profile)
	if attempted {
		t.Fatal("second attempt within backoff window should be skipped")
	}
	if exec.callCount() != 1 {
		t.Fatalf("executor called %d times, want 1 during backoff", exec.callCount())
	}
	// Advance past the backoff window: allowed again.
	clock.now = now.Add(claudeRefreshFailBackoff + time.Second)
	attempted, _ = r.maybeRefreshSync(context.Background(), profile)
	if !attempted {
		t.Fatal("attempt after backoff window should run")
	}
	if exec.callCount() != 2 {
		t.Fatalf("executor called %d times, want 2 after backoff elapsed", exec.callCount())
	}
}

func TestClaudeRefreshInFlightGuard(t *testing.T) {
	now := time.Unix(8_000_000, 0)
	clock := &fakeClock{now: now}
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)

	entered := make(chan struct{})
	release := make(chan struct{})
	exec := &fakeExecutor{writeNew: rotated, entered: entered, release: release}
	r := newTestRefresher(exec, clock)
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}

	done := make(chan error, 1)
	go func() {
		_, err := r.maybeRefreshSync(context.Background(), profile)
		done <- err
	}()
	<-entered // first refresh is now inside the executor, holding the in-flight slot

	// A concurrent attempt must be skipped by the in-flight guard.
	if attempted, _ := r.maybeRefreshSync(context.Background(), profile); attempted {
		t.Fatal("second concurrent attempt should be skipped by the in-flight guard")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first refresh failed: %v", err)
	}
	if exec.callCount() != 1 {
		t.Fatalf("executor called %d times, want 1", exec.callCount())
	}
}

// ── Manager integration ──────────────────────────────────────────────────

func TestManagerTriggerClaudeAutoRefreshGatedOnSetting(t *testing.T) {
	root := t.TempDir()
	store := NewStoreAt(root)
	now := time.Unix(9_000_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)

	m := NewManager(store, EmptyRegistry())
	m.SetClock(&fakeClock{now: now})
	m.claudeAvailableFn = func() bool { return true }
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}}
	})
	exec := &fakeExecutor{writeNew: rotated}
	m.claudeRefresher = newTestRefresher(exec, &fakeClock{now: now})
	m.claudeRefresher.reresolve = m.currentClaudeProfile

	// Setting disabled by default → no refresh.
	m.triggerClaudeAutoRefresh(context.Background())
	waitForCalls(t, exec, 0)

	// Enable the setting → refresh fires for the eligible inactive profile.
	cfg, _ := store.LoadConfig()
	cfg.ClaudeAutoRefresh = true
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m.triggerClaudeAutoRefresh(context.Background())
	waitForCalls(t, exec, 1)
}

func waitForCalls(t *testing.T, exec *fakeExecutor, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := exec.callCount()
		if got == want {
			// Give a brief grace to ensure it does not exceed.
			time.Sleep(20 * time.Millisecond)
			if exec.callCount() != want {
				t.Fatalf("executor calls exceeded %d: got %d", want, exec.callCount())
			}
			return
		}
		if got > want {
			t.Fatalf("executor calls = %d, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if want != 0 {
		t.Fatalf("timed out waiting for %d executor calls, got %d", want, exec.callCount())
	}
}

func TestManagerSettingsExposeAutoRefreshAvailability(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	m.claudeAvailableFn = func() bool { return false }
	info, err := m.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.ClaudeAutoRefreshAvailable {
		t.Fatal("expected ClaudeAutoRefreshAvailable=false when harness-docker-ctrl absent")
	}
	if info.ClaudeAutoRefresh {
		t.Fatal("expected ClaudeAutoRefresh default off")
	}
	if info.WarmupAvailable {
		t.Fatal("expected WarmupAvailable=false while auto-refresh is off")
	}

	bTrue := true
	if _, err := m.UpdateSettings(context.Background(), 0, "", nil, false, &bTrue, nil, nil); err != nil {
		t.Fatal(err)
	}
	m.claudeAvailableFn = func() bool { return true }
	info, err = m.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.ClaudeAutoRefresh {
		t.Fatal("expected ClaudeAutoRefresh=true after enabling")
	}
	if !info.ClaudeAutoRefreshAvailable {
		t.Fatal("expected ClaudeAutoRefreshAvailable=true when harness-docker-ctrl present")
	}
	if !info.WarmupAvailable {
		t.Fatal("expected WarmupAvailable=true when auto-refresh and runtime gate are on")
	}
}
