package switcher

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
)

// ErrLinkedProfile is returned when a profile's stored credential is still a
// link to a live file (the pre-migration layout). Importing a login would write
// through the link into the live file, so it is refused.
var ErrLinkedProfile = credtarget.ErrLinkedProfile

// ImportLogin stores a login's credentials as a regular file in the profile
// store, under the switcher lock. The caller holds the account's maintenance
// lock for a replaced profile (account lock first, switcher lock second, as in
// every other path). Nothing is written through a link, a replaced credential
// is backed up first, and repeating the same import is harmless.
func (m *Manager) ImportLogin(in credtarget.LoginImport) error {
	if err := validateProfileName(in.Profile); err != nil {
		return err
	}
	if profileCredFilename(in.Tool) == "" {
		return fmt.Errorf("unknown tool %q", in.Tool)
	}
	if len(bytes.TrimSpace(in.Full)) == 0 {
		return errors.New("import login: empty credential")
	}
	lock, err := m.acquireLock()
	if err != nil {
		return fmt.Errorf("acquire switcher lock: %w", err)
	}
	defer lock.unlock()

	state, err := m.loadState()
	if err != nil {
		return err
	}
	dir := m.profileDir(in.Tool, in.Profile)
	credPath := m.profileCredPath(in.Tool, in.Profile)
	_, statErr := os.Stat(dir)
	switch {
	case in.Create && statErr == nil:
		return fmt.Errorf("%w: %w: %s/%s", credtarget.ErrProfileExists, ErrProfileExists, in.Tool, in.Profile)
	case !in.Create && statErr != nil:
		return fmt.Errorf("%w: %w: %s/%s", credtarget.ErrProfileNotFound, ErrProfileNotFound, in.Tool, in.Profile)
	}
	if info, err := os.Lstat(credPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s/%s", ErrLinkedProfile, in.Tool, in.Profile)
	}
	if in.Create {
		if err := m.deleteProfileBackups(in.Tool, in.Profile); err != nil {
			return fmt.Errorf("import login: clear leftover backups: %w", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	// A replaced credential is kept before it is overwritten, so nothing a
	// profile held is ever lost to an import.
	if old, err := os.ReadFile(credPath); err == nil && len(bytes.TrimSpace(old)) > 0 && !bytes.Equal(old, in.Full) {
		if err := m.writeUniqueBackup(in.Tool, in.Profile, old); err != nil {
			return fmt.Errorf("import login: back up the replaced credential: %w", err)
		}
	}
	if err := atomicWrite(credPath, in.Full, 0o600); err != nil {
		if in.Create {
			_ = os.RemoveAll(dir)
		}
		return fmt.Errorf("import login: %w", err)
	}
	if in.Tool == ToolClaude && in.Email != "" {
		m.writeProfileMeta(in.Tool, in.Profile, profileMeta{Email: in.Email})
	}
	if in.SetActive && state.Active[in.Tool] != in.Profile {
		previous := state.Active[in.Tool]
		state.Active[in.Tool] = in.Profile
		if err := m.saveState(state); err != nil {
			return fmt.Errorf("import login: update state: %w", err)
		}
		if previous != "" {
			m.recordSwitchAway(in.Tool, previous)
		}
	}
	return nil
}

// ProfileNames lists the profile directories of a tool, sorted.
func (m *Manager) ProfileNames(tool string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.profilesRoot(), tool))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// StoredCredential reads a profile's stored credential bytes. A missing file
// returns os.ErrNotExist. A link to a live file (the pre-migration layout)
// returns ErrLinkedProfile: those bytes belong to the live file, not to the
// store, and treating them as the profile's own would let a caller strip the
// only copy of a refresh token.
func (m *Manager) StoredCredential(tool, name string) ([]byte, error) {
	if err := validateProfileName(name); err != nil {
		return nil, err
	}
	path := m.profileCredPath(tool, name)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s/%s", ErrLinkedProfile, tool, name)
	}
	return os.ReadFile(path)
}

// ProfileEmail is the Claude account email recorded beside a profile ("" when
// none was captured).
func (m *Manager) ProfileEmail(tool, name string) string {
	if validateProfileName(name) != nil {
		return ""
	}
	return m.readProfileMeta(tool, name).Email
}

// loginRescueName names a rescued login: a leading underscore can never be a
// profile's backup (those are <name>-<digits>.json), so it never collides with
// one and is never pruned as one.
func loginRescueName(at time.Time) string {
	return "_login-" + at.UTC().Format("20060102T150405.000000000Z") + ".json"
}

// SaveLoginRescue persists a full credential before it is imported or stripped
// from its target, so a crash between the steps loses nothing. An identical
// existing rescue is reused instead of minting another. It returns the path.
func (m *Manager) SaveLoginRescue(tool string, data []byte, at time.Time) (string, error) {
	if profileCredFilename(tool) == "" {
		return "", fmt.Errorf("unknown tool %q", tool)
	}
	dir := filepath.Join(m.backupsRoot(), tool)
	if existing, err := m.findLoginRescue(dir, data); err != nil {
		return "", err
	} else if existing != "" {
		return existing, nil
	}
	path := filepath.Join(dir, loginRescueName(at))
	for {
		if _, err := os.Lstat(path); err != nil {
			break
		}
		at = at.Add(time.Nanosecond)
		path = filepath.Join(dir, loginRescueName(at))
	}
	if err := atomicWrite(path, data, 0o600); err != nil {
		return "", err
	}
	m.pruneLoginRescues(dir)
	return path, nil
}

// maxLoginRescueAge bounds how long a rescued login is kept. A rescue only has
// to outlive the window in which an import can fail part-way (minutes), but it
// holds a full credential, so a stale one is a liability rather than a safety
// net.
const maxLoginRescueAge = 24 * time.Hour

// deleteLoginRescuesFor removes the rescued logins that hold one of the given
// refresh tokens. Removing a profile is the explicit destructive action: its
// credentials must not survive in a rescue copy of the login that created it.
func (m *Manager) deleteLoginRescuesFor(tool string, refreshTokens map[string]bool) {
	if len(refreshTokens) == 0 {
		return
	}
	dir := filepath.Join(m.backupsRoot(), tool)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "_login-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if cred, err := credtarget.ParseFull(tool, data); err == nil && refreshTokens[cred.RefreshToken] {
			if err := os.Remove(path); err != nil {
				log.Printf("switcher: %s: delete rescued login of a removed profile: %v", tool, err)
			}
		}
	}
}

