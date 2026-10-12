package switcher

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/hrubymar10/aimebu/internal/usages"
)

// List returns all profiles across all tools, sorted by tool then name.
func (m *Manager) List() ([]Profile, error) {
	state, err := m.loadState()
	if err != nil {
		return nil, err
	}
	var profiles []Profile
	for _, tool := range Tools() {
		toolDir := filepath.Join(m.profilesRoot(), tool)
		entries, err := os.ReadDir(toolDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("list profiles for %s: %w", tool, err)
		}
		active := state.Active[tool]
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			p := m.buildProfile(tool, name, name == active)
			profiles = append(profiles, p)
		}
	}
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].Tool != profiles[j].Tool {
			return profiles[i].Tool < profiles[j].Tool
		}
		return profiles[i].Name < profiles[j].Name
	})
	return profiles, nil
}

// buildProfile constructs a Profile by reading the appropriate credential file.
// §14.8: for the active profile, credentials are read from the live harness
// directory, not the stored snapshot. The harness refreshes the live file
// continuously; the stored copy is only updated on switch-away. Reading the
// store for the active profile would report stale expiry and, after a fresh
// login, show HasCreds=false even though the user is logged in.
// Inactive profiles read from their stored copy — the live file belongs to
// whoever is currently active.
func (m *Manager) buildProfile(tool, name string, active bool) Profile {
	p := Profile{Tool: tool, Name: name, Active: active}

	var credPath string
	if active {
		if lp, err := m.liveCredPath(tool); err == nil {
			credPath = lp
		}
	} else {
		credPath = m.profileCredPath(tool, name)
	}
	if credPath == "" {
		return p
	}

	switch tool {
	case ToolClaude:
		if expiresAt, err := usages.ValidateClaudeCredentials(credPath); err == nil {
			p.HasCreds = true
			p.ExpiresAt = expiresAt
			// Claude's email lives in a single global .claude.json that does NOT
			// move with credentials. Prefer the identity captured beside this
			// profile's own credentials: it is right for inactive profiles, and
			// also right for the active one immediately after a switch, when
			// .claude.json still names the previous account until the harness
			// re-authenticates. Fall back to the live file only when nothing was
			// captured yet (a profile logged into but never switched away from),
			// and only for the active profile — for an inactive one that would
			// report whichever account happens to be live, which is a lie.
			meta := m.readProfileMeta(tool, name)
			p.Email = meta.Email
			if p.Email == "" && active {
				// .claude.json lags a switch: until the harness authenticates it
				// still names the previous account. Adopt a live value only once
				// it differs from the one recorded at switch-in.
				if live := m.claudeEmailFromDotJSON(); live != "" && live != meta.StaleEmail {
					p.Email = live
					m.writeProfileMeta(tool, name, profileMeta{Email: live})
				}
			}
		}
	case ToolCodex:
		if idToken, accessToken, err := usages.ValidateCodexCredentials(credPath); err == nil {
			p.HasCreds = true
			p.Email = usages.CodexIDTokenEmail(idToken)
			p.ExpiresAt = usages.CodexTokenExpiry(accessToken)
		}
	}
	return p
}

// Active returns the active profile name for tool, or "" when none is active.
func (m *Manager) Active(tool string) (string, error) {
	state, err := m.loadState()
	if err != nil {
		return "", err
	}
	return state.Active[tool], nil
}

// Add creates an empty profile directory. No credentials are stored. Backups
// left under the name by an earlier profile are deleted first, so the new
// profile never inherits another account's credentials.
func (m *Manager) Add(tool, name string) error {
	if err := validateProfileName(name); err != nil {
		return err
	}
	lock, lockErr := m.acquireLock()
	if lockErr != nil {
		return fmt.Errorf("acquire switcher lock: %w", lockErr)
	}
	defer lock.unlock()

	dir := m.profileDir(tool, name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%w: %s/%s", ErrProfileExists, tool, name)
	}
	if err := m.deleteProfileBackups(tool, name); err != nil {
		return fmt.Errorf("add: clear leftover backups: %w", err)
	}
	return os.MkdirAll(dir, 0o700)
}

