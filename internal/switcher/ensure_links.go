package switcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureActiveLinks brings existing installs up to the active-profile symlink
// invariant without a switch. Installs from before active profiles were linked
// still hold a plain (and, once the harness has rotated tokens, stale) copy in
// the active profile's stored file until the next switch away and back — and
// asking users to switch just to fix that would swap the live login under any
// running harness session.
//
// Under the switcher flock, for each tool with an active profile — and only
// when the switcher is enabled and the live credential file exists and
// validates — the active profile's stored file is replaced by a symlink to the
// live file. The stored bytes are first saved to
// backups/<tool>/<name>-<unix>.json, so a stale copy is never discarded
// silently. An already-correct link is a no-op, so this is safe for the server
// to run at start and periodically. Inactive profiles are never touched.
//
// Per-tool failures are collected and returned joined; callers log them and
// carry on — nothing here is worth failing server startup over.
func (m *Manager) EnsureActiveLinks() error {
	// Unlocked pre-check: acquiring the lock creates the switcher root and
	// .lock, which a never-used or disabled switcher must not get just because
	// the server started. loadState only reads.
	pre, err := m.loadState()
	if err != nil {
		return err
	}
	if !pre.Enabled || !anyActive(pre) {
		return nil
	}

	lock, err := m.acquireLock()
	if err != nil {
		return fmt.Errorf("acquire switcher lock: %w", err)
	}
	defer func() { _ = lock.unlock() }()

	state, err := m.loadState()
	if err != nil {
		return err
	}
	if !state.Enabled { // re-check under the lock
		return nil
	}
	var errs []error
	for _, tool := range Tools() {
		if err := m.ensureActiveLink(tool, state.Active[tool]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tool, err))
		}
	}
	return errors.Join(errs...)
}

func anyActive(s State) bool {
	for _, name := range s.Active {
		if name != "" {
			return true
		}
	}
	return false
}

func (m *Manager) ensureActiveLink(tool, name string) error {
	if name == "" {
		return nil
	}
	if err := validateProfileName(name); err != nil {
		return fmt.Errorf("corrupt state: active profile name invalid: %w", err)
	}
	if info, err := os.Stat(m.profileDir(tool, name)); err != nil || !info.IsDir() {
		return nil // stale pointer: doctor reports it; nothing to link into
	}
	livePath, err := m.liveCredPath(tool)
	if err != nil {
		return err
	}
	if _, err := os.Stat(livePath); err != nil {
		return nil // absent live: an empty active profile, nothing to link to
	}
	if err := validateLiveCred(tool, livePath); err != nil {
		return nil // never link a profile to corrupt live credentials
	}
	absLive, err := filepath.Abs(livePath)
	if err != nil {
		return err
	}

	storedPath := m.profileCredPath(tool, name)
	info, err := os.Lstat(storedPath)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		if target, readErr := os.Readlink(storedPath); readErr == nil && target == absLive {
			return nil // already correct
		}
	case err == nil && info.IsDir():
		return fmt.Errorf("stored credential path %s is a directory", storedPath)
	case err != nil && !os.IsNotExist(err):
		return err
	}

	// Safety copy of whatever the stored path holds, even when it equals live:
	// like Switch's link-time backup, it is this profile's last-known
	// credentials, which detachLinkedProfile restores if live is later replaced
	// from outside a switch. An unreadable stored path (missing file, dangling
	// link) has nothing to keep.
	if stored, readErr := os.ReadFile(storedPath); readErr == nil {
		if err := m.writeUniqueBackup(tool, name, stored); err != nil {
			return fmt.Errorf("backup stored credentials: %w", err)
		}
	}
	return m.linkStoredToLive(tool, name, livePath)
}
