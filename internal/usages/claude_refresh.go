package usages

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
// have been removed or otherwise changed. Any of these aborts the copy-back
// rather than clobbering the new reality. (There is deliberately no
// "profile became active" sentinel: refreshing an active profile is an
// intended target now — see commitClaudeCredCopyBack.)
var (
	errClaudeRefreshProfileGone    = errors.New("claude auto-refresh: profile no longer exists")
	errClaudeRefreshProfileChanged = errors.New("claude auto-refresh: stored credentials changed mid-refresh")

	// errClaudeRefreshNoop signals a benign non-rotation: the executor ran but
	// the returned expiry did not advance. This is NOT a failure — it must not
	// incur the post-failure backoff nor an outcome=failed log line. The caller
	// defers the next attempt to the stored token's real expiry.
	errClaudeRefreshNoop = errors.New("claude auto-refresh: token still valid, no rotation needed")
)

// refreshExecutor runs whatever side-effecting step rotates the credentials in
// profileDir. The real implementation runs the claude binary inside an
// ephemeral harness-docker container; tests inject a fake. It is deliberately
// generic (a directory, not a claude-specific type) so a later warmup task can
// reuse the same abstraction.
type refreshExecutor interface {
	Refresh(ctx context.Context, profileDir, modelFlag string) error
}

// dockerRefreshExecutor is the production executor: it builds the image on
// demand and runs the verified `docker run ... claude -p` invocation.
type dockerRefreshExecutor struct{}

