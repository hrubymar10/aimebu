package switcher

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-json"
)

// switchedAwayFile records, per profile, when it last stopped being the active
// one. Background token maintenance leaves such a profile alone for a grace
// period: harness sessions started while it was active may still be running
// and refreshing the token they hold, so the stored snapshot is not yet safe to
// rotate. The file is advisory, separate from state.json (whose format is
// shared with other versions), and only ever read for that decision.
const switchedAwayFile = "switched_away.json"

func (m *Manager) switchedAwayPath() string { return filepath.Join(m.root, switchedAwayFile) }

func switchedAwayKey(tool, name string) string { return tool + "/" + name }

// readSwitchedAway parses the file. A missing file is an empty record, not an
// error; an unparseable one is reported so a writer can set it aside rather
// than overwrite it.
func (m *Manager) readSwitchedAway() (map[string]time.Time, error) {
	data, err := os.ReadFile(m.switchedAwayPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]time.Time{}, nil
		}
		return map[string]time.Time{}, err
	}
	var raw map[string]time.Time
	if err := json.Unmarshal(data, &raw); err != nil {
		return map[string]time.Time{}, err
	}
	if raw == nil {
		raw = map[string]time.Time{}
	}
	return raw, nil
}

// loadSwitchedAway is the read-only view used for listing: whatever cannot be
// read counts as no recorded time, and nothing on disk is touched.
func (m *Manager) loadSwitchedAway() map[string]time.Time {
	entries, _ := m.readSwitchedAway()
	return entries
}

// loadSwitchedAwayForWrite returns the entries a writer should extend. A file
// that exists but cannot be parsed (hand-edited, a single bad timestamp, a
// partial copy) holds other profiles' records too, including v1's, so it is
// moved aside to switched_away.json.corrupt (a numbered suffix if one is
// already there) and logged instead of being replaced by a one-entry file.
func (m *Manager) loadSwitchedAwayForWrite() map[string]time.Time {
	entries, err := m.readSwitchedAway()
	if err == nil {
		return entries
	}
	if os.IsNotExist(err) {
		return map[string]time.Time{}
	}
	dest := m.switchedAwayPath() + ".corrupt"
	for i := 1; ; i++ {
		if _, statErr := os.Lstat(dest); os.IsNotExist(statErr) {
			break
		}
		dest = fmt.Sprintf("%s.corrupt.%d", m.switchedAwayPath(), i)
	}
	if renameErr := os.Rename(m.switchedAwayPath(), dest); renameErr != nil {
		log.Printf("ERROR switcher: %s is unreadable (%v) and could not be set aside (%v); not recording this switch-away time", switchedAwayFile, err, renameErr)
		return nil
	}
	log.Printf("ERROR switcher: %s is unreadable (%v); moved it to %s and started a new record", switchedAwayFile, err, filepath.Base(dest))
	return map[string]time.Time{}
}

func (m *Manager) saveSwitchedAway(entries map[string]time.Time) {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err == nil {
		err = atomicWrite(m.switchedAwayPath(), append(data, '\n'), 0o600)
	}
	if err != nil {
		// Without the record the grace is not enforced for this profile.
		log.Printf("ERROR switcher: record switch-away time: %v", err)
	}
}

// recordSwitchAway stamps the profile as having just stopped being active. The
// caller holds switcher/.lock.
func (m *Manager) recordSwitchAway(tool, name string) {
	if name == "" {
		return
	}
	entries := m.loadSwitchedAwayForWrite()
	if entries == nil {
		return
	}
	entries[switchedAwayKey(tool, name)] = time.Now().UTC()
	m.saveSwitchedAway(entries)
}

// moveSwitchedAway follows a rename of an inactive profile; the old name's
// entry moves to the new name, and any stale entry already under the new name
// is replaced (or dropped when the old name had none).
func (m *Manager) moveSwitchedAway(tool, oldName, newName string) {
	entries := m.loadSwitchedAwayForWrite()
	if entries == nil {
		return
	}
	oldKey, newKey := switchedAwayKey(tool, oldName), switchedAwayKey(tool, newName)
	at, hadOld := entries[oldKey]
	_, hadNew := entries[newKey]
	if !hadOld && !hadNew {
		return
	}
	delete(entries, oldKey)
	delete(entries, newKey)
	if hadOld {
		entries[newKey] = at
	}
	m.saveSwitchedAway(entries)
}

// forgetSwitchedAway follows a removal (and a rename of the active profile,
// which has no entry of its own: any stale ones under either name go).
func (m *Manager) forgetSwitchedAway(tool string, names ...string) {
	entries := m.loadSwitchedAwayForWrite()
	if entries == nil {
		return
	}
	changed := false
	for _, name := range names {
		if _, ok := entries[switchedAwayKey(tool, name)]; ok {
			delete(entries, switchedAwayKey(tool, name))
			changed = true
		}
	}
	if changed {
		m.saveSwitchedAway(entries)
	}
}
