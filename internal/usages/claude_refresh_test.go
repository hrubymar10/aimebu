package usages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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
	mu         sync.Mutex
	calls      int
	modelFlags []string
	err        error
	writeNew   string // if non-empty, written to <dir>/.credentials.json on each call
	inspect    func(profileDir string) error
	entered    chan struct{}
	release    chan struct{}
}

func (f *fakeExecutor) Refresh(ctx context.Context, profileDir, modelFlag string) error {
	f.mu.Lock()
	f.calls++
	f.modelFlags = append(f.modelFlags, modelFlag)
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}
	if f.inspect != nil {
		if err := f.inspect(profileDir); err != nil {
			return err
		}
	}
	if f.writeNew != "" {
		if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(f.writeNew), 0o600); err != nil {
			return err
		}
	}
	return f.err
}

func (f *fakeExecutor) lastModelFlag() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.modelFlags) == 0 {
		return ""
	}
	return f.modelFlags[len(f.modelFlags)-1]
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

func TestClaudeRefreshCopiesBackRotationDespiteExecutorError(t *testing.T) {
	now := time.Unix(2_100_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	_, credPath := writeStoredProfile(t, old)
	r := newTestRefresher(&fakeExecutor{writeNew: rotated, err: errors.New("model turn rate limited")}, &fakeClock{now: now})
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}

	attempted, err := r.maybeRefreshSync(context.Background(), profile)
	if !attempted || err != nil {
		t.Fatalf("refresh = attempted %v, err %v; want successful rotated copy-back", attempted, err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != rotated {
		t.Fatalf("stored credentials not copied back:\n got=%s\nwant=%s", got, rotated)
	}
}

func TestClaudeRefreshReturnsExecutorErrorWithoutRotation(t *testing.T) {
	now := time.Unix(2_200_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	_, credPath := writeStoredProfile(t, old)
	executorErr := errors.New("model turn rate limited")
	r := newTestRefresher(&fakeExecutor{writeNew: old, err: executorErr}, &fakeClock{now: now})
	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}

	_, err := r.maybeRefreshSync(context.Background(), profile)
	if !errors.Is(err, executorErr) {
		t.Fatalf("refresh error = %v, want original executor error", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != old {
		t.Fatalf("stored credentials changed without rotation: %s", got)
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
	r.listProfiles = func() ([]ProfileInfo, error) {
		return []ProfileInfo{{Tool: "claude", Name: "work", Active: true, CredPath: credPath, HasCredentials: true}}, nil
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

// The real switcher detaches an active profile's symlink into a snapshot while
// the container is running. The rotated token must follow that profile.
func TestClaudeRefreshRotationSurvivesSwitchAway(t *testing.T) {
	now := time.Unix(4_100_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	root := t.TempDir()
	home := filepath.Join(root, "home")
	live := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	switchRoot := filepath.Join(root, "switcher")
	runSwitcherDriver(t, "setup", switchRoot, home, "claude")
	stored := filepath.Join(switchRoot, "profiles", "claude", "main", ".credentials.json")

	executor := &fakeExecutor{writeNew: rotated, inspect: func(string) error {
		runSwitcherDriver(t, "switch", switchRoot, home, "claude")
		return nil
	}}
	r := newTestRefresher(executor, &fakeClock{now: now})
	r.listProfiles = func() ([]ProfileInfo, error) {
		_, err := os.Stat(stored)
		return []ProfileInfo{{Tool: "claude", Name: "main", CredPath: stored, HasCredentials: err == nil}}, nil
	}
	p := ProfileInfo{Tool: "claude", Name: "main", Active: true, CredPath: live, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
	_, err := r.maybeRefreshSync(context.Background(), p)
	if err != nil {
		t.Fatalf("refresh after switch: %v", err)
	}
	got, err := os.ReadFile(stored)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != rotated {
		t.Fatalf("stored credentials = %s, want rotated %s", got, rotated)
	}
}

func TestClaudeRefreshRotationSurvivesProfileRename(t *testing.T) {
	now := time.Unix(4_200_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	root := t.TempDir()
	home := filepath.Join(root, "home")
	live := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	switchRoot := filepath.Join(root, "switcher")
	runSwitcherDriver(t, "setup", switchRoot, home, "claude")
	oldStored := filepath.Join(switchRoot, "profiles", "claude", "main", ".credentials.json")
	renamedStored := filepath.Join(switchRoot, "profiles", "claude", "renamed", ".credentials.json")
	unrelated := filepath.Join(root, "unrelated.json")
	if err := os.WriteFile(unrelated, []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{writeNew: rotated, inspect: func(string) error {
		runSwitcherDriver(t, "rename", switchRoot, home, "claude")
		return nil
	}}
	r := newTestRefresher(executor, &fakeClock{now: now})
	r.listProfiles = func() ([]ProfileInfo, error) {
		return []ProfileInfo{
			{Tool: "claude", Name: "unrelated", CredPath: unrelated},
			{Tool: "claude", Name: "renamed", Active: true, CredPath: renamedStored, HasCredentials: true},
		}, nil
	}
	p := ProfileInfo{Tool: "claude", Name: "main", Active: true, CredPath: live, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
	if _, err := r.maybeRefreshSync(context.Background(), p); err != nil {
		t.Fatalf("refresh after rename: %v", err)
	}
	got, err := os.ReadFile(renamedStored)
	if err != nil || string(got) != rotated {
		t.Fatalf("renamed profile credentials = %s, %v; want %s", got, err, rotated)
	}
	if _, err := os.Lstat(oldStored); !os.IsNotExist(err) {
		t.Fatalf("old profile still exists: %v", err)
	}
}

func TestClaudeCopyBackRescuesRotationWhenProfileListingFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"r1","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"r2","expiresAt":2000}}`)
	if err := os.WriteFile(path, started, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	err := commitClaudeCredCopyBack(nil, func() ([]ProfileInfo, error) { return nil, errors.New("list failed") },
		"work", path, started, rotated, root)
	if err == nil || !strings.Contains(err.Error(), "list failed") {
		t.Fatalf("listing error = %v", err)
	}
	assertRotatedRescue(t, root, "claude", "work", string(rotated))
}

func TestClaudeCopyBackRescuesAmbiguousRename(t *testing.T) {
	root := t.TempDir()
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"shared","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"next","expiresAt":2000}}`)
	first := filepath.Join(root, "first.json")
	second := filepath.Join(root, "second.json")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, started, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list := func() ([]ProfileInfo, error) {
		return []ProfileInfo{
			{Tool: "claude", Name: "first", CredPath: first, HasCredentials: true},
			{Tool: "claude", Name: "second", CredPath: second, HasCredentials: true},
		}, nil
	}
	rescueRoot := t.TempDir()
	if err := commitClaudeCredCopyBack(nil, list, "renamed", first, started, rotated, rescueRoot); err == nil {
		t.Fatal("ambiguous rename must not pick a profile")
	}
	for _, path := range []string{first, second} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(started) {
			t.Fatalf("ambiguous profile %s changed: %s, %v", path, got, err)
		}
	}
	assertRotatedRescue(t, rescueRoot, "claude", "renamed", string(rotated))
}

func TestClaudeCopyBackDiscardsRemovedTokenAfterNameReuse(t *testing.T) {
	root := t.TempDir()
	runStarted := time.Now().Add(-time.Minute)
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"old-refresh","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"rotated","expiresAt":2000}}`)
	replacement := []byte(`{"claudeAiOauth":{"accessToken":"other","refreshToken":"other-refresh","expiresAt":3000}}`)
	path := filepath.Join(root, "work.json")
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordRemovedProfile(root, "claude", "work", started); err != nil {
		t.Fatal(err)
	}
	list := func() ([]ProfileInfo, error) {
		return []ProfileInfo{{Tool: "claude", Name: "work", CredPath: path}}, nil
	}
	err := commitClaudeCredCopyBack(nil, list, "work", path, started, rotated, root, runStarted)
	if !errors.Is(err, errClaudeRefreshProfileGone) {
		t.Fatalf("removed token copy-back = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(got, replacement) {
		t.Fatalf("reused name credentials changed: %s, %v", got, err)
	}
	assertNoRotatedRescue(t, root, "claude", "work")
}

func TestClaudeCopyBackRescuesRecentRemovalWithMalformedSibling(t *testing.T) {
	root := t.TempDir()
	runStarted := time.Now().Add(-time.Minute)
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"old-refresh","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"rotated","expiresAt":2000}}`)
	malformed := filepath.Join(root, "sibling.json")
	if err := os.WriteFile(malformed, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := func() ([]ProfileInfo, error) {
		return []ProfileInfo{{Tool: "claude", Name: "sibling", CredPath: malformed}}, nil
	}
	if err := RecordRemovedProfile(root, "claude", "work", started); err != nil {
		t.Fatal(err)
	}
	if err := commitClaudeCredCopyBack(nil, list, "work", "", started, rotated, root, runStarted); err == nil {
		t.Fatal("unreadable sibling must rescue")
	}
	assertRotatedRescue(t, root, "claude", "work", string(rotated))
}

func TestClaudeCopyBackKeepsSharedTokenProfileAfterSiblingRemoval(t *testing.T) {
	root := t.TempDir()
	runStarted := time.Now().Add(-time.Minute)
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"shared","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"next","expiresAt":2000}}`)
	workPath := filepath.Join(root, "work.json")
	if err := os.WriteFile(workPath, started, 0o600); err != nil {
		t.Fatal(err)
	}
	// tmp and work held the same token; tmp was removed during work's run.
	if err := RecordRemovedProfile(root, "claude", "tmp", started); err != nil {
		t.Fatal(err)
	}
	list := func() ([]ProfileInfo, error) {
		return []ProfileInfo{{Tool: "claude", Name: "work", CredPath: workPath}}, nil
	}
	if err := commitClaudeCredCopyBack(nil, list, "work", workPath, started, rotated, root, runStarted); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(workPath)
	if err != nil || !reflect.DeepEqual(got, rotated) {
		t.Fatalf("work credentials = %s, %v", got, err)
	}
}

func TestClaudeCopyBackRescuesOldTombstone(t *testing.T) {
	root := t.TempDir()
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"old-refresh","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"rotated","expiresAt":2000}}`)
	if err := RecordRemovedProfile(root, "claude", "work", started); err != nil {
		t.Fatal(err)
	}
	// The removal predates this maintenance run and cannot explain why its
	// current profile lookup is empty.
	runStarted := time.Now().Add(time.Minute)
	err := commitClaudeCredCopyBack(nil, func() ([]ProfileInfo, error) { return nil, nil },
		"work", "", started, rotated, root, runStarted)
	if err == nil {
		t.Fatal("old tombstone should lead to a rescue")
	}
	assertRotatedRescue(t, root, "claude", "work", string(rotated))
}

func TestClaudeCopyBackRescuesRenameAndRelogin(t *testing.T) {
	root := t.TempDir()
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"starting","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"rotated","expiresAt":2000}}`)
	newLogin := []byte(`{"claudeAiOauth":{"accessToken":"other","refreshToken":"other","expiresAt":3000}}`)
	path := filepath.Join(root, "renamed.json")
	if err := os.WriteFile(path, newLogin, 0o600); err != nil {
		t.Fatal(err)
	}
	list := func() ([]ProfileInfo, error) {
		return []ProfileInfo{{Tool: "claude", Name: "renamed", CredPath: path}}, nil
	}
	if err := commitClaudeCredCopyBack(nil, list, "original", "", started, rotated, root); err == nil {
		t.Fatal("rename and re-login must rescue")
	}
	assertRotatedRescue(t, root, "claude", "original", string(rotated))
	got, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(got, newLogin) {
		t.Fatalf("new login overwritten: %s, %v", got, err)
	}
}

func TestClaudeCopyBackCorruptTombstonesRescue(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "removed.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"starting","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"rotated","expiresAt":2000}}`)
	if err := commitClaudeCredCopyBack(nil, func() ([]ProfileInfo, error) { return nil, nil },
		"work", "", started, rotated, root); err == nil {
		t.Fatal("corrupt tombstones and missing profile must rescue")
	}
	assertRotatedRescue(t, root, "claude", "work", string(rotated))
}

func TestClaudeCopyBackRescuesMalformedSiblingWithoutRemoval(t *testing.T) {
	root := t.TempDir()
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"old-refresh","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"rotated","expiresAt":2000}}`)
	malformed := filepath.Join(root, "sibling.json")
	if err := os.WriteFile(malformed, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	list := func() ([]ProfileInfo, error) {
		return []ProfileInfo{{Tool: "claude", Name: "sibling", CredPath: malformed}}, nil
	}
	if err := commitClaudeCredCopyBack(nil, list, "work", "", started, rotated, root); err == nil {
		t.Fatal("malformed sibling must rescue")
	}
	assertRotatedRescue(t, root, "claude", "work", string(rotated))
}

func TestClaudeCopyBackPreservesUnrelatedCredentialKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"r1","expiresAt":1000},"mcpOAuth":{"server":"before"}}`)
	current := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"r1","expiresAt":1000},"mcpOAuth":{"server":"updated"}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"r2","expiresAt":2000}}`)
	if err := os.WriteFile(path, current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commitClaudeCredCopyBack(nil, nil, "work", path, started, rotated, t.TempDir()); err != nil {
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
	if string(got["mcpOAuth"]) != `{"server":"updated"}` || string(got["claudeAiOauth"]) != `{"accessToken":"new","refreshToken":"r2","expiresAt":2000}` {
		t.Fatalf("copy-back lost concurrent keys or rotated OAuth: %s", data)
	}
}

func TestClaudeCopyBackAcceptsSupportedRefreshTokenSpellings(t *testing.T) {
	for _, section := range []string{"claudeAiOauth", "claude_ai_oauth"} {
		for _, field := range []string{"refreshToken", "refresh_token"} {
			t.Run(section+"/"+field, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), ".credentials.json")
				started := []byte(fmt.Sprintf(`{"%s":{"accessToken":"old","%s":"r1","expiresAt":1000}}`, section, field))
				rotated := []byte(fmt.Sprintf(`{"%s":{"accessToken":"new","%s":"r2","expiresAt":2000}}`, section, field))
				if err := os.WriteFile(path, started, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := commitClaudeCredCopyBack(nil, nil, "work", path, started, rotated, t.TempDir()); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(rotated) {
					t.Fatalf("copy-back = %s, %v; want %s", got, err, rotated)
				}
			})
		}
	}
}

func TestClaudeCopyBackRescuesRotationWhenStartingTokenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	started := []byte(`{"claudeAiOauth":{"accessToken":"old","expiresAt":1000}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"r2","expiresAt":2000}}`)
	if err := os.WriteFile(path, started, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := commitClaudeCredCopyBack(nil, nil, "work", path, started, rotated, root); err == nil {
		t.Fatal("copy-back accepted a missing starting refresh token")
	}
	assertRotatedRescue(t, root, "claude", "work", string(rotated))
}

func TestRotatedRescueFilesPrunedToThreePerProfile(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		if err := saveAbortedRotation("claude", "work", []byte(fmt.Sprint(i)), root, errClaudeRefreshProfileChanged); err == nil {
			t.Fatal("expected copy-back abort")
		}
	}
	paths, err := filepath.Glob(filepath.Join(root, "backups", "claude", "work-rotated-*.json"))
	if err != nil || len(paths) != maxRotatedRescues {
		t.Fatalf("rotated rescue paths = %v, err=%v", paths, err)
	}
	newest, err := os.ReadFile(paths[len(paths)-1])
	if err != nil || string(newest) != "4" {
		t.Fatalf("newest rescue = %q, %v; want 4", newest, err)
	}
}

func runSwitcherDriver(t *testing.T, action, root, home, tool string) {
	t.Helper()
	cmd := exec.Command("go", "run", "./testdata/switcherdriver", action, root, home, tool)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"), "CODEX_HOME="+filepath.Join(home, ".codex"), "AIMEBU_CONFIG_DIR="+filepath.Dir(root))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("switcher %s: %v: %s", action, err, output)
	}
}

func TestClaudeRefreshCASAbortsWhenStoredFileChanged(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
	rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
	changed := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"switched-in-tok","refreshToken":"new-login","expiresAt":%d}}`, now.Add(9*time.Hour).UnixMilli())

	_, credPath := writeStoredProfile(t, old)
	// The executor mutates the real stored file mid-refresh (simulating a switch
	// capturing into it) before returning the rotated temp file.
	exec := &fakeExecutor{}
	exec.err = nil
	r := newTestRefresher(&mutatingExecutor{inner: exec, storedPath: credPath, mutateTo: changed, writeNew: rotated}, &fakeClock{now: now})
	r.rescueRoot = t.TempDir()

	profile := ProfileInfo{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
	_, err := r.maybeRefreshSync(context.Background(), profile)
	if !errors.Is(err, errClaudeRefreshProfileChanged) {
		t.Fatalf("expected changed-file CAS abort, got %v", err)
	}
	got, _ := os.ReadFile(credPath)
	if string(got) != changed {
		t.Fatalf("stored credentials must retain the mid-refresh change, got=%s", got)
	}
	assertRotatedRescue(t, r.rescueRoot, "claude", "work", rotated)
}

func assertRotatedRescue(t *testing.T, root, tool, profile, want string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "backups", tool, profile+"-rotated-*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("rotated backup paths = %v, err=%v", paths, err)
	}
	info, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o", info.Mode().Perm())
	}
	got, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("rotated backup = %s, want %s", got, want)
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

func (m *mutatingExecutor) Refresh(ctx context.Context, profileDir, modelFlag string) error {
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
	r.rescueRoot = t.TempDir()
	// Re-resolve says the profile is gone.
	r.listProfiles = func() ([]ProfileInfo, error) { return nil, nil }
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
	assertRotatedRescue(t, r.rescueRoot, "claude", "work", rotated)
	_ = dir
}

func assertNoRotatedRescue(t *testing.T, root, tool, profile string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "backups", tool, profile+"-rotated-*.json"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("removed profile left rotated rescues: %v, err=%v", paths, err)
	}
}

type removingExecutor struct {
	storedPath string
	writeNew   string
}

func (e *removingExecutor) Refresh(ctx context.Context, profileDir, modelFlag string) error {
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
	m.SetProfileLister(func(tool string) ([]ProfileInfo, error) {
		if tool != "claude" {
			return nil, nil
		}
		return []ProfileInfo{{Tool: "claude", Name: "work", CredPath: credPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}}, nil
	})
	exec := &fakeExecutor{writeNew: rotated}
	m.claudeRefresher = newTestRefresher(exec, &fakeClock{now: now})
	m.claudeRefresher.listProfiles = func() ([]ProfileInfo, error) { return m.listMaintenanceProfiles("claude") }

	// Setting disabled by default → no refresh.
	m.triggerClaudeAutoRefresh(context.Background())
	waitForCalls(t, exec, 0)

	// Enable the setting → refresh fires for the eligible inactive profile.
	cfg, _ := store.LoadConfig()
	cfg.ClaudeAutoRefresh = true
	cfg.ClaudeModelFlag = "--model claude-sonnet-4-6"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m.triggerClaudeAutoRefresh(context.Background())
	waitForCalls(t, exec, 1)
	if got := exec.lastModelFlag(); got != "--model claude-sonnet-4-6" {
		t.Fatalf("refresh model flag = %q, want configured flag", got)
	}
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
	if _, err := m.UpdateSettings(context.Background(), 0, "", nil, false, &bTrue, nil, nil, nil, nil); err != nil {
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

func TestDockerRefreshArgsAppendModelFlagTokens(t *testing.T) {
	args := dockerRefreshArgs("/tmp/profile", "--model claude-sonnet-4-6")
	wantTail := []string{"claude", "-p", claudeRefreshPrompt, "--model", "claude-sonnet-4-6"}
	if got := args[len(args)-len(wantTail):]; !reflect.DeepEqual(got, wantTail) {
		t.Fatalf("docker args tail = %#v, want %#v", got, wantTail)
	}
	without := dockerRefreshArgs("/tmp/profile", "")
	if got := without[len(without)-3:]; !reflect.DeepEqual(got, []string{"claude", "-p", claudeRefreshPrompt}) {
		t.Fatalf("docker args without model tail = %#v", got)
	}
}
