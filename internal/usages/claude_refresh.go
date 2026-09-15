package usages

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// harnessDockerImage is the ephemeral container that runs the real claude
	// binary against a mounted credential dir. Inside the container there is no
	// macOS Keychain, so claude's lazy OAuth refresh-on-use writes the rotated
	// tokens straight back into the mounted file.
	harnessDockerImage      = "harness-docker:latest"
	harnessDockerCtrlBinary = "harness-docker-ctrl"

	// claudeRefreshPrompt is a trivial no-op prompt: running claude at all is
	// enough to trigger the refresh-on-use of an expired access token; the
	// prompt text is irrelevant to the token rotation.
	claudeRefreshPrompt = `write "hi"`

	// claudeNearExpiryWindow is how far ahead of expiry an inactive profile
	// becomes eligible for a pre-emptive refresh.
	claudeNearExpiryWindow = 10 * time.Minute

	// claudeRefreshFailBackoff is the minimum gap between refresh attempts for a
	// profile after a failure, so a persistently-broken profile is not retried
	// on every 5-second poller tick.
	claudeRefreshFailBackoff = 5 * time.Minute

	dockerReachableTimeout = 5 * time.Second
)

// CAS-guard sentinels. A refresh runs its slow docker step outside the switcher
// lock; by the time it wants to write the rotated tokens back the profile may
// have been switched-to (now live-owned), removed, or otherwise changed. Any of
// these aborts the copy-back rather than clobbering the new reality.
var (
	errClaudeRefreshProfileGone    = errors.New("claude auto-refresh: profile no longer exists")
	errClaudeRefreshProfileActive  = errors.New("claude auto-refresh: profile became active mid-refresh")
	errClaudeRefreshProfileChanged = errors.New("claude auto-refresh: stored credentials changed mid-refresh")
)

// refreshExecutor runs whatever side-effecting step rotates the credentials in
// profileDir. The real implementation runs the claude binary inside an
// ephemeral harness-docker container; tests inject a fake. It is deliberately
// generic (a directory, not a claude-specific type) so a later warmup task can
// reuse the same abstraction.
type refreshExecutor interface {
	Refresh(ctx context.Context, profileDir string) error
}

// dockerRefreshExecutor is the production executor: it builds the image on
// demand and runs the verified `docker run ... claude -p` invocation.
type dockerRefreshExecutor struct{}

