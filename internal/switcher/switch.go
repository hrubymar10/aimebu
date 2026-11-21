package switcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/credtarget"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// Switch performs the safe credential swap. The ordering — validate, capture,
// backup, restore — is what prevents the token-staleness bug: the outgoing
// profile's stored copy is refreshed before the incoming one replaces it, so
// switching back always sees the most recently live credentials.
//
// Algorithm:
//  1. Guard: disabled / not eligible / profile missing / no active profile.
//  2. Validate the live credentials. Abort before writing anything if corrupt.
//  3. Capture: copy live → profiles/<tool>/<active>/.
//  4. Backup: copy live → backups/<tool>/<active>-<unix>.json.
//  5. Restore: copy profiles/<tool>/<name>/ → live path, or delete live file
//     for an empty profile (forces a fresh login). For Claude the MCP OAuth
//     logins of the outgoing live file are merged into the incoming credential
//     (the live side wins a conflict, the incoming copy is backed up first), and
//     an empty profile keeps them: the live file is reduced to its mcpOAuth
//     section instead of being deleted. Claude's storage-write lock is held from
//     the first read of the live file to the end of the swap.
//  6. Update state.json.
//  7. On failure in 5–6: restore from the step-4 backup bytes, return original error.
//     Never leave the live location half-written.
//  8. Link: replace the now-active profile's stored credential file with a
//     symlink to the live credential path, so external readers of the stored
//     path follow the harness's token rotation instead of an orphaned copy.
//     Inactive profiles always hold plain files: step 3's temp+rename replaces
//     the outgoing link itself (rename never writes through a link). If the
//     switch fails after step 3, the still-active outgoing profile is re-linked
//     once the rollback has restored its live bytes.
//
// The flock on switcher/.lock is held for the entire operation so a concurrent
// CLI or server switch cannot interleave.
func (m *Manager) Switch(tool, name string) (active string, switched bool, err error) {
	return m.SwitchGuarded(tool, name, SwitchGuard{})
}

// SwitchForce installs stored credentials without refreshing the incoming profile.
// It still uses the normal guarded swap and token-only fan-out.
func (m *Manager) SwitchForce(tool, name string) (active string, switched bool, err error) {
	return m.switchGuarded(tool, name, SwitchGuard{}, true)
}

// ErrActiveChanged: a guarded switch found a different profile active than the one the
// caller decided from (someone switched in the meantime). Nothing was changed.
var ErrActiveChanged = errors.New("the active profile changed before the switch could run")

// SwitchGuard are conditions checked while the switcher lock is held, immediately before
// anything is written. They exist for callers that decided on a snapshot taken earlier
// (the automatic switcher): by the time the lock is held a manual switch may have moved
// the active profile, and a ≤45s account wait or incoming refresh sits in between.
type SwitchGuard struct {
	// ExpectedActive, when set, must still be the active profile of the tool; otherwise
	// the switch is refused with ErrActiveChanged.
	ExpectedActive string
	// Quiet, when set, is called under the lock immediately before the swap, after every other
	// check. It is for a condition that can change while the caller waits for the account lock or
	// the incoming refresh (a turn starting on the tool). A non-nil error aborts the switch before
	// anything is written and is returned unchanged.
	Quiet func() error
	// Hold, when set, is called under the lock right before Quiet and returns a release that runs when the switch
	// returns. It keeps the condition Quiet checked from changing before the swap has finished. The hold must not
	// open a store transaction (Store.HoldTurns blocks them all) and should cover file I/O only.
	Hold func() (release func())
}

// SwitchGuarded is Switch with its guard conditions checked under the lock.
func (m *Manager) SwitchGuarded(tool, name string, guard SwitchGuard) (active string, switched bool, err error) {
	return m.switchGuarded(tool, name, guard, false)
}

// SwitchForceGuarded keeps the server's turn and warm-agent guards for a forced switch.
func (m *Manager) SwitchForceGuarded(tool, name string, guard SwitchGuard) (active string, switched bool, err error) {
	return m.switchGuarded(tool, name, guard, true)
}

