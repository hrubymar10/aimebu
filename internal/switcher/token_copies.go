package switcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// Token-copy model (design: __others/design-token-only-copies-server.md rev 3).
// After `aimebu switcher migrate-copies`:
//   - the profile store holds the only FULL credentials, as regular files (no
//     symlinks);
//   - the live ~/.claude and ~/.codex files are token-only targets that the
//     server rewrites;
//   - Switch therefore never captures or restores the live file: it refreshes the
//     incoming profile if it is due, rewrites the live targets from it, and moves
//     the active pointer.
//
// The model is switched on by a marker file so a link-based install keeps the
// old Switch until it is migrated.

// ErrLoginPending: the live file still holds a refresh token (a /login that has
// not been imported yet). Switching now could throw that login away; the
// reconciler imports it within a minute.
var ErrLoginPending = errors.New("a login in the live credentials is waiting to be imported; retry in a minute")

// ErrMigrationRaced: migrate-copies completed while this switch was starting.
// Nothing was changed; the switch must be retried so it takes the token-copy path
// (including the refresh of the incoming profile).
var ErrMigrationRaced = errors.New("the credential migration finished while this switch was starting; try again")

// ErrIncomingRefresh: the incoming profile is due for a refresh and the refresh
// failed, so the switch is refused rather than handing sessions a dying account.
var ErrIncomingRefresh = errors.New("cannot refresh the incoming profile")

// TokenCopyHooks connect the manager to the server-side pieces (injected, so this
// package keeps no dependency on the refresher or the reconciler).
type TokenCopyHooks struct {
	// FanOutActive rewrites the live targets of tool from a full credential
	// (credtarget.FanOut.FanOutActive). Required.
	FanOutActive func(tool string, full []byte) error
	// ReplaceActive is FanOutActive allowed to overwrite a live file that still
	// holds a refresh token. Only migrate-copies uses it, after backing that
	// credential up and writing it into the store. Optional: nil falls back to
	// FanOutActive.
	ReplaceActive func(tool string, full []byte) error
	// ClearActive removes every live target of tool (the live file and each
	// registered target that follows the active profile): a switch to an empty
	// profile must not leave the outgoing account's access token in a target.
	// Optional: nil removes the live file only.
	ClearActive func(tool string) error
	// RefreshIncoming refreshes (tool, profile) now. It is called WITH the
	// account lock held (Switch holds it) and BEFORE the switcher flock, and it
	// commits through Manager.WithLock itself. Optional: nil skips the refresh.
	RefreshIncoming func(ctx context.Context, tool, profile string) error
	// RestoreFull rewrites the live target with the stored full credential,
	// preserving unrelated live fields and refusing a pending login.
	RestoreFull func(tool string, full []byte) error
}

// SetTokenCopyHooks installs the hooks.
func (m *Manager) SetTokenCopyHooks(h TokenCopyHooks) { m.copyHooks = h }

const tokenCopiesFile = usages.TokenCopiesMarkerFile

func (m *Manager) tokenCopiesPath() string { return filepath.Join(m.root, tokenCopiesFile) }

// TokenCopies reports whether ANY tool runs the token-copy model: the server
// starts its fan-out, reconciler and refresher on it. Per-tool decisions use
// TokenCopiesFor. The marker's shape and reader live in usages (one definition
// shared with the refresher and doctor).
func (m *Manager) TokenCopies() bool {
	marker, ok := usages.ReadTokenCopiesMarker(m.root)
	return ok && marker.Any()
}

// TokenCopiesFor reports whether tool has been migrated to the token-copy model.
func (m *Manager) TokenCopiesFor(tool string) bool {
	_, ok := m.TokenCopiesSince(tool)
	return ok
}

// TokenCopiesSince is when tool was migrated; false when it was not.
func (m *Manager) TokenCopiesSince(tool string) (time.Time, bool) {
	marker, ok := usages.ReadTokenCopiesMarker(m.root)
	if !ok {
		return time.Time{}, false
	}
	return marker.Since(tool)
}

