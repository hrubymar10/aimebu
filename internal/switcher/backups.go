package switcher

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxProfileBackups caps how many backups/<tool>/<name>-<unix>.json files are
// kept per profile. Backups are written on every switch away, link-in, import,
// rename of the active profile and link migration, so without a cap they grow
// forever. The newest valid backup is always kept on top of the cap, because
// detachLinkedProfile restores from it.
const maxProfileBackups = 10

// backupFile is one backup of a profile, identified by its <unix> suffix.
type backupFile struct {
	ts   int64
	path string
}

// profileBackups lists name's backups under backups/<tool>/, newest first.
// Only files named exactly <name>-<digits>.json match, so profile "a" never
// sees the backups of a profile named "a-b".
func (m *Manager) profileBackups(tool, name string) ([]backupFile, error) {
	dir := filepath.Join(m.backupsRoot(), tool)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var backups []backupFile
	prefix := name + "-"
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		digits, ok := strings.CutSuffix(rest, ".json")
		if !ok || digits == "" || strings.Trim(digits, "0123456789") != "" {
			continue
		}
		ts, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			continue
		}
		backups = append(backups, backupFile{ts, filepath.Join(dir, e.Name())})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].ts > backups[j].ts })
	return backups, nil
}

// writeUniqueBackup writes data to backups/<tool>/<name>-<unix>.json, stepping
// the timestamp forward rather than overwriting an existing backup, then
// prunes that profile's backups to the cap. Every backup writer goes through
// here.
//
// When data equals the profile's newest existing backup the write is skipped
// (nil is returned): the bytes are already kept, and a retried operation — a
// link that keeps failing on the server's periodic pass, say — must not mint a
// new copy every time and evict older, different backups under the cap. No
// caller needs a fresh file: rollbacks restore from their in-memory bytes.
func (m *Manager) writeUniqueBackup(tool, name string, data []byte) error {
	ts := time.Now().Unix()
	if existing, err := m.profileBackups(tool, name); err == nil && len(existing) > 0 {
		if newest, readErr := os.ReadFile(existing[0].path); readErr == nil && bytes.Equal(newest, data) {
			return nil
		}
		// Stay newest by suffix even if an earlier collision stepped a
		// backup's timestamp past the clock.
		if existing[0].ts >= ts {
			ts = existing[0].ts + 1
		}
	}
	path := m.backupPath(tool, name, ts)
	for {
		if _, err := os.Lstat(path); err != nil {
			break // free (or unstat-able, which atomicWrite will report)
		}
		ts++
		path = m.backupPath(tool, name, ts)
	}
	if err := atomicWrite(path, data, 0o600); err != nil {
		return err
	}
	m.pruneBackups(tool, name)
	return nil
}

// pruneBackups deletes name's backups beyond the newest maxProfileBackups,
// except the newest valid one, which is kept even when it falls outside the
// cap. Best effort: failures are logged and never fail the caller.
func (m *Manager) pruneBackups(tool, name string) {
	backups, err := m.profileBackups(tool, name)
	if err != nil {
		log.Printf("switcher: %s/%s: list backups for pruning: %v", tool, name, err)
		return
	}
	if len(backups) <= maxProfileBackups {
		return
	}
	keepValid := ""
	for _, b := range backups {
		if validateStoredCred(tool, b.path) == nil {
			keepValid = b.path
			break
		}
	}
	for _, b := range backups[maxProfileBackups:] {
		if b.path == keepValid {
			continue
		}
		if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
			log.Printf("switcher: %s/%s: prune backup %s: %v", tool, name, filepath.Base(b.path), err)
		}
	}
}

// newestValidBackup returns the bytes of name's newest backup that validates
// as stored credentials.
func (m *Manager) newestValidBackup(tool, name string) ([]byte, bool) {
	backups, err := m.profileBackups(tool, name)
	if err != nil {
		return nil, false
	}
	for _, b := range backups {
		if validateStoredCred(tool, b.path) != nil {
			continue
		}
		if data, err := os.ReadFile(b.path); err == nil {
			return data, true
		}
	}
	return nil, false
}

// renameProfileBackups moves oldName's backups (exactly <oldName>-<digits>.json)
// to <newName>-<digits>.json. A destination that already exists — a backup
// left by an earlier profile of that name — is never overwritten: the
// timestamp steps forward to the next free name instead. Best effort: failures
// are logged, since the profile rename itself has already happened. Callers
// hold the switcher lock.
func (m *Manager) renameProfileBackups(tool, oldName, newName string) {
	backups, err := m.profileBackups(tool, oldName)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("switcher: %s/%s: list backups to rename: %v", tool, oldName, err)
		}
		return
	}
	// Oldest first, so stepping past a collision never makes an older moved
	// backup look newer than a later one.
	for i := len(backups) - 1; i >= 0; i-- {
		b := backups[i]
		ts := b.ts
		for {
			// os.Link refuses an existing destination, so nothing is overwritten.
			err = os.Link(b.path, m.backupPath(tool, newName, ts))
			if !os.IsExist(err) {
				break
			}
			ts++
		}
		if err != nil {
			log.Printf("switcher: %s/%s: move backup %s to %s: %v", tool, oldName, filepath.Base(b.path), newName, err)
			continue
		}
		if err := os.Remove(b.path); err != nil {
			log.Printf("switcher: %s/%s: remove moved backup %s: %v", tool, oldName, filepath.Base(b.path), err)
		}
	}
	m.pruneBackups(tool, newName)
}

// deleteProfileBackups removes every backup of name (exactly
// <name>-<digits>.json). Backups are the recovery source detachLinkedProfile
// restores from by name, so a profile created under a name that once belonged
// to another account must not inherit that account's backups. Callers hold the
// switcher lock.
func (m *Manager) deleteProfileBackups(tool, name string) error {
	backups, err := m.profileBackups(tool, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, b := range backups {
		if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