func (m *Manager) switchGuarded(tool, name string, guard SwitchGuard, force bool) (active string, switched bool, err error) {
	for attempts := 0; attempts < 5; attempts++ {
		active, switched, err = m.switchGuardedOnce(tool, name, guard, force)
		if guard.ExpectedActive != "" || !errors.Is(err, ErrActiveChanged) {
			return active, switched, err
		}
	}
	return active, false, fmt.Errorf("%w: active profile kept changing", ErrMaintenanceBusy)
}

func (m *Manager) switchGuardedOnce(tool, name string, guard SwitchGuard, force bool) (active string, switched bool, err error) {
	if err := validateTool(tool); err != nil {
		return "", false, err
	}
	// 1. Validate the target name before any I/O. Apply the same validateProfileName
	// guard that Add/Import/Rename/Remove all use — a check applied everywhere except
	// one place reads as complete in review but isn't.
	if err := validateProfileName(name); err != nil {
		return "", false, err
	}
	observedActive, err := m.Active(tool)
	if err != nil {
		return "", false, err
	}
	if observedActive != "" {
		if err := validateProfileName(observedActive); err != nil {
			return "", false, fmt.Errorf("corrupt state: active profile name invalid: %w", err)
		}
	}

	// Hold both sides' account locks through the swap: an outgoing refresh must
	// not rotate while its credentials are captured. Account identities are
	// acquired in sorted order before the switcher lock, so two switches and a
	// refresh cannot form a lock cycle. The active pointer is checked again under
	// the switcher lock before writing, since it may change during the wait.
	accounts := []usages.MaintenanceProfile{{Name: name, CredPath: m.profileCredPath(tool, name)}}
	if observedActive != "" && observedActive != name {
		accounts = append(accounts, usages.MaintenanceProfile{Name: observedActive, CredPath: m.profileCredPath(tool, observedActive)})
	}
	releaseAccount, holdErr := usages.HoldMaintenanceProfiles(m.root, tool, accounts, MaintenanceWait)
	if errors.Is(holdErr, usages.ErrMaintenanceBusy) {
		return "", false, fmt.Errorf("%w: %v; try again in a minute", ErrMaintenanceBusy, holdErr)
	}
	if holdErr != nil {
		// Fail closed: without the account lock a background run could be
		// rotating this token while it goes live.
		return "", false, fmt.Errorf("switch refused: %w", holdErr)
	}
	defer releaseAccount()

	// Token-copy model: bring the INCOMING profile up to date first, so sessions
	// are never handed a dying account. Runs under the account lock held above and
	// BEFORE the switcher flock, because the refresher commits under that flock.
	tokenCopies := m.TokenCopiesFor(tool)
	if tokenCopies && !force {
		if err := m.refreshIncomingIfDue(tool, name); err != nil {
			return "", false, err
		}
	}

	// Check eligibility before taking the lock for a useful early error. It is
	// checked again under the lock before any write: a login may change between
	// this read and the lock acquisition.
	//
	// Absent live creds do NOT block the switch — switching away
	// from a profile with no live login captures nothing and proceeds, so you
	// can't get stuck on an empty active profile. Corrupt creds still refuse:
	// capturing a corrupt file over a good stored profile would destroy real
	// credentials, the one thing this feature must never do.
	el, err := m.eligibilityFor(tool)
	if err != nil {
		return "", false, err
	}
	if el.TargetCopy {
		return "", false, fmt.Errorf("%w: %s", ErrTargetCopy, tool)
	}
	if !el.Eligible && !el.Absent {
		return "", false, &NotEligibleError{Tool: tool, Reason: el.Reason}
	}
	liveAbsent := el.Absent

	if m.beforeSwitchLock != nil {
		m.beforeSwitchLock()
	}

	// Acquire the flock before reading or writing any state.
	lock, lockErr := m.acquireLock()
	if lockErr != nil {
		return "", false, fmt.Errorf("acquire switcher lock: %w", lockErr)
	}
	defer func() {
		if unlockErr := lock.unlock(); unlockErr != nil && err == nil {
			err = unlockErr
		}
	}()
	el, err = m.eligibilityFor(tool)
	if err != nil {
		return "", false, err
	}
	if el.TargetCopy {
		return "", false, fmt.Errorf("%w: %s", ErrTargetCopy, tool)
	}
	if !el.Eligible && !el.Absent {
		return "", false, &NotEligibleError{Tool: tool, Reason: el.Reason}
	}
	liveAbsent = el.Absent

	// Read state once; check enabled and active inside the lock so SetEnabled
	// and Switch are serialized on the same flock — no cross-lock race.
	state, err := m.loadState()
	if err != nil {
		return "", false, err
	}
	if !state.Enabled {
		return "", false, ErrDisabled
	}
	activeProfile := state.Active[tool]
	if activeProfile == "" {
		return "", false, ErrNoActiveProfile
	}
	// Validate the stored active name too — defence in depth. state.json could
	// contain a poisoned value written before this guard existed, or have been
	// hand-edited. A stored traversal name would turn the capture step into an
	// arbitrary file write; refuse to proceed rather than trusting old state.
	if err := validateProfileName(activeProfile); err != nil {
		return "", false, fmt.Errorf("corrupt state: active profile name invalid: %w", err)
	}
	targetDir := m.profileDir(tool, name)
	if _, statErr := os.Stat(targetDir); statErr != nil {
		return "", false, fmt.Errorf("%w: %s/%s", ErrProfileNotFound, tool, name)
	}
	if force {
		if _, statErr := os.Stat(m.profileCredPath(tool, name)); statErr != nil {
			return "", false, fmt.Errorf("force switch requires stored credentials for %s/%s: %w", tool, name, statErr)
		}
	}

	// Switching to the already-active profile is an explicit no-op: return before
	// any capture or restore. A pointless capture-and-restore cycle over the live
	// credentials file is the one thing this feature must never risk — even a
	// no-op-looking round trip writes to a live path for no reason, and an empty
	// already-active profile would otherwise delete the live file. This also
	// covers the UI re-entry window where a second Switch click targets the
	// profile that is already active.
	if name == activeProfile {
		return name, false, nil
	}
	if activeProfile != observedActive {
		return activeProfile, false, fmt.Errorf("%w: expected %s, %s is active", ErrActiveChanged, observedActive, activeProfile)
	}
	if guard.ExpectedActive != "" && activeProfile != guard.ExpectedActive {
		return activeProfile, false, fmt.Errorf("%w: expected %s, %s is active", ErrActiveChanged, guard.ExpectedActive, activeProfile)
	}
	if guard.Hold != nil {
		defer guard.Hold()()
	}
	if guard.Quiet != nil {
		if err := guard.Quiet(); err != nil {
			return activeProfile, false, err
		}
	}

	// Token-copy model (after `migrate-copies`): the live file is a token-only
	// copy, so there is nothing to capture or restore. See token_copies.go.
	if m.TokenCopiesFor(tool) {
		if !tokenCopies {
			// migrate-copies finished between the unlocked check and the lock: the
			// incoming profile was not refreshed above, and the link-model steps
			// below would be wrong. Nothing has been changed; retry.
			return "", false, ErrMigrationRaced
		}
		return m.switchTokenCopies(tool, name, activeProfile)
	}

	livePath, err := m.liveCredPath(tool)
	if err != nil {
		return "", false, err
	}

	// Claude: hold its storage-write lock from the first read of the live file to the end of
	// the swap. The MCP logins carried to the incoming profile are read from the live file, and
	// the CLI writes them into it under this same lock: without it a login or refresh landing
	// between the read and the replacement would be lost.
	if tool == ToolClaude {
		if m.beforeStorageLock != nil {
			m.beforeStorageLock()
		}
		release, lockErr := m.lockClaudeStorage(livePath)
		if lockErr != nil {
			return "", false, lockErr
		}
		if release != nil {
			defer func() {
				if release() {
					log.Printf("switcher: %s: the storage-write lock lost its heartbeat during the switch; the swap itself completed", tool)
					telemetry.WarnCause(context.Background(), "switcher", "the storage-write lock lost its heartbeat during a switch; the swap itself completed")
				}
			}()
		}
		// The eligibility read above predates the lock: a /login or a logout may have landed since.
		// Judge the file again now that nothing can write it, so a login that arrived in between is
		// captured and backed up like any other instead of being skipped as "absent".
		if el, err = m.eligibilityFor(tool); err != nil {
			return "", false, err
		}
		if el.TargetCopy {
			return "", false, fmt.Errorf("%w: %s", ErrTargetCopy, tool)
		}
		if !el.Eligible && !el.Absent {
			return "", false, &NotEligibleError{Tool: tool, Reason: el.Reason}
		}
		liveAbsent = el.Absent
	}
	// What the live file holds before the switch (nil when absent). A logged-out placeholder
	// counts as absent for capture but may still carry MCP logins, so it is read regardless.
	preLive, readErr := os.ReadFile(livePath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", false, fmt.Errorf("%w: cannot read live credentials: %s", ErrInvalidCredentials, readErr)
	}

	// 2. Validate the live credentials before writing anything. A corrupt live
	// file must abort here — storing those bytes into the outgoing profile would
	// overwrite the known-good stored copy with garbage. SKIPPED when the live
	// file is absent (liveAbsent): there's nothing to capture or validate, so we
	// proceed straight to restoring the target.
	var liveData []byte
	if !liveAbsent {
		var readErr error
		liveData, readErr = os.ReadFile(livePath)
		if readErr != nil {
			return "", false, fmt.Errorf("%w: cannot read live credentials: %s", ErrInvalidCredentials, readErr)
		}
		if valErr := validateLiveCred(tool, livePath); valErr != nil {
			return "", false, valErr
		}
	}

	// Any failure from here on leaves the outgoing profile active; once capture
	// has replaced its link with a snapshot, re-link it (step 8 rollback).
	captured := false
	defer func() {
		if err != nil && captured {
			m.relinkAfterFailedSwitch(tool, activeProfile, livePath, liveData)
		}
	}()

	// 3. Capture: update the stored copy of the outgoing profile. SKIPPED when
	// liveAbsent — capturing nothing is exactly the point. atomicWrite renames
	// a temp file over captureDest, which replaces an active-profile symlink
	// with a regular snapshot rather than writing through it to the live file.
	//
	// When liveAbsent and the outgoing stored file is a symlink, that link now
	// dangles (the harness logged out or the live file was deleted): drop it so
	// the outgoing profile reads as "no stored credentials". The copy from an
	// earlier switch-away, if any, is still under backups/.
	if liveAbsent {
		if err := m.removeStoredLink(tool, activeProfile); err != nil {
			return "", false, err
		}
	} else {
		captureDir := m.profileDir(tool, activeProfile)
		if err := os.MkdirAll(captureDir, 0o700); err != nil {
			return "", false, fmt.Errorf("capture: mkdir: %w", err)
		}
		captureDest := m.profileCredPath(tool, activeProfile)
		captured = true
		if err := atomicWrite(captureDest, liveData, 0o600); err != nil {
			return "", false, fmt.Errorf("capture: %w", err)
		}
	}
	// Capture the outgoing account's identity too (claude only). SKIPPED when
	// liveAbsent — no live login means no outgoing identity to capture.
	var liveEmail string
	if !liveAbsent && tool == ToolClaude {
		liveEmail = m.claudeEmailFromDotJSON()
		// The outgoing profile was genuinely in use, so this address is its own —
		// unless it is itself only the stale marker from an earlier switch.
		if outgoing := m.readProfileMeta(tool, activeProfile); liveEmail != "" && liveEmail != outgoing.StaleEmail {
			m.writeProfileMeta(tool, activeProfile, profileMeta{Email: liveEmail})
		}
	}

	// 4. Backup: safety copy of the live file before we overwrite it. SKIPPED
	// when liveAbsent — there's no live file to back up.
	if !liveAbsent {
		if err := os.MkdirAll(filepath.Join(m.backupsRoot(), tool), 0o700); err != nil {
			return "", false, fmt.Errorf("backup: mkdir: %w", err)
		}
		if err := m.writeUniqueBackup(tool, activeProfile, liveData); err != nil {
			return "", false, fmt.Errorf("backup: %w", err)
		}
	}

	// 5+6: ordering depends on whether the target profile is empty.
	// for an empty target, save state.json BEFORE deleting the live file.
	// A crash after saveState and before Remove leaves working credentials (live
	// file intact) with state pointing at the new empty profile — recoverable by
	// running the harness, which will prompt for login and write a new live file.
	// The old ordering (delete then save) could leave state.Active pointing at
	// the OLD profile while no live file exists, blocking every further Switch
	// because eligibility requires the live file to be present and valid.
	//
	// For a non-empty target, restore the new credentials first (step 5) then
	// save state (step 6): if restore fails the live file is still the old one,
	// and the rollback is a no-op on state.json (never written).
	targetCredPath := m.profileCredPath(tool, name)
	_, targetStatErr := os.Stat(targetCredPath)
	if os.IsNotExist(targetStatErr) {
		// A dangling symlink also reads as not-exist: the target has no stored
		// credentials, so treat it as empty and drop the link so the profile
		// does not become active pointing at nothing.
		if err := m.removeStoredLink(tool, name); err != nil {
			return "", false, err
		}
		// Empty target: state first, then clear live (the account login goes, MCP logins stay).
		state.Active[tool] = name
		if stateErr := m.saveState(state); stateErr != nil {
			return "", false, fmt.Errorf("update state: %w", stateErr)
		}
		if removeErr := clearLiveLogin(tool, livePath, preLive); removeErr != nil {
			// State is updated but live file persists. Rollback both.
			//
			// With liveAbsent this is reachable only when the placeholder carried MCP logins
			// and rewriting it failed; os.Remove on an absent file reports success. The
			// rollback bytes are then the placeholder as it was (preLive), and nothing is
			// written when there are none: an atomicWrite of nil bytes to a live credentials
			// path is the scariest line in this file, so keep the guard below.
			state.Active[tool] = activeProfile
			_ = m.saveState(state)
			rollback := liveData
			if liveAbsent {
				rollback = preLive
			}
			if rollback != nil {
				liveDir, _ := m.liveCredDir(tool)
				_ = os.MkdirAll(liveDir, 0o700)
				if rbErr := atomicWrite(livePath, rollback, 0o600); rbErr != nil {
					return "", false, fmt.Errorf("remove live failed (%s) and rollback also failed (%s)", removeErr, rbErr)
				}
			}
			return "", false, fmt.Errorf("remove live credentials for empty profile: %w", removeErr)
		}
		m.markIncomingStale(tool, name, liveEmail)
		m.recordSwitchAway(tool, activeProfile)
		return name, true, nil
	}

	// Non-empty target: restore credentials first (step 5), then update state (step 6).
	restoreErr := m.restoreProfile(tool, name, livePath, preLive)
	if restoreErr != nil {
		// For liveAbsent the live file was absent before the switch and restore
		// failed before writing (atomicWrite is temp+rename, so no partial), so
		// live is still absent — nothing to roll back. For non-absent, write the
		// captured liveData back to restore the pre-switch credentials.
		if !liveAbsent {
			liveDir, _ := m.liveCredDir(tool)
			_ = os.MkdirAll(liveDir, 0o700)
			if rbErr := atomicWrite(livePath, liveData, 0o600); rbErr != nil {
				return "", false, fmt.Errorf("restore failed (%s) and rollback also failed (%s)", restoreErr, rbErr)
			}
		}
		return "", false, restoreErr
	}

	state.Active[tool] = name
	if stateErr := m.saveState(state); stateErr != nil {
		// Restore succeeded so live now holds the target's creds; state save
		// failed, so roll back to the pre-switch live state. For non-absent that's
		// the captured liveData; for liveAbsent the pre-state was no live file at
		// all, so remove it rather than writing nil bytes (a placeholder that was there goes back as it was).
		if liveAbsent {
			if preLive != nil {
				_ = atomicWrite(livePath, preLive, 0o600)
			} else {
				_ = os.Remove(livePath)
			}
		} else {
			liveDir, _ := m.liveCredDir(tool)
			_ = os.MkdirAll(liveDir, 0o700)
			if rbErr := atomicWrite(livePath, liveData, 0o600); rbErr != nil {
				return "", false, fmt.Errorf("state save failed (%s) and rollback also failed (%s)", stateErr, rbErr)
			}
		}
		return "", false, fmt.Errorf("update state: %w", stateErr)
	}

	m.recordSwitchAway(tool, activeProfile)

	// 8. Link the now-active profile's stored file to the live file. First
	// back up the bytes being replaced by the link: they are this profile's
	// last-known credentials, which detachLinkedProfile restores if the live
	// login is later replaced from outside a switch (Import). Best effort and
	// logged, not returned: the switch itself is complete and consistent here,
	// and a skipped link leaves the stored path holding the plain copy just
	// restored (the same bytes as live), which the next switch-away
	// re-captures anyway.
	if data, readErr := os.ReadFile(targetCredPath); readErr != nil {
		log.Printf("switcher: %s/%s: read stored credentials for link backup: %v; leaving a plain copy", tool, name, readErr)
		telemetry.WarnCause(context.Background(), "switcher", "reading stored credentials for the link backup; leaving a plain copy")
	} else if backupErr := m.writeUniqueBackup(tool, name, data); backupErr != nil {
		log.Printf("switcher: %s/%s: back up stored credentials before linking: %v; leaving a plain copy", tool, name, backupErr)
		telemetry.WarnCause(context.Background(), "switcher", "backing up stored credentials before linking; leaving a plain copy")
	} else if linkErr := m.linkStoredToLive(tool, name, livePath); linkErr != nil {
		log.Printf("switcher: %s/%s: link stored credentials to live: %v; leaving a plain copy", tool, name, linkErr)
		telemetry.WarnCause(context.Background(), "switcher", "linking stored credentials to live; leaving a plain copy")
	}

	m.markIncomingStale(tool, name, liveEmail)
	return name, true, nil
}