func (dockerRefreshExecutor) Refresh(ctx context.Context, profileDir string) error {
	if err := ensureHarnessDockerImage(ctx); err != nil {
		return err
	}
	// Verified-working invocation: mount the profile dir at /config, point
	// CLAUDE_CONFIG_DIR at it, and run any claude prompt. The image bakes its
	// own identity, so no `-e` identity flags are needed.
	args := []string{
		"run", "--rm",
		"-v", profileDir + ":/config",
		"-e", "CLAUDE_CONFIG_DIR=/config",
		harnessDockerImage,
		"claude", "-p", claudeRefreshPrompt,
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("harness-docker claude refresh run failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureHarnessDockerImage builds the image via harness-docker-ctrl when it is
// not already present locally.
func ensureHarnessDockerImage(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "image", "inspect", harnessDockerImage).Run() == nil {
		return nil
	}
	build := exec.CommandContext(ctx, harnessDockerCtrlBinary, "build-image")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("harness-docker-ctrl build-image failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// harnessDockerCtrlAvailable reports whether the harness-docker-ctrl binary is
// on PATH. This is the cheap gate used for the Settings UI "unavailable" state:
// without it the whole feature cannot run.
func harnessDockerCtrlAvailable() bool {
	_, err := exec.LookPath(harnessDockerCtrlBinary)
	return err == nil
}

// dockerReachable reports whether the docker daemon answers. Kept short so a
// down daemon does not stall a refresh attempt for long.
func dockerReachable(ctx context.Context) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, dockerReachableTimeout)
	defer cancel()
	return exec.CommandContext(checkCtx, "docker", "info").Run() == nil
}

// claudeProfileNeedsRefresh is the pure eligibility predicate: an INACTIVE
// claude switcher profile with stored credentials whose access token is already
// expired or within window of expiry. The active profile is never eligible —
// the live harness owns it.
func claudeProfileNeedsRefresh(profile ProfileInfo, now time.Time, window time.Duration) bool {
	if profile.Tool != "claude" {
		return false
	}
	if profile.Active || !profile.HasCredentials {
		return false
	}
	if profile.ExpiresAt == nil {
		return false
	}
	return !now.Before(profile.ExpiresAt.Add(-window))
}

type claudeRefreshState struct {
	inFlight    bool
	lastFailure time.Time
}

// claudeRefresher coordinates auto-refresh of near-expiry inactive claude
// profiles. It owns the rate-limiter (per-profile in-flight guard + post-failure
// backoff) and performs the copy-back under the switcher lock with a CAS
// identity guard. The slow docker run happens outside the lock.
type claudeRefresher struct {
	executor  refreshExecutor
	withLock  func(func() error) error              // switcher WithLock; nil = no locking (tests)
	reresolve func(name string) (ProfileInfo, bool) // current profile by name, for the CAS guard
	available func() bool                           // harness-docker-ctrl present
	reachable func(context.Context) bool            // docker daemon reachable
	clock     Clock
	backoff   time.Duration

	mu    sync.Mutex
	state map[string]*claudeRefreshState
}

func newClaudeRefresher(withLock func(func() error) error, reresolve func(string) (ProfileInfo, bool)) *claudeRefresher {
	return &claudeRefresher{
		executor:  dockerRefreshExecutor{},
		withLock:  withLock,
		reresolve: reresolve,
		available: harnessDockerCtrlAvailable,
		reachable: dockerReachable,
		clock:     realClock{},
		backoff:   claudeRefreshFailBackoff,
		state:     map[string]*claudeRefreshState{},
	}
}

// tryStart claims the in-flight slot for a profile. It returns false when a
// refresh is already running for that profile or the profile is still inside
// its post-failure backoff window.
func (r *claudeRefresher) tryStart(name string) bool {
	now := r.clock.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state[name]
	if st == nil {
		st = &claudeRefreshState{}
		r.state[name] = st
	}
	if st.inFlight {
		return false
	}
	if !st.lastFailure.IsZero() && now.Sub(st.lastFailure) < r.backoff {
		return false
	}
	st.inFlight = true
	return true
}

// finish releases the in-flight slot, recording (or clearing) the failure stamp
// that drives backoff.
func (r *claudeRefresher) finish(name string, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state[name]
	if st == nil {
		st = &claudeRefreshState{}
		r.state[name] = st
	}
	st.inFlight = false
	if failed {
		st.lastFailure = r.clock.Now()
	} else {
		st.lastFailure = time.Time{}
	}
}

// maybeRefresh triggers an asynchronous refresh for an eligible profile. It is
// a no-op when the feature is unavailable, a refresh is already in flight, or
// the profile is in failure backoff. Callers must have already checked that the
// config setting is enabled and that the profile is eligible.
func (r *claudeRefresher) maybeRefresh(ctx context.Context, profile ProfileInfo) {
	if r == nil || r.executor == nil {
		return
	}
	if r.available != nil && !r.available() {
		return
	}
	if !r.tryStart(profile.Name) {
		return
	}
	log.Printf("usages: claude auto-refresh profile=%q outcome=starting", profile.Name)
	go func() {
		// failed defaults to true so a panic in refresh (exec/file/JSON work)
		// still records a failure and, crucially, always releases the in-flight
		// guard via the deferred finish. recover() keeps one profile's panic from
		// crashing the whole server on this background goroutine.
		failed := true
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("usages: claude auto-refresh profile=%q outcome=failed reason=panic: %v", profile.Name, recovered)
			}
			r.finish(profile.Name, failed)
		}()
		err := r.refresh(ctx, profile)
		failed = err != nil
		if err != nil {
			switch {
			case errors.Is(err, errClaudeRefreshProfileGone):
				log.Printf("usages: claude auto-refresh profile=%q outcome=aborted reason=profile removed", profile.Name)
			case errors.Is(err, errClaudeRefreshProfileActive):
				log.Printf("usages: claude auto-refresh profile=%q outcome=aborted reason=profile switched active", profile.Name)
			case errors.Is(err, errClaudeRefreshProfileChanged):
				log.Printf("usages: claude auto-refresh profile=%q outcome=aborted reason=credentials changed", profile.Name)
			default:
				log.Printf("usages: claude auto-refresh profile=%q outcome=failed reason=%v", profile.Name, err)
			}
		}
	}()
}

// maybeRefreshSync is the synchronous form used by tests: it applies the
// in-flight/backoff guard, runs the refresh, and records the outcome. The bool
// reports whether the refresh was attempted (false = skipped by the guard).
func (r *claudeRefresher) maybeRefreshSync(ctx context.Context, profile ProfileInfo) (bool, error) {
	if !r.tryStart(profile.Name) {
		return false, nil
	}
	var err error
	// Deferred so the in-flight guard is released even if refresh panics (the
	// panic still propagates here — this is the synchronous test path).
	defer func() { r.finish(profile.Name, err != nil) }()
	err = r.refresh(ctx, profile)
	return true, err
}

