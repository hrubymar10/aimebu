package usages

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// claudeWarmupCooldown is the minimum gap between warmup attempts for one
// account, regardless of outcome. Warmup starts a rolling 5-hour session window
// server-side (at Anthropic); whether it took effect is only observable on the
// NEXT usages snapshot, not from the container exit code. So we cannot classify
// an attempt as success/failure inline the way auto-refresh can (which checks
// that expiresAt advanced). Instead every attempt stamps a cooldown: if it
// worked, the account shows in-window next snapshot and drops out of the
// eligible set anyway; if it did not, the 1h cooldown stops us hammering it on
// every button press / tick. This is deliberately distinct from auto-refresh's
// 5-minute post-failure backoff.
const claudeWarmupCooldown = 1 * time.Hour

// claudeWarmupTimeout bounds a single warmup run (image build on first use +
// container start + one `claude -p` model call). Warmup runs on the long-lived
// poller context (it would otherwise only end at server shutdown), so this
// timeout is what actually stops a wedged run.
const claudeWarmupTimeout = 5 * time.Minute

// claudeProfileEligibleForWarmup is the switcher-side eligibility gate for
// warmup: any claude switcher profile that has stored credentials, including
// the active one. Whether the account is actually idle and out of its 5h window
// is a separate check against the usages snapshot (claudeSnapshotOutOfWindow).
func claudeProfileEligibleForWarmup(profile ProfileInfo) bool {
	if profile.Tool != "claude" {
		return false
	}
	if !profile.HasCredentials {
		return false
	}
	return true
}

// claudeSnapshotOutOfWindow reports whether a claude usage snapshot shows NO
// active rolling 5-hour session window — i.e. the account is idle and its 5h
// capacity is not currently counting down. Claude's 5h window only STARTS on
// first use and then carries a future resets_at; once idle past the reset there
// is either no session window in the usage payload or its resets_at is in the
// past.
//
// Chosen semantic (the union reading): "out of window" ==
//   - the snapshot has no session (five_hour) window at all, OR
//   - it has one whose reset time is nil or already in the past.
//
// We deliberately do NOT treat 0% utilization alone as out-of-window: a
// freshly-started window sits near 0% but IS active (it has a future
// resets_at), and warming it would waste a prompt call. Requiring StatusOK
// avoids acting on an errored/auth-missing snapshot whose windows we cannot
// trust.
//
// FLAG (needs live-snapshot confirmation): the exact shape the Anthropic usage
// API returns for a long-idle account — an omitted five_hour block vs. a
// present-but-stale resets_at — has not been verified against real accounts.
// The union above is the safe superset of the plausible shapes, but confirm it
// against a running server before relying on the precise branch taken.
func claudeSnapshotOutOfWindow(snap Snapshot, now time.Time) bool {
	if snap.Status != StatusOK {
		return false
	}
	for _, w := range snap.Windows {
		if w.Key != "session" {
			continue
		}
		// A session window is present: it is active only while its reset time is
		// still in the future. nil or past reset ⇒ the window has lapsed.
		return w.ResetAt == nil || !w.ResetAt.After(now)
	}
	// No session window in an otherwise-OK snapshot ⇒ the account has not opened
	// a 5h window ⇒ out of window.
	return true
}

type claudeWarmupState struct {
	inFlight    bool
	lastAttempt time.Time
}

// claudeWarmer keeps idle claude accounts' rolling 5-hour session window
// warm by running a trivial `claude -p` as that account inside the same
// ephemeral harness-docker container that auto-refresh uses (the shared
// refreshExecutor). It differs from claudeRefresher only in eligibility (out of
// 5h window, not near token expiry) and rate-limiting (a flat 1h cooldown per
// account, not a post-failure backoff).
//
// Credential copy-back is NOT optional: running `claude -p` performs OAuth
// refresh-on-use, and for an idle/aged account (warmup's exact target set) the
// access token is typically expired, so Anthropic rotates the refresh token and
// writes it into the container's credential copy. Discarding that copy would
// leave the stored profile holding the OLD, now-invalidated refresh token — a
// dead, unrecoverable login. So warmup persists any rotated credentials back via
// the same CAS copy-back auto-refresh uses, and therefore carries the same
// withLock + reresolve deps.
type claudeWarmer struct {
	executor  refreshExecutor
	withLock  func(func() error) error              // switcher WithLock; nil = no locking (tests)
	reresolve func(name string) (ProfileInfo, bool) // current profile by name, for the CAS guard
	available func() bool
	reachable func(context.Context) bool
	clock     Clock
	cooldown  time.Duration

	mu    sync.Mutex
	state map[string]*claudeWarmupState
}

func newClaudeWarmer(withLock func(func() error) error, reresolve func(string) (ProfileInfo, bool)) *claudeWarmer {
	return &claudeWarmer{
		executor:  dockerRefreshExecutor{},
		withLock:  withLock,
		reresolve: reresolve,
		available: harnessDockerCtrlAvailable,
		reachable: dockerReachable,
		clock:     realClock{},
		cooldown:  claudeWarmupCooldown,
		state:     map[string]*claudeWarmupState{},
	}
}