func (dockerRefreshExecutor) Refresh(ctx context.Context, profileDir, modelFlag string) error {
	if err := ensureHarnessDockerImage(ctx); err != nil {
		return err
	}
	// Verified-working invocation: mount the profile dir at /config, point
	// CLAUDE_CONFIG_DIR at it, and run any claude prompt. The image bakes its
	// own identity, so no `-e` identity flags are needed.
	args := dockerRefreshArgs(profileDir, modelFlag)
	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("harness-docker claude refresh run failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func dockerRefreshArgs(profileDir, modelFlag string) []string {
	args := []string{
		"run", "--rm",
		"-v", profileDir + ":/config",
		"-e", "CLAUDE_CONFIG_DIR=/config",
		harnessDockerImage,
		"claude", "-p", claudeRefreshPrompt,
	}
	if modelFlag != "" {
		args = append(args, strings.Fields(modelFlag)...)
	}
	return args
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

// claudeProfileNeedsRefresh is the pure eligibility predicate: any claude
// switcher profile with stored credentials whose access token is already
// expired or within window of expiry, INCLUDING the active profile. The active
// account is intentionally eligible: auto-refresh fires only when its token is
// expired or near-expiry, so an idle active account (e.g. overnight) still gets
// its token rotated instead of silently expiring. This mirrors
// claudeProfileEligibleForWarmup, which also covers the active account.
func claudeProfileNeedsRefresh(profile ProfileInfo, now time.Time, window time.Duration) bool {
	if profile.Tool != "claude" {
		return false
	}
	if !profile.HasCredentials {
		return false
	}
	if profile.ExpiresAt == nil {
		return false
	}
	return !now.Before(profile.ExpiresAt.Add(-window))
}

// claudeRefreshOutcome classifies how a single refresh attempt ended, which
// drives the rate-limiter state (failure backoff vs. benign expiry-gated defer).
type claudeRefreshOutcome int

const (
	refreshOutcomeSuccess claudeRefreshOutcome = iota
	refreshOutcomeFailure
	refreshOutcomeNoop
)

// classifyRefreshErr maps a refresh() result to an outcome and, for the benign
// no-op case, the time before which the next attempt should be suppressed (the
// token's real expiry — once past it, a re-run WILL rotate).
func classifyRefreshErr(err error, profile ProfileInfo) (claudeRefreshOutcome, time.Time) {
	switch {
	case err == nil:
		return refreshOutcomeSuccess, time.Time{}
	case errors.Is(err, errClaudeRefreshNoop):
		var retry time.Time
		if profile.ExpiresAt != nil {
			retry = *profile.ExpiresAt
		}
		return refreshOutcomeNoop, retry
	default:
		return refreshOutcomeFailure, time.Time{}
	}
}

type claudeRefreshState struct {
	inFlight    bool
	lastFailure time.Time
	// retryAfter defers the next attempt for a benign no-op (token still valid):
	// unlike lastFailure it is not a fixed backoff but the token's real expiry, so
	// the profile is not re-run on every poller tick while its token is valid.
	retryAfter time.Time
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
	if !st.retryAfter.IsZero() && now.Before(st.retryAfter) {
		return false
	}
	st.inFlight = true
	return true
}

// finish releases the in-flight slot and records the rate-limiter state for the
// outcome: a failure stamps lastFailure (fixed backoff); a benign no-op stamps
// retryAfter (defer to real expiry) without a failure; success clears both.
func (r *claudeRefresher) finish(name string, outcome claudeRefreshOutcome, retryAfter time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state[name]
	if st == nil {
		st = &claudeRefreshState{}
		r.state[name] = st
	}
	st.inFlight = false
	switch outcome {
	case refreshOutcomeSuccess:
		st.lastFailure = time.Time{}
		st.retryAfter = time.Time{}
	case refreshOutcomeNoop:
		// Benign: don't stamp a failure; defer the next run until the token is at
		// real expiry so we don't re-run the container every poller tick.
		st.lastFailure = time.Time{}
		st.retryAfter = retryAfter
	case refreshOutcomeFailure:
		st.lastFailure = r.clock.Now()
	}
}

// maybeRefresh triggers an asynchronous refresh for an eligible profile. It is
// a no-op when the feature is unavailable, a refresh is already in flight, or
// the profile is in failure backoff. Callers must have already checked that the
// config setting is enabled and that the profile is eligible.
func (r *claudeRefresher) maybeRefresh(ctx context.Context, profile ProfileInfo, modelFlag string) {
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
		// outcome defaults to failure so a panic in refresh (exec/file/JSON work)
		// still records a failure and, crucially, always releases the in-flight
		// guard via the deferred finish. recover() keeps one profile's panic from
		// crashing the whole server on this background goroutine.
		outcome := refreshOutcomeFailure
		var retryAfter time.Time
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("usages: claude auto-refresh profile=%q outcome=failed reason=panic: %v", profile.Name, recovered)
			}
			r.finish(profile.Name, outcome, retryAfter)
		}()
		err := r.refresh(ctx, profile, modelFlag)
		outcome, retryAfter = classifyRefreshErr(err, profile)
		switch {
		case err == nil:
			// success is logged inside refresh (outcome=rotated)
		case errors.Is(err, errClaudeRefreshNoop):
			// benign no-op is logged inside refresh (outcome=noop)
		case errors.Is(err, errClaudeRefreshProfileGone):
			log.Printf("usages: claude auto-refresh profile=%q outcome=aborted reason=profile removed", profile.Name)
		case errors.Is(err, errClaudeRefreshProfileChanged):
			log.Printf("usages: claude auto-refresh profile=%q outcome=aborted reason=credentials changed", profile.Name)
		default:
			log.Printf("usages: claude auto-refresh profile=%q outcome=failed reason=%v", profile.Name, err)
		}
	}()
}

// maybeRefreshSync is the synchronous form used by tests: it applies the
// in-flight/backoff guard, runs the refresh, and records the outcome. The bool
// reports whether the refresh was attempted (false = skipped by the guard).
func (r *claudeRefresher) maybeRefreshSync(ctx context.Context, profile ProfileInfo, modelFlags ...string) (bool, error) {
	if !r.tryStart(profile.Name) {
		return false, nil
	}
	var err error
	// Deferred so the in-flight guard is released even if refresh panics (the
	// panic still propagates here — this is the synchronous test path).
	defer func() {
		outcome, retryAfter := classifyRefreshErr(err, profile)
		r.finish(profile.Name, outcome, retryAfter)
	}()
	modelFlag := ""
	if len(modelFlags) > 0 {
		modelFlag = modelFlags[0]
	}
	err = r.refresh(ctx, profile, modelFlag)
	return true, err
}