// Import validates the live credentials for tool, creates a new profile
// directory whose stored credential file is a symlink to the live file, and
// marks the profile active. No switch happens — this is "adopt what is already
// there". The link (rather than a copy) is the same invariant Switch maintains
// for every active profile: the stored path follows the harness's token
// rotation, and the first switch away replaces it with a regular snapshot. The
// UI must refuse switching without a prior Import because there would be
// nowhere to capture the outgoing credentials.
//
// Import also writes a backup of the adopted live bytes, so a profile that was
// only ever imported still has a recovery copy if the live file disappears
// before its first switch away. When another profile was active, its stored
// file is still a link to live — which now holds the login being imported, not
// that profile's account — so it is detached first (see detachLinkedProfile).
func (m *Manager) Import(tool, name string) error {
	if err := validateProfileName(name); err != nil {
		return err
	}
	livePath, err := m.liveCredPath(tool)
	if err != nil {
		return err
	}

	lock, lockErr := m.acquireLock()
	if lockErr != nil {
		return fmt.Errorf("acquire switcher lock: %w", lockErr)
	}
	defer lock.unlock()

	// Validate before touching anything.
	if err := validateLiveCred(tool, livePath); err != nil {
		return err
	}
	liveData, err := os.ReadFile(livePath)
	if err != nil {
		return fmt.Errorf("%w: cannot read live credentials: %s", ErrInvalidCredentials, err)
	}
	state, err := m.loadState()
	if err != nil {
		return err
	}
	previous := state.Active[tool]
	if previous != "" {
		if err := validateProfileName(previous); err != nil {
			return fmt.Errorf("corrupt state: active profile name invalid: %w", err)
		}
	}

	dir := m.profileDir(tool, name)
	if _, statErr := os.Stat(dir); statErr == nil {
		return fmt.Errorf("%w: %s/%s", ErrProfileExists, tool, name)
	}
	// Leftover backups under this name belong to an earlier profile's
	// account; clear them before writing this profile's own.
	if err := m.deleteProfileBackups(tool, name); err != nil {
		return fmt.Errorf("import: clear leftover backups: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := m.writeUniqueBackup(tool, name, liveData); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("import: backup: %w", err)
	}
	if tool == ToolClaude {
		// Importing adopts the login that is live right now, so .claude.json is
		// authoritative for it.
		m.writeProfileMeta(tool, name, profileMeta{Email: m.claudeEmailFromDotJSON()})
	}
	if err := m.linkStoredToLive(tool, name, livePath); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("import credentials: %w", err)
	}

	// Detach the previously active profile before moving the active pointer, so
	// no inactive profile is ever left following the live login.
	if previous != "" && previous != name {
		if err := m.detachLinkedProfile(tool, previous); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("import: detach previously active profile %s/%s: %w", tool, previous, err)
		}
	}

	// Mark this profile active in state.json.
	state.Active[tool] = name
	if err := m.saveState(state); err != nil {
		m.rollbackImport(tool, name, previous, livePath)
		return fmt.Errorf("import: update state: %w", err)
	}
	return nil
}

// rollbackImport undoes an Import whose final state write failed, so state.json
// (which still names previous active) matches the filesystem again: the new
// profile's dir — an otherwise inactive symlink to live — is removed along
// with the backup written under its name (left behind, a later profile of that
// name would inherit it), and previous is re-linked to live as it was before
// the import detached it. Best effort, logged.
func (m *Manager) rollbackImport(tool, name, previous, livePath string) {
	if err := os.RemoveAll(m.profileDir(tool, name)); err != nil {
		log.Printf("switcher: %s/%s: roll back import: remove profile: %v", tool, name, err)
	}
	if err := m.deleteProfileBackups(tool, name); err != nil {
		log.Printf("switcher: %s/%s: roll back import: delete backups: %v", tool, name, err)
	}
	if previous == "" || previous == name {
		return
	}
	if err := m.linkStoredToLive(tool, previous, livePath); err != nil {
		log.Printf("switcher: %s/%s: roll back import: re-link previously active profile: %v", tool, previous, err)
	}
}