// tryStart claims the in-flight slot for an account. It returns false when a
// warmup is already running for that account or the account is still inside its
// 1h cooldown. Claiming stamps lastAttempt = now so the cooldown starts from the
// moment the attempt begins (not when it ends).
func (w *claudeWarmer) tryStart(name string) bool {
	now := w.clock.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state[name]
	if st == nil {
		st = &claudeWarmupState{}
		w.state[name] = st
	}
	if st.inFlight {
		return false
	}
	if !st.lastAttempt.IsZero() && now.Sub(st.lastAttempt) < w.cooldown {
		return false
	}
	st.inFlight = true
	st.lastAttempt = now
	return true
}

// finish releases the in-flight slot. The cooldown stamp was set in tryStart, so
// finish only clears the concurrency guard.
func (w *claudeWarmer) finish(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st := w.state[name]; st != nil {
		st.inFlight = false
	}
}

// warm runs the executor against a throwaway copy of the profile's credentials
// (so the claude binary pollutes the temp dir, not the real profile), then
// persists any rotated credentials back. Running the prompt starts the account's
// rolling 5h window at Anthropic AND, when the token was expired, rotates it.
// Unlike auto-refresh, warmup does NOT require the expiry to advance and treats
// "no rotation" as success (the window still started); it only copies back when
// the credentials actually changed, guarded by the same CAS identity check so a
// switch/removal that landed mid-run is never clobbered. Active profiles are
// safe targets here because the caller only warms an out-of-window (idle)
// account, so no live session depends on the pre-rotation token; the file-based
// switcher reconciles the live config on its next switch/capture.
func (w *claudeWarmer) warm(ctx context.Context, profile ProfileInfo) error {
	storedPath := profile.CredPath
	if storedPath == "" {
		return errors.New("claude warmup: profile has no credential path")
	}
	startData, err := os.ReadFile(storedPath)
	if err != nil {
		return fmt.Errorf("claude warmup: read stored credentials: %w", err)
	}
	if _, err := ValidateClaudeCredentials(storedPath); err != nil {
		return fmt.Errorf("claude warmup: stored credentials invalid: %w", err)
	}
	startFingerprint := sha256.Sum256(startData)

	tmpDir, tmpCred, err := writeClaudeCredTemp(startData, "aimebu-claude-warmup-*")
	if err != nil {
		return fmt.Errorf("claude warmup: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Bound the run: warm() executes on the long-lived poller context, so this
	// timeout is the actual stop for a wedged run.
	runCtx, cancel := context.WithTimeout(ctx, claudeWarmupTimeout)
	defer cancel()

	if w.reachable != nil && !w.reachable(runCtx) {
		return errors.New("claude warmup: docker is not reachable")
	}
	if err := w.executor.Refresh(runCtx, tmpDir); err != nil {
		return fmt.Errorf("claude warmup: executor: %w", err)
	}

	// Copy back only if the container rotated the credentials. An unchanged file
	// means no rotation happened — still a success (the 5h window started).
	rotated, err := os.ReadFile(tmpCred)
	if err != nil {
		return fmt.Errorf("claude warmup: read post-run credentials: %w", err)
	}
	if sha256.Sum256(rotated) == startFingerprint {
		return nil
	}
	// Never write a corrupt credentials file over a working one.
	if _, err := ValidateClaudeCredentials(tmpCred); err != nil {
		return fmt.Errorf("claude warmup: rotated credentials invalid: %w", err)
	}
	if err := commitClaudeCredCopyBack(w.withLock, w.reresolve, profile.Name, storedPath, startFingerprint, rotated, true); err != nil {
		return fmt.Errorf("claude warmup: copy-back: %w", err)
	}
	log.Printf("usages: claude warmup profile=%q outcome=credentials-rotated copy_back=success", profile.Name)
	return nil
}

// maybeWarm triggers an asynchronous warmup for an eligible account. It is a
// no-op (returning false) when the feature is unavailable, a warmup is already
// in flight, or the account is inside its cooldown. Callers must have already
// checked the setting is enabled and the account is out of window. It returns
// true when a warmup goroutine was actually started.
func (w *claudeWarmer) maybeWarm(ctx context.Context, profile ProfileInfo, mode string) bool {
	if w == nil || w.executor == nil {
		return false
	}
	if w.available != nil && !w.available() {
		return false
	}
	if !w.tryStart(profile.Name) {
		return false
	}
	log.Printf("usages: claude warmup profile=%q mode=%s outcome=starting", profile.Name, mode)
	go func() {
		// recover() keeps one account's panic (exec/file work) from crashing the
		// whole server on this background goroutine, and the deferred finish always
		// releases the in-flight guard.
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("usages: claude warmup profile=%q mode=%s outcome=failed reason=panic: %v", profile.Name, mode, recovered)
			}
			w.finish(profile.Name)
		}()
		if err := w.warm(ctx, profile); err != nil {
			log.Printf("usages: claude warmup profile=%q mode=%s outcome=failed reason=%v", profile.Name, mode, err)
		}
	}()
	return true
}

// maybeWarmSync is the synchronous form used by tests: it applies the
// in-flight/cooldown guard, runs the warmup, and releases the guard. The bool
// reports whether the warmup was attempted (false = skipped by the guard).
func (w *claudeWarmer) maybeWarmSync(ctx context.Context, profile ProfileInfo) (bool, error) {
	if !w.tryStart(profile.Name) {
		return false, nil
	}
	defer w.finish(profile.Name)
	return true, w.warm(ctx, profile)
}
