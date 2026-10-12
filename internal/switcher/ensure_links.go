package switcher

import (
	"errors"
	"fmt"
	"io/fs"
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

	// Unlocked per-tool check of the link state: the steady state (every
	// active profile already linked, or nothing to link to) must take no lock
	// and write nothing, since the server re-runs this periodically to link an
	// empty profile after its first login. Lock only when a tool needs work.
	var errs []error
	needed := false
	for _, tool := range Tools() {
		need, _, err := m.activeLinkNeeded(tool, pre.Active[tool])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tool, err))
		}
		needed = needed || need
	}
	if !needed {
		return errors.Join(errs...)
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
	errs = nil
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

// activeLinkNeeded reports, without writing or locking, whether the active
// profile name of tool needs its stored file linked to live, and the live path
// to link to. Nothing is needed for no active profile, a stale pointer, an
// absent or corrupt live file (never link to those), or an already-correct
// link — the cheap link check comes first so the steady state does not even
// read the live file.
func (m *Manager) activeLinkNeeded(tool, name string) (bool, string, error) {
	if name == "" {
		return false, "", nil
	}
	if err := validateProfileName(name); err != nil {
		return false, "", fmt.Errorf("corrupt state: active profile name invalid: %w", err)
	}
	if info, err := os.Stat(m.profileDir(tool, name)); err != nil || !info.IsDir() {
		return false, "", nil // stale pointer: doctor reports it; nothing to link into
	}
	livePath, err := m.liveCredPath(tool)
	if err != nil {
		return false, "", err
	}
	absLive, err := filepath.Abs(livePath)
	if err != nil {
		return false, "", err
	}

	storedPath := m.profileCredPath(tool, name)
	info, err := os.Lstat(storedPath)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		if target, readErr := os.Readlink(storedPath); readErr == nil && target == absLive {
			return false, "", nil // already correct
		}
	case err == nil && info.IsDir():
		return false, "", fmt.Errorf("stored credential path %s is a directory", storedPath)
	case err != nil && !os.IsNotExist(err):
		return false, "", err
	}

	if _, err := os.Stat(livePath); err != nil {
		return false, "", nil // absent live: an empty active profile, nothing to link to
	}
	if err := validateLiveCred(tool, livePath); err != nil {
		return false, "", nil // never link a profile to corrupt live credentials
	}
	return true, livePath, nil
}

// ensureActiveLink links the active profile name of tool when
// activeLinkNeeded says so. Callers hold the switcher lock.
func (m *Manager) ensureActiveLink(tool, name string) error {
	need, livePath, err := m.activeLinkNeeded(tool, name)
	if err != nil || !need {
		return err
	}
	storedPath := m.profileCredPath(tool, name)

	// Safety copy of whatever the stored path holds, even when it equals live:
	// like Switch's link-time backup, it is this profile's last-known
	// credentials, which detachLinkedProfile restores if live is later replaced
	// from outside a switch. Only a stored path that does not exist (missing
	// file — the empty profile after its first login — or a dangling link,
	// which reads as not-exist) has nothing to keep and skips the backup. Any
	// other read error (permissions, I/O) aborts: replacing bytes we could not
	// back up would discard them.
	stored, readErr := os.ReadFile(storedPath)
	switch {
	case readErr == nil:
		if err := m.writeUniqueBackup(tool, name, stored); err != nil {
			return fmt.Errorf("backup stored credentials: %w", err)
		}
	case errors.Is(readErr, fs.ErrNotExist):
		// nothing stored to keep
	default:
		return fmt.Errorf("read stored credentials for backup: %w", readErr)
	}
	return m.linkStoredToLive(tool, name, livePath)
}