// SetTokenCopies marks both tools migrated (on) or clears the marker (off).
func (m *Manager) SetTokenCopies(on bool) error {
	if !on {
		if err := os.Remove(m.tokenCopiesPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return m.markTokenCopies(ToolClaude, ToolCodex)
}

// markTokenCopies records tools as migrated now, keeping the tools that already
// were (and their original timestamps). Called by Migrate under the switcher
// lock.
func (m *Manager) markTokenCopies(tools ...string) error {
	marker, _ := usages.ReadTokenCopiesMarker(m.root)
	now := time.Now().UTC()
	next := usages.TokenCopiesMarker{Version: 2, MigratedAt: now, Migrated: map[string]time.Time{}}
	if marker.Version == 1 {
		next.MigratedAt = marker.MigratedAt
		next.Migrated[ToolClaude] = marker.MigratedAt
		next.Migrated[ToolCodex] = marker.MigratedAt
	} else if marker.Version >= 2 {
		next.MigratedAt = marker.MigratedAt
		for tool, at := range marker.Migrated {
			next.Migrated[tool] = at
		}
	}
	for _, tool := range tools {
		if _, done := next.Migrated[tool]; !done {
			next.Migrated[tool] = now
		}
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.tokenCopiesPath(), append(data, '\n'), 0o600)
}

func (m *Manager) unmarkTokenCopies(tool string) error {
	marker, ok := usages.ReadTokenCopiesMarker(m.root)
	if !ok {
		return nil
	}
	if marker.Version == 1 {
		marker.Version = 2
		marker.Migrated = map[string]time.Time{ToolClaude: marker.MigratedAt, ToolCodex: marker.MigratedAt}
	}
	delete(marker.Migrated, tool)
	if len(marker.Migrated) == 0 {
		if err := os.Remove(m.tokenCopiesPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.tokenCopiesPath(), append(data, '\n'), 0o600)
}

// UnmigrateTokenCopies returns one tool to the switcher's link model. The full
// stored credential remains the recovery copy until the live write succeeds;
// the marker is removed last so a failed operation can be retried.
func (m *Manager) UnmigrateTokenCopies(tool string) error {
	if tool != ToolClaude && tool != ToolCodex {
		return fmt.Errorf("unknown tool %q", tool)
	}
	if !m.TokenCopiesFor(tool) {
		return m.EnsureActiveLinks()
	}
	pre, err := m.loadState()
	if err != nil {
		return err
	}
	profile := pre.Active[tool]
	if profile == "" {
		return ErrNoActiveProfile
	}
	if err := validateProfileName(profile); err != nil {
		return err
	}
	storePath := m.profileCredPath(tool, profile)
	release, err := usages.HoldMaintenance(m.root, tool, profile, storePath, MaintenanceWait)
	if err != nil {
		return err
	}
	defer release()
	livePath, err := m.liveCredPath(tool)
	if err != nil {
		return err
	}
	if live, readErr := os.ReadFile(livePath); readErr == nil && credtarget.HasRefreshToken(tool, live) {
		if !sameStoredRefresh(tool, storePath, live) {
			return ErrLoginPending
		}
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if live, readErr := os.ReadFile(livePath); os.IsNotExist(readErr) || (readErr == nil && !credtarget.HasRefreshToken(tool, live)) {
		if err := m.refreshIncomingIfDue(tool, profile); err != nil {
			return err
		}
	}
	lock, err := m.acquireLock()
	if err != nil {
		return err
	}
	defer lock.unlock()
	state, err := m.loadState()
	if err != nil {
		return err
	}
	if state.Active[tool] != profile {
		return errors.New("active profile changed while disabling token auto-refresh; retry")
	}
	if !m.TokenCopiesFor(tool) {
		return nil
	}
	if err := validateStoredCred(tool, storePath); err != nil {
		return err
	}
	full, err := os.ReadFile(storePath)
	if err != nil {
		return err
	}
	live, readErr := os.ReadFile(livePath)
	if readErr == nil && credtarget.HasRefreshToken(tool, live) && !sameStoredRefresh(tool, storePath, live) {
		return ErrLoginPending
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if m.copyHooks.RestoreFull == nil {
		return errors.New("full-credential restore is not wired")
	}
	// Store first: persist the merged full document (stored credential plus the
	// live file's unrelated keys such as MCP OAuth) before the live file gets the
	// refresh token, so the two never diverge. A crash after this write leaves the
	// live file token-only with the marker still set; the retry is clean and the
	// reconciler never sees a login. When the live file already holds the full
	// credential (a resumed run) it is the document.
	merged := live
	if !credtarget.HasRefreshToken(tool, live) {
		merged, err = credtarget.BuildFullTarget(tool, live, full)
		if err != nil {
			return err
		}
	}
	if err := atomicWrite(storePath, merged, 0o600); err != nil {
		return err
	}
	if !credtarget.HasRefreshToken(tool, live) {
		if err := m.copyHooks.RestoreFull(tool, merged); err != nil {
			return err
		}
		// A worker may have added a key between the two writes: the live file
		// stays the source of truth, so the store catches up again.
		if live, err = os.ReadFile(livePath); err != nil {
			return err
		}
		if !bytes.Equal(live, merged) {
			if err := atomicWrite(storePath, live, 0o600); err != nil {
				return err
			}
		}
	}
	// Stop the server refresher before exposing the full live credential to the
	// legacy link model. If linking fails or the process crashes, the next OFF
	// tick's EnsureActiveLinks repairs the link from this full live file.
	if err := m.unmarkTokenCopies(tool); err != nil {
		return err
	}
	return m.linkStoredToLive(tool, profile, livePath)
}

func sameStoredRefresh(tool, storePath string, live []byte) bool {
	stored, err := os.ReadFile(storePath)
	if err != nil {
		return false
	}
	a, err := credtarget.ParseFull(tool, stored)
	if err != nil {
		return false
	}
	b, err := credtarget.ParseFull(tool, live)
	return err == nil && a.RefreshToken == b.RefreshToken
}

// defaultRefreshLead is the default lead, used only when there is no refresher to
// ask. With a refresher wired it is the one that decides (it applies the
// AIMEBU_TOKEN_LEAD_* knobs, the post-rotation floor and the lead cap), so the
// switcher never second-guesses it with a constant of its own.
func defaultRefreshLead(tool string) time.Duration {
	if tool == ToolCodex {
		return usages.TargetLeadCodex
	}
	return usages.TargetLeadClaude
}

// refreshIncomingIfDue asks the refresher to bring the incoming profile up to
// date; the refresher decides whether it is due and returns nil when it is not.
// Missing or empty profiles are skipped (an empty profile has nothing to
// refresh). Without a refresher (a CLI process) a profile inside the default
// lead is refused rather than handed over dying.
func (m *Manager) refreshIncomingIfDue(tool, name string) error {
	data, err := os.ReadFile(m.profileCredPath(tool, name))
	if err != nil {
		return nil
	}
	cred, err := credtarget.ParseFull(tool, data)
	if err != nil || cred.ExpiresAt == nil {
		return nil
	}
	if m.copyHooks.RefreshIncoming == nil {
		if time.Until(*cred.ExpiresAt) > defaultRefreshLead(tool) {
			return nil
		}
		// A due profile with no refresher must not be handed to sessions as is.
		return fmt.Errorf("%w %s/%s: it is due for a refresh and this process has no refresher; start the aimebu server (it refreshes due profiles) or switch from the web UI once it is running", ErrIncomingRefresh, tool, name)
	}
	// Longer than a CLI run (4 min), so the refresher's own timeout decides: a
	// cancelled parent would be classified as an unknown outcome and park the
	// account in needs_relogin_soon.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := m.copyHooks.RefreshIncoming(ctx, tool, name); err != nil {
		return fmt.Errorf("%w %s/%s: %v", ErrIncomingRefresh, tool, name, err)
	}
	return nil
}

// switchTokenCopies is Switch for the token-copy model. The caller holds the
// target account lock and the switcher flock, and has verified that the switcher
// is enabled, that the target profile exists and differs from the active one.
func (m *Manager) switchTokenCopies(tool, name, activeProfile string) (active string, switched bool, err error) {
	if m.copyHooks.FanOutActive == nil {
		return "", false, errors.New("switch refused: token-copy fan-out is not wired")
	}
	livePath, err := m.liveCredPath(tool)
	if err != nil {
		return "", false, err
	}
	// A refresh token in the live file is a login nobody imported yet.
	if liveData, readErr := os.ReadFile(livePath); readErr == nil && credtarget.HasRefreshToken(tool, liveData) {
		return "", false, ErrLoginPending
	}
	// The live file must still be readable as a token-only target (or absent).
	if err := m.validateLiveForCopies(tool, livePath); err != nil {
		return "", false, err
	}

	targetPath := m.profileCredPath(tool, name)
	targetData, readErr := os.ReadFile(targetPath)
	emptyTarget := os.IsNotExist(readErr)
	if readErr != nil && !emptyTarget {
		return "", false, fmt.Errorf("read target credentials: %w", readErr)
	}
	if !emptyTarget {
		if valErr := validateStoredCred(tool, targetPath); valErr != nil {
			return "", false, fmt.Errorf("%w: target profile: %s", ErrInvalidCredentials, valErr)
		}
	}

	var outgoing []byte
	if data, rerr := os.ReadFile(m.profileCredPath(tool, activeProfile)); rerr == nil {
		outgoing = data
	}
	var liveEmail string
	if tool == ToolClaude {
		liveEmail = m.claudeEmailFromDotJSON()
		if out := m.readProfileMeta(tool, activeProfile); liveEmail != "" && liveEmail != out.StaleEmail {
			m.writeProfileMeta(tool, activeProfile, profileMeta{Email: liveEmail})
		}
	}

	state, err := m.loadState()
	if err != nil {
		return "", false, err
	}
	if emptyTarget {
		// Empty profile: state first (a crash then leaves working live credentials
		// and a pointer at the new empty profile), then clear the live file so the
		// CLI asks for a fresh login, which the reconciler imports into it.
		state.Active[tool] = name
		if stateErr := m.saveState(state); stateErr != nil {
			return "", false, fmt.Errorf("update state: %w", stateErr)
		}
		var rmErr error
		if m.copyHooks.ClearActive != nil {
			rmErr = m.copyHooks.ClearActive(tool)
		} else if err := os.Remove(livePath); err != nil && !os.IsNotExist(err) {
			rmErr = err
		}
		if rmErr != nil {
			state.Active[tool] = activeProfile
			_ = m.saveState(state)
			return "", false, fmt.Errorf("remove live credentials for empty profile: %w", rmErr)
		}
		m.markIncomingStale(tool, name, liveEmail)
		m.recordSwitchAway(tool, activeProfile)
		return name, true, nil
	}

	// Rewrite the live targets first: a failure leaves the old account fully in
	// place (each target is an atomic rename).
	if fanErr := m.copyHooks.FanOutActive(tool, targetData); fanErr != nil {
		return "", false, fmt.Errorf("rewrite live credentials: %w", fanErr)
	}
	state.Active[tool] = name
	if stateErr := m.saveState(state); stateErr != nil {
		if len(outgoing) > 0 {
			_ = m.copyHooks.FanOutActive(tool, outgoing) // best-effort rollback
		}
		return "", false, fmt.Errorf("update state: %w", stateErr)
	}
	m.markIncomingStale(tool, name, liveEmail)
	m.recordSwitchAway(tool, activeProfile)
	return name, true, nil
}

// validateLiveForCopies accepts an absent live file or a parseable token-only
// one; a corrupt file refuses the switch (never overwrite what we cannot read).
func (m *Manager) validateLiveForCopies(tool, livePath string) error {
	if _, err := os.Stat(livePath); os.IsNotExist(err) {
		return nil
	}
	return validateLiveCred(tool, livePath)
}
