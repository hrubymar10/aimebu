package switcher

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hrubymar10/aimebu/internal/usages"
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
//     for an empty profile (forces a fresh login).
//  6. Update state.json.
//  7. On failure in 5–6: restore from the step-4 backup bytes, return original error.
//     Never leave the live location half-written.
//
// The flock on switcher/.lock is held for the entire operation so a concurrent
// CLI or server switch cannot interleave.
func (m *Manager) Switch(tool, name string) (active string, err error) {
	// 1. Validate the target name before any I/O. Apply the same validateProfileName
	// guard that Add/Import/Rename/Remove all use — a check applied everywhere except
	// one place reads as complete in review but isn't.
	if err := validateProfileName(name); err != nil {
		return "", err
	}

	// Check eligibility before taking the lock: reads only the live credentials
	// file and does not touch switcher state. Failing fast here avoids the lock
	// overhead when the tool is plainly not logged in.
	el, err := m.eligibilityFor(tool)
	if err != nil {
		return "", err
	}
	if !el.Eligible {
		return "", &NotEligibleError{Tool: tool, Reason: el.Reason}
	}

	// Acquire the flock before reading or writing any state.
	lock, lockErr := m.acquireLock()
	if lockErr != nil {
		return "", fmt.Errorf("acquire switcher lock: %w", lockErr)
	}
	defer func() {
		if unlockErr := lock.unlock(); unlockErr != nil && err == nil {
			err = unlockErr
		}
	}()

	// Read state once; check enabled and active inside the lock so SetEnabled
	// and Switch are serialized on the same flock — no cross-lock race.
	state, err := m.loadState()
	if err != nil {
		return "", err
	}
	if !state.Enabled {
		return "", ErrDisabled
	}
	activeProfile := state.Active[tool]
	if activeProfile == "" {
		return "", ErrNoActiveProfile
	}
	// Validate the stored active name too — defence in depth. state.json could
	// contain a poisoned value written before this guard existed, or have been
	// hand-edited. A stored traversal name would turn the capture step into an
	// arbitrary file write; refuse to proceed rather than trusting old state.
	if err := validateProfileName(activeProfile); err != nil {
		return "", fmt.Errorf("corrupt state: active profile name invalid: %w", err)
	}
	targetDir := m.profileDir(tool, name)
	if _, statErr := os.Stat(targetDir); statErr != nil {
		return "", fmt.Errorf("%w: %s/%s", ErrProfileNotFound, tool, name)
	}

	livePath, err := m.liveCredPath(tool)
	if err != nil {
		return "", err
	}

	// 2. Validate the live credentials before writing anything. A corrupt live
	// file must abort here — storing those bytes into the outgoing profile would
	// overwrite the known-good stored copy with garbage.
	liveData, readErr := os.ReadFile(livePath)
	if readErr != nil {
		return "", fmt.Errorf("%w: cannot read live credentials: %s", ErrInvalidCredentials, readErr)
	}
	if valErr := validateLiveCred(tool, livePath); valErr != nil {
		return "", valErr
	}

	// 3. Capture: update the stored copy of the outgoing profile.
	captureDir := m.profileDir(tool, activeProfile)
	if err := os.MkdirAll(captureDir, 0o700); err != nil {
		return "", fmt.Errorf("capture: mkdir: %w", err)
	}
	captureDest := m.profileCredPath(tool, activeProfile)
	if err := atomicWrite(captureDest, liveData, 0o600); err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	// Capture the outgoing account's identity too. Claude keeps its email in a
	// global .claude.json that stays behind when credentials move, so without
	// this the profile would later report whichever account is live.
	var liveEmail string
	if tool == ToolClaude {
		liveEmail = m.claudeEmailFromDotJSON()
		// The outgoing profile was genuinely in use, so this address is its own —
		// unless it is itself only the stale marker from an earlier switch.
		if outgoing := m.readProfileMeta(tool, activeProfile); liveEmail != "" && liveEmail != outgoing.StaleEmail {
			m.writeProfileMeta(tool, activeProfile, profileMeta{Email: liveEmail})
		}
	}

	// 4. Backup: safety copy of the live file before we overwrite it.
	backupFilePath := m.backupPath(tool, activeProfile, time.Now().Unix())
	if err := os.MkdirAll(filepath.Dir(backupFilePath), 0o700); err != nil {
		return "", fmt.Errorf("backup: mkdir: %w", err)
	}
	if err := atomicWrite(backupFilePath, liveData, 0o600); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}

	// 5+6: ordering depends on whether the target profile is empty.
	// §14.1: for an empty target, save state.json BEFORE deleting the live file.
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
		// Empty target: state first, then delete live.
		state.Active[tool] = name
		if stateErr := m.saveState(state); stateErr != nil {
			return "", fmt.Errorf("update state: %w", stateErr)
		}
		if removeErr := os.Remove(livePath); removeErr != nil && !os.IsNotExist(removeErr) {
			// State is updated but live file persists. Rollback both.
			state.Active[tool] = activeProfile
			_ = m.saveState(state)
			liveDir, _ := m.liveCredDir(tool)
			_ = os.MkdirAll(liveDir, 0o700)
			if rbErr := atomicWrite(livePath, liveData, 0o600); rbErr != nil {
				return "", fmt.Errorf("remove live failed (%s) and rollback also failed (%s)", removeErr, rbErr)
			}
			return "", fmt.Errorf("remove live credentials for empty profile: %w", removeErr)
		}
		m.markIncomingStale(tool, name, liveEmail)
		return name, nil
	}

	// Non-empty target: restore credentials first (step 5), then update state (step 6).
	restoreErr := m.restoreProfile(tool, name, livePath)
	if restoreErr != nil {
		liveDir, _ := m.liveCredDir(tool)
		_ = os.MkdirAll(liveDir, 0o700)
		if rbErr := atomicWrite(livePath, liveData, 0o600); rbErr != nil {
			return "", fmt.Errorf("restore failed (%s) and rollback also failed (%s)", restoreErr, rbErr)
		}
		return "", restoreErr
	}

	state.Active[tool] = name
	if stateErr := m.saveState(state); stateErr != nil {
		liveDir, _ := m.liveCredDir(tool)
		_ = os.MkdirAll(liveDir, 0o700)
		if rbErr := atomicWrite(livePath, liveData, 0o600); rbErr != nil {
			return "", fmt.Errorf("state save failed (%s) and rollback also failed (%s)", stateErr, rbErr)
		}
		return "", fmt.Errorf("update state: %w", stateErr)
	}

	m.markIncomingStale(tool, name, liveEmail)
	return name, nil
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
// livePath. Only called for non-empty profiles — empty-profile delete is handled
// directly in Switch so state.json can be written first (§14.1).
func (m *Manager) restoreProfile(tool, name, livePath string) error {
	targetCredPath := m.profileCredPath(tool, name)
	targetData, err := os.ReadFile(targetCredPath)
	if err != nil {
		return fmt.Errorf("read target credentials: %w", err)
	}

	// Validate the target credentials before overwriting live.
	if valErr := validateStoredCred(tool, targetCredPath); valErr != nil {
		return fmt.Errorf("%w: target profile: %s", ErrInvalidCredentials, valErr)
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
		_, err = usages.ValidateClaudeCredentials(path)
	case ToolCodex:
		_, err = usages.ValidateCodexCredentials(path)
	default:
		return fmt.Errorf("unknown tool %q", tool)
	}
	return err
}