// refresh performs the actual work: seed a temp dir with only the profile's
// .credentials.json, run the executor against the temp dir, verify the rotated
// file parses and its expiresAt advanced, then copy the rotated .credentials.json
// back into the real profile dir under the switcher lock with a CAS guard.
func (r *claudeRefresher) refresh(ctx context.Context, profile ProfileInfo, modelFlag string) error {
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
	if err := forceExpireClaudeTempCredentials(tmpCred); err != nil {
		return fmt.Errorf("claude auto-refresh: force-expire temp credentials: %w", err)
	}

	if r.reachable != nil && !r.reachable(ctx) {
		return errors.New("claude auto-refresh: docker is not reachable")
	}
	if err := r.executor.Refresh(ctx, tmpDir, modelFlag); err != nil {
		return fmt.Errorf("claude auto-refresh: executor: %w", err)
	}

	// Success is defined by the rotated file, not the docker exit code: it must
	// parse and its expiresAt must have advanced beyond the old value.
	newExpiry, err := ValidateClaudeCredentials(tmpCred)
	if err != nil {
		return fmt.Errorf("claude auto-refresh: rotated credentials invalid: %w", err)
	}
	if !newExpiry.After(*oldExpiry) {
		// Safety net: the executor ran after the temp copy was force-expired, but
		// the resulting expiry still did not advance. This is benign, not a
		// failure: signal errClaudeRefreshNoop so the caller defers the next attempt
		// to the stored token's real expiry rather than stamping a 5-min failure
		// backoff and re-running the container every poller tick.
		log.Printf("usages: claude auto-refresh profile=%q outcome=noop reason=token-still-valid expiry=%s", profile.Name, oldExpiry.UTC().Format(time.RFC3339))
		return errClaudeRefreshNoop
	}
	rotated, err := os.ReadFile(tmpCred)
	if err != nil {
		return fmt.Errorf("claude auto-refresh: read rotated credentials: %w", err)
	}

	// Copy-back under the switcher lock with a compare-and-swap identity guard:
	// a removal or credential change that landed mid-refresh must never be
	// clobbered. Refreshing an active profile is intentional now (an idle active
	// account whose token expired): auto-refresh only fires at/near expiry, so no
	// live session depends on the pre-rotation token at that moment, and the
	// file-based switcher reconciles ~/.claude on its next switch/capture.
	if err := commitClaudeCredCopyBack(r.withLock, r.reresolve, profile.Name, storedPath, startFingerprint, rotated); err != nil {
		return err
	}
	log.Printf("usages: claude auto-refresh profile=%q outcome=rotated expiry=%s->%s", profile.Name, oldExpiry.UTC().Format(time.RFC3339), newExpiry.UTC().Format(time.RFC3339))
	return nil
}

// forceExpireClaudeTempCredentials makes only the throwaway credential copy
// look expired so claude performs a real pre-emptive rotation. The stored
// profile remains byte-for-byte untouched until the rotated temp file passes
// validation and the copy-back CAS guard. Raw JSON maps preserve credential
// fields that aimebu does not interpret.
func forceExpireClaudeTempCredentials(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse credentials: %w", err)
	}
	updated := false
	for sectionName, expiryName := range map[string]string{
		"claudeAiOauth":   "expiresAt",
		"claude_ai_oauth": "expires_at",
	} {
		raw, ok := root[sectionName]
		if !ok {
			continue
		}
		var tokens map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return fmt.Errorf("parse %s: %w", sectionName, err)
		}
		if _, ok := tokens[expiryName]; !ok {
			continue
		}
		tokens[expiryName] = json.RawMessage("1")
		encoded, err := json.Marshal(tokens)
		if err != nil {
			return fmt.Errorf("encode %s: %w", sectionName, err)
		}
		root[sectionName] = encoded
		updated = true
	}
	if !updated {
		return errors.New("OAuth expiresAt field missing")
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	return atomicWriteBytes(path, encoded, 0o600)
}

// commitClaudeCredCopyBack copies `rotated` back into storedPath under withLock
// with a compare-and-swap identity guard: the slow container run happened
// outside the lock, so before writing it re-checks that the profile still
// exists, its CredPath is unchanged, and the stored file still byte-matches
// startFingerprint. It deliberately does NOT abort when the profile is active:
// both auto-refresh (near/at expiry) and warmup (out-of-window idle) intend to
// rotate the active account's stored credentials, and the file-based switcher
// reconciles the live config on its next switch/capture. Any other mismatch
// aborts rather than clobbering new reality. withLock may be nil (tests) to
// skip locking.
func commitClaudeCredCopyBack(withLock func(func() error) error, reresolve func(string) (ProfileInfo, bool), name, storedPath string, startFingerprint [32]byte, rotated []byte) error {
	commit := func() error {
		if reresolve != nil {
			cur, ok := reresolve(name)
			if !ok {
				return errClaudeRefreshProfileGone
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