// markIncomingStale records, on the profile just switched to, the address
// .claude.json still holds — which belongs to the PREVIOUS account until the
// harness authenticates. Reads refuse to adopt that value, so a switch cycle
// can never mislabel a profile with the account it replaced. Skipped when the
// profile already knows its own address.
func (m *Manager) markIncomingStale(tool, name, liveEmail string) {
	if tool != ToolClaude || liveEmail == "" {
		return
	}
	meta := m.readProfileMeta(tool, name)
	if meta.Email != "" {
		return
	}
	meta.StaleEmail = liveEmail
	m.writeProfileMeta(tool, name, meta)
}

// restoreProfile validates the target profile's credentials and writes them to
// livePath, with the MCP logins of the outgoing live file (live) merged in for Claude.
// Only called for non-empty profiles — empty-profile clearing is handled
// directly in Switch so state.json can be written first.
func (m *Manager) restoreProfile(tool, name, livePath string, live []byte) error {
	targetCredPath := m.profileCredPath(tool, name)
	targetData, err := os.ReadFile(targetCredPath)
	if err != nil {
		return fmt.Errorf("read target credentials: %w", err)
	}

	// Validate the target credentials before overwriting live.
	if valErr := validateStoredCred(tool, targetCredPath); valErr != nil {
		return fmt.Errorf("%w: target profile: %s", ErrInvalidCredentials, valErr)
	}
	if tool == ToolClaude {
		if targetData, err = m.carryMCPLogins(tool, name, targetData, live); err != nil {
			return err
		}
	}

	liveDir, err := m.liveCredDir(tool)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		return fmt.Errorf("restore: mkdir live dir: %w", err)
	}
	if err := atomicWrite(livePath, targetData, 0o600); err != nil {
		return fmt.Errorf("restore: write live credentials: %w", err)
	}
	return nil
}