// refreshTokensOf lists the refresh tokens held by a profile's stored credential
// and its backups.
func (m *Manager) refreshTokensOf(tool, name string) map[string]bool {
	tokens := map[string]bool{}
	add := func(path string) {
		if data, err := os.ReadFile(path); err == nil {
			if cred, err := credtarget.ParseFull(tool, data); err == nil {
				tokens[cred.RefreshToken] = true
			}
		}
	}
	add(m.profileCredPath(tool, name))
	if backups, err := m.profileBackups(tool, name); err == nil {
		for _, backup := range backups {
			add(backup.path)
		}
	}
	if rotated, err := m.profileRotatedBackups(tool, name); err == nil {
		for _, path := range rotated {
			add(path)
		}
	}
	return tokens
}

// maxLoginRescues bounds the rescued logins kept per tool. They hold full
// credentials, so they must not pile up; the oldest go first.
const maxLoginRescues = 5

func (m *Manager) findLoginRescue(dir string, data []byte) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || len(entry.Name()) < 7 || entry.Name()[:7] != "_login-" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
			return path, nil
		}
	}
	return "", nil
}

func (m *Manager) pruneLoginRescues(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && len(entry.Name()) >= 7 && entry.Name()[:7] == "_login-" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names) // the timestamp sorts oldest first
	for len(names) > 0 {
		oldest := filepath.Join(dir, names[0])
		info, err := os.Stat(oldest)
		tooMany := len(names) > maxLoginRescues
		tooOld := err == nil && time.Since(info.ModTime()) > maxLoginRescueAge
		if !tooMany && !tooOld {
			break
		}
		_ = os.Remove(oldest)
		names = names[1:]
	}
}

// tokenCopiesFor reports whether tool runs the token-copy model (the marker is
// per tool: a claude-only migration leaves codex on the link-based layout).
func (m *Manager) tokenCopiesFor(tool string) bool { return m.TokenCopiesFor(tool) }

// importTokenCopies is Import for the token-copy model. The live file holds a
// full login the user has just made (a login the reconciler has not filed yet),
// and this stores it under the name the user chose: as a regular 0600 file in
// the profile store, never a link, and then rewrites the live file token-only
// so no session keeps a refresh token the server also holds.
//
// The order is what keeps a crash safe at every point:
//
//  1. a rescue copy of the login and a backup are saved first;
//  2. the stored credential is written as a regular file;
//  3. the active pointer moves, so the live file and the state agree about
//     whose login it holds;
//  4. only then is the live file stripped, through the replacing hook (it is
//     the one write allowed to overwrite a file that still holds a refresh
//     token, and only after steps 1-3).
//
// A failure before step 3 undoes the profile and leaves the live file exactly as
// it was. A failure at step 4 leaves a consistent state (the profile is stored
// and active, the live file still carries the login) that the reconciler's
// import path finishes within a minute, so it is logged but not an error.
// The caller holds the switcher lock.
func (m *Manager) importTokenCopies(tool, name, livePath string, liveData []byte, state State, previous string) error {
	replace := m.copyHooks.ReplaceActive
	if replace == nil {
		return errors.New("import refused: token-copy fan-out is not wired")
	}
	dir := m.profileDir(tool, name)
	if _, statErr := os.Stat(dir); statErr == nil {
		return fmt.Errorf("%w: %s/%s", ErrProfileExists, tool, name)
	}
	if _, err := m.SaveLoginRescue(tool, liveData, time.Now()); err != nil {
		return fmt.Errorf("import: save a rescue copy of the login: %w", err)
	}
	if err := m.deleteProfileBackups(tool, name); err != nil {
		return fmt.Errorf("import: clear leftover backups: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	undo := func() {
		_ = os.RemoveAll(dir)
		_ = m.deleteProfileBackups(tool, name)
	}
	if err := m.writeUniqueBackup(tool, name, liveData); err != nil {
		undo()
		return fmt.Errorf("import: backup: %w", err)
	}
	if tool == ToolClaude {
		m.writeProfileMeta(tool, name, profileMeta{Email: m.claudeEmailFromDotJSON()})
	}
	storePath := m.profileCredPath(tool, name)
	if err := atomicWrite(storePath, liveData, 0o600); err != nil {
		undo()
		return fmt.Errorf("import credentials: %w", err)
	}
	if err := validateStoredCred(tool, storePath); err != nil {
		undo()
		return fmt.Errorf("%w: stored credentials failed validation: %s", ErrInvalidCredentials, err)
	}
	state.Active[tool] = name
	if err := m.saveState(state); err != nil {
		undo()
		return fmt.Errorf("import: update state: %w", err)
	}
	if previous != "" && previous != name {
		m.recordSwitchAway(tool, previous)
	}
	if err := replace(tool, liveData); err != nil {
		log.Printf("switcher: ERROR %s/%s imported and active, but the live file could not be rewritten token-only yet (the reconciler will strip it): %v", tool, name, err)
	}
	return nil
}