// Rename renames a profile directory and updates state.json if it was active.
func (m *Manager) Rename(tool, oldName, newName string) error {
	if err := validateProfileName(oldName); err != nil {
		return err
	}
	if err := validateProfileName(newName); err != nil {
		return err
	}

	lock, lockErr := m.acquireLock()
	if lockErr != nil {
		return fmt.Errorf("acquire switcher lock: %w", lockErr)
	}
	defer lock.unlock()

	oldDir := m.profileDir(tool, oldName)
	if _, err := os.Stat(oldDir); err != nil {
		return fmt.Errorf("%w: %s/%s", ErrProfileNotFound, tool, oldName)
	}
	newDir := m.profileDir(tool, newName)
	if _, err := os.Stat(newDir); err == nil {
		return fmt.Errorf("%w: %s/%s", ErrProfileExists, tool, newName)
	}
	// Leftover backups under the new name belong to an earlier profile's
	// account; clear them so the moved backups below are the only ones.
	if err := m.deleteProfileBackups(tool, newName); err != nil {
		return fmt.Errorf("rename: clear leftover backups: %w", err)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return fmt.Errorf("rename profile: %w", err)
	}
	// Backups are keyed by profile name and are the recovery source
	// detachLinkedProfile uses, so they move with the profile (active or not).
	m.renameProfileBackups(tool, oldName, newName)
	state, err := m.loadState()
	if err != nil {
		return err
	}
	if state.Active[tool] == oldName {
		// Also record the active profile's current live bytes under the new
		// name — fresher than any moved backup. Best effort: the rename itself
		// has already happened.
		m.backupActiveLiveAs(tool, newName)
		state.Active[tool] = newName
		return m.saveState(state)
	}
	return nil
}

// Remove deletes a profile. If the profile is active and force is false,
// ErrActiveProfile is returned so the CLI/UI can confirm destructively.
// With force=true on the active profile, the live credentials file is also
// removed — same end-state as switching to an empty profile, which forces a
// fresh login on next harness start.
func (m *Manager) Remove(tool, name string, force bool) error {
	if err := validateProfileName(name); err != nil {
		return err
	}

	lock, lockErr := m.acquireLock()
	if lockErr != nil {
		return fmt.Errorf("acquire switcher lock: %w", lockErr)
	}
	defer lock.unlock()

	dir := m.profileDir(tool, name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%w: %s/%s", ErrProfileNotFound, tool, name)
	}

	state, err := m.loadState()
	if err != nil {
		return err
	}
	isActive := state.Active[tool] == name
	if isActive && !force {
		return fmt.Errorf("%w: %s/%s", ErrActiveProfile, tool, name)
	}

	// RemoveAll unlinks symlinks rather than following them, so removing an
	// active profile whose stored file links to the live credentials deletes
	// only the link here; the live file is handled explicitly below.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove profile: %w", err)
	}
	// Removal is the explicit destructive action: the profile's backups go
	// with it, so a later profile of the same name cannot inherit them.
	// Logged, not returned: the profile itself is already gone.
	if err := m.deleteProfileBackups(tool, name); err != nil {
		log.Printf("switcher: %s/%s: delete backups of removed profile: %v", tool, name, err)
	}

	if isActive {
		// Clear live credentials so the harness prompts for a fresh login.
		if livePath, pathErr := m.liveCredPath(tool); pathErr == nil {
			// Ignore remove error: the profile dir is already gone, so state.json
			// will record no active profile. If the live file lingers the user
			// simply re-imports; their credentials are never in a worse state than
			// before the Remove call.
			_ = os.Remove(livePath)
		}
		delete(state.Active, tool)
		if err := m.saveState(state); err != nil {
			return err
		}
	}
	return nil
}

// validateLiveCred validates the live credential file for tool, returning
// ErrInvalidCredentials (wrapped) on failure.
func validateLiveCred(tool, path string) error {
	var err error
	switch tool {
	case ToolClaude:
		_, err = usages.ValidateClaudeCredentials(path)
	case ToolCodex:
		_, _, err = usages.ValidateCodexCredentials(path)
	default:
		return fmt.Errorf("%w: %s", ErrNotEligible, tool)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidCredentials, err)
	}
	return nil
}