// validateStoredCred validates a stored credential file. The asymmetry with
// validateLiveCred is intentional: both reject malformed and accept expired.
// For claude, expiresAt == 0 is the observed corruption marker. For codex,
// the rule is loadCodexAuth success — access_token + refresh_token must be
// present; no JWT exp check is applied (an absent or past-expired id_token
// is structurally fine). The rule is "reject malformed, accept expired"; the
// corruption signal differs by tool.
func validateStoredCred(tool, path string) error {
	var err error
	switch tool {
	case ToolClaude:
		_, err = usages.ValidateFullClaudeCredentials(path)
	case ToolCodex:
		_, _, err = usages.ValidateFullCodexCredentials(path)
	default:
		return fmt.Errorf("unknown tool %q", tool)
	}
	return err
}

// linkStoredToLive replaces profile name's stored credential file with a
// symlink to the absolute live credential path, so anything reading
// profiles/<tool>/<name>/<file> while the profile is active sees the tokens the
// harness keeps rotating in the live file instead of an orphaned copy. Only the
// ACTIVE profile is ever linked; inactive profiles stay plain files.
//
// The link is created under a temp name in the profile dir and renamed over the
// stored path, so the stored path is always either the old regular file or the
// new link — never missing. The live file must exist: a link to nothing would
// just be a dangling stored path. A live path that itself resolves onto the
// stored path is refused, since linking it would create a loop and lose the
// bytes.
func (m *Manager) linkStoredToLive(tool, name, livePath string) error {
	absLive, err := filepath.Abs(livePath)
	if err != nil {
		return fmt.Errorf("link: resolve live path: %w", err)
	}
	resolvedLive, err := filepath.EvalSymlinks(absLive)
	if err != nil {
		return fmt.Errorf("link: live credentials unavailable: %w", err)
	}
	storedPath := m.profileCredPath(tool, name)
	dir := filepath.Dir(storedPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("link: mkdir: %w", err)
	}
	if realDir, dirErr := filepath.EvalSymlinks(dir); dirErr == nil && filepath.Join(realDir, filepath.Base(storedPath)) == resolvedLive {
		return fmt.Errorf("link: live credentials resolve to the stored profile file %s", storedPath)
	}
	// Reserve a unique temp name, then swap the reserved file for the link.
	// The switcher lock is held by every caller and the dir is 0700, so the
	// gap between Remove and Symlink is not contended.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(storedPath)+".*.link")
	if err != nil {
		return fmt.Errorf("link: %w", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	if err := os.Remove(tmpName); err != nil {
		return fmt.Errorf("link: %w", err)
	}
	if err := os.Symlink(absLive, tmpName); err != nil {
		return fmt.Errorf("link: %w", err)
	}
	if err := os.Rename(tmpName, storedPath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("link: %w", err)
	}
	return nil
}

// removeStoredLink deletes profile name's stored credential file when (and
// only when) it is a symlink. Used when switching away from a profile whose
// live file is gone: the link would dangle, so the profile is left with no
// stored credentials instead. A regular stored file is left untouched.
func (m *Manager) removeStoredLink(tool, name string) error {
	storedPath := m.profileCredPath(tool, name)
	info, err := os.Lstat(storedPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if err := os.Remove(storedPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stored credential link: %w", err)
	}
	return nil
}

// relinkAfterFailedSwitch re-links the still-active outgoing profile after a
// switch failed past the capture step (which replaced its link with a regular
// snapshot). It only links when the live file holds exactly the pre-switch
// bytes, i.e. the rollback really restored the outgoing account; otherwise the
// regular snapshot is kept, which is always a valid stored file.
func (m *Manager) relinkAfterFailedSwitch(tool, name, livePath string, liveData []byte) {
	state, err := m.loadState()
	if err != nil || state.Active[tool] != name {
		return
	}
	cur, err := os.ReadFile(livePath)
	if err != nil || !bytes.Equal(cur, liveData) {
		return
	}
	if err := m.linkStoredToLive(tool, name, livePath); err != nil {
		log.Printf("switcher: %s/%s: re-link after failed switch: %v; keeping the regular snapshot", tool, name, err)
		telemetry.ReportCause(context.Background(), "switcher", "re-link after a failed switch; keeping the regular snapshot")
	}
}

// detachLinkedProfile turns a profile that is about to stop being active
// without a switch (Import adopting a different live login) back into a plain
// inactive profile. Its stored file is a link to live, but live no longer holds
// this profile's account, so a snapshot of live would mislabel the new login
// as this profile. The correct bytes are this profile's last-known
// credentials: the newest valid backup under its name (written at every
// import, link-in, switch-away and link migration). Without one the link is
// dropped and the profile becomes empty, which is honest — there is nothing of
// its account left to keep. A regular stored file is left untouched.
func (m *Manager) detachLinkedProfile(tool, name string) error {
	storedPath := m.profileCredPath(tool, name)
	info, err := os.Lstat(storedPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if data, ok := m.newestValidBackup(tool, name); ok {
		if err := atomicWrite(storedPath, data, 0o600); err != nil {
			return fmt.Errorf("restore last-known credentials: %w", err)
		}
		return nil
	}
	log.Printf("switcher: %s/%s: no backup of its credentials; dropping its link to the live login (profile is now empty)", tool, name)
	if err := os.Remove(storedPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stored credential link: %w", err)
	}
	return nil
}

// backupActiveLiveAs writes a backup of the live credentials under name. Best
// effort, logged on failure.
func (m *Manager) backupActiveLiveAs(tool, name string) {
	livePath, err := m.liveCredPath(tool)
	if err != nil {
		return
	}
	data, err := os.ReadFile(livePath)
	if err != nil {
		return
	}
	if validateLiveCred(tool, livePath) != nil {
		return
	}
	if err := m.writeUniqueBackup(tool, name, data); err != nil {
		log.Printf("switcher: %s/%s: back up live credentials: %v", tool, name, err)
		telemetry.ReportCause(context.Background(), "switcher", "backing up live credentials")
	}
}

// carryMCPLogins merges the MCP OAuth logins of the outgoing live file into the credential of
// the profile being activated, so a login made to an MCP server while one account was active is
// still there after the switch. The active (live) side wins a server both hold with different
// content; the incoming bytes are backed up first in that case, and only server keys and the
// profile name reach the log. A live file whose MCP section cannot be read is not carried (logged,
// the switch proceeds: the live file is captured and backed up as always).
func (m *Manager) carryMCPLogins(tool, name string, incoming, live []byte) ([]byte, error) {
	res, err := credtarget.MergeClaudeMCPOAuth(incoming, live)
	if err != nil {
		log.Printf("switcher: %s/%s: MCP logins not carried over: %v", tool, name, err)
		telemetry.WarnCause(context.Background(), "switcher", "MCP logins not carried over")
		return incoming, nil
	}
	if bytes.Equal(res.Merged, incoming) {
		return incoming, nil
	}
	if !json.Valid(res.Merged) {
		log.Printf("switcher: %s/%s: MCP logins not carried over: merged credentials are not valid JSON", tool, name)
		telemetry.WarnCause(context.Background(), "switcher", "MCP logins not carried over: merged credentials are not valid JSON")
		return incoming, nil
	}
	if res.LosesIncoming() {
		if err := m.writeUniqueBackup(tool, name, incoming); err != nil {
			return nil, fmt.Errorf("back up %s/%s before merging MCP logins: %w", tool, name, err)
		}
		if len(res.Conflicts) > 0 {
			log.Printf("switcher: %s/%s: the active MCP login replaced the profile's for server(s) %s; the profile's copy is in its backups", tool, name, strings.Join(res.Conflicts, ", "))
			telemetry.WarnCause(context.Background(), "switcher", "the active MCP login replaced the profile's for some servers; the profile's copy is in its backups")
		} else {
			log.Printf("switcher: %s/%s: the profile's MCP login section was unreadable and was replaced by the active one; the profile's copy is in its backups", tool, name)
			telemetry.WarnCause(context.Background(), "switcher", "the profile's MCP login section was unreadable and was replaced by the active one")
		}
	}
	return res.Merged, nil
}

// clearLiveLogin makes the live credential file hold no account login: Claude keeps its MCP
// OAuth logins (preLive, the file as read under the storage lock) and the file is removed only
// when there are none, as it always was; every other tool's file is removed.
func clearLiveLogin(tool, livePath string, preLive []byte) error {
	if tool == ToolClaude {
		if only, ok := credtarget.ClaudeMCPOnly(preLive); ok {
			return atomicWrite(livePath, only, 0o600)
		}
	}
	if err := os.Remove(livePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// storageLockTimeout bounds the wait for Claude's storage-write lock; a var so tests need not wait it out.
var storageLockTimeout = 3 * time.Second

// lockClaudeStorage takes Claude's <configdir>/.storage-write.lock for the live credential file
// and returns its release (reporting a lost heartbeat). It returns a nil release when the config
// directory does not exist: no CLI can be writing there, and the lock is a directory inside it.
func (m *Manager) lockClaudeStorage(livePath string) (release func() bool, err error) {
	dir := filepath.Dir(livePath)
	if _, statErr := os.Stat(dir); statErr != nil {
		return nil, nil
	}
	rel, lockErr := (&credtarget.Writer{LockTimeout: storageLockTimeout}).LockStorage(dir)
	if lockErr != nil {
		return nil, fmt.Errorf("switch refused: Claude is writing its credentials, try again in a minute: %w", lockErr)
	}
	return rel, nil
}