// refresh performs the actual work: seed a temp dir with only the profile's
// .credentials.json, run the executor against the temp dir, verify the rotated
// file parses and its expiresAt advanced, then copy the rotated .credentials.json
// back into the real profile dir under the switcher lock with a CAS guard.
func (r *claudeRefresher) refresh(ctx context.Context, profile ProfileInfo) error {
	storedPath := profile.CredPath
	if storedPath == "" {
		return errors.New("claude auto-refresh: profile has no credential path")
	}
	startData, err := os.ReadFile(storedPath)
	if err != nil {
		return fmt.Errorf("claude auto-refresh: read stored credentials: %w", err)
	}
	oldExpiry, err := ValidateClaudeCredentials(storedPath)
	if err != nil {
		return fmt.Errorf("claude auto-refresh: stored credentials invalid: %w", err)
	}
	startFingerprint := sha256.Sum256(startData)

	// Temp-dir isolation: claude pollutes its config dir with .claude.json,
	// sessions/, projects/, policy-limits.json. Run against a throwaway copy of
	// only the credentials file so the profile dir stays clean.
	tmpDir, tmpCred, err := writeClaudeCredTemp(startData, "aimebu-claude-refresh-*")
	if err != nil {
		return fmt.Errorf("claude auto-refresh: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if r.reachable != nil && !r.reachable(ctx) {
		return errors.New("claude auto-refresh: docker is not reachable")
	}
	if err := r.executor.Refresh(ctx, tmpDir); err != nil {
		return fmt.Errorf("claude auto-refresh: executor: %w", err)
	}

	// Success is defined by the rotated file, not the docker exit code: it must
	// parse and its expiresAt must have advanced beyond the old value.
	newExpiry, err := ValidateClaudeCredentials(tmpCred)
	if err != nil {
		return fmt.Errorf("claude auto-refresh: rotated credentials invalid: %w", err)
	}
	if !newExpiry.After(*oldExpiry) {
		return fmt.Errorf("claude auto-refresh: expiry did not advance (old=%s new=%s)", oldExpiry.UTC(), newExpiry.UTC())
	}
	rotated, err := os.ReadFile(tmpCred)
	if err != nil {
		return fmt.Errorf("claude auto-refresh: read rotated credentials: %w", err)
	}

	// Copy-back under the switcher lock with a compare-and-swap identity guard,
	// mirroring codex's persistAuth: a switch or removal that landed mid-refresh
	// must never be clobbered.
	if err := commitClaudeCredCopyBack(r.withLock, r.reresolve, profile.Name, storedPath, startFingerprint, rotated, false); err != nil {
		return err
	}
	log.Printf("usages: claude auto-refresh profile=%q outcome=rotated expiry=%s->%s", profile.Name, oldExpiry.UTC().Format(time.RFC3339), newExpiry.UTC().Format(time.RFC3339))
	return nil
}

// commitClaudeCredCopyBack copies `rotated` back into storedPath under withLock
// with a compare-and-swap identity guard: the slow container run happened
// outside the lock, so before writing it re-checks that the profile still
// exists, its CredPath is unchanged, and the stored file still byte-matches
// startFingerprint. Auto-refresh additionally requires the profile to remain
// inactive; warmup passes allowActive because an out-of-window active account
// is an intentional target. Any other mismatch aborts rather than clobbering
// new reality. withLock may be nil (tests) to skip locking.
func commitClaudeCredCopyBack(withLock func(func() error) error, reresolve func(string) (ProfileInfo, bool), name, storedPath string, startFingerprint [32]byte, rotated []byte, allowActive bool) error {
	commit := func() error {
		if reresolve != nil {
			cur, ok := reresolve(name)
			if !ok {
				return errClaudeRefreshProfileGone
			}
			if cur.Active && !allowActive {
				return errClaudeRefreshProfileActive
			}
			if cur.CredPath != storedPath {
				return errClaudeRefreshProfileChanged
			}
		}
		curData, err := os.ReadFile(storedPath)
		if err != nil {
			return errClaudeRefreshProfileGone
		}
		if sha256.Sum256(curData) != startFingerprint {
			return errClaudeRefreshProfileChanged
		}
		return atomicWriteBytes(storedPath, rotated, 0o600)
	}
	if withLock != nil {
		return withLock(commit)
	}
	return commit()
}

// writeClaudeCredTemp creates a throwaway temp dir seeded with only a
// .credentials.json holding the provided bytes, so the claude binary — run by
// the executor — pollutes the temp dir instead of the real profile dir. It is
// shared by auto-refresh (which copies the rotated file back) and warmup (which
// discards it). The caller owns removing tmpDir. Returns (tmpDir, tmpCredPath).
func writeClaudeCredTemp(data []byte, prefix string) (string, string, error) {
	tmpDir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", "", fmt.Errorf("create temp dir: %w", err)
	}
	tmpCred := filepath.Join(tmpDir, ".credentials.json")
	if err := atomicWriteBytes(tmpCred, data, 0o600); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("seed temp credentials: %w", err)
	}
	return tmpDir, tmpCred, nil
}

// atomicWriteBytes writes data to path via a temp file + rename so a crash can
// never leave a half-written credentials file.
func atomicWriteBytes(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(path, mode)
}
