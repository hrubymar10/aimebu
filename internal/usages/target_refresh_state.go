package usages

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// refreshStateFileName lives beside the rescue files under the switcher root.
// It is the server's memory across restarts of the two facts the spend-once rule
// depends on: which refresh tokens may have been spent, and which accounts need
// a human.
const refreshStateFileName = "refresh-state.json"

// refreshStateLockName is the flock that serialises read-modify-write cycles of
// the state file across processes (the server and `aimebu switcher use` both run
// the refresher's code).
const refreshStateLockName = "refresh-state.lock"

// refreshTempDirName holds the throwaway credential copies a refresh run spends,
// under the switcher root so they survive a reboot (recovery reads them) and stay
// in a 0700 directory of ours instead of the shared system temp dir.
const refreshTempDirName = "refresh-tmp"

// refreshTempMinAge is how old an unreferenced temp dir must be before the sweep
// removes it: a run that has not journaled yet (or another process's run) is
// younger than this.
const refreshTempMinAge = 15 * time.Minute

// Journal phases of one refresh run.
const (
	// journalSpending is written before the CLI runs: from here on the stored
	// refresh token may have been consumed at the provider.
	journalSpending = "spending"
	// journalRotated is written once the new credentials exist on disk (rescue
	// file) but are not yet committed to the profile store.
	journalRotated = "rotated"
)

// refreshJournalEntry records one run in flight. At most one exists per
// (tool, profile); it is removed when the run ends in a known outcome.
type refreshJournalEntry struct {
	Tool      string    `json:"tool"`
	Name      string    `json:"name"`
	Phase     string    `json:"phase"`
	SpentHash string    `json:"spent_hash"`
	Rescue    string    `json:"rescue,omitempty"`
	TempDir   string    `json:"temp_dir,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// refreshAccountEntry is a terminal account state. It is keyed by the hash of
// the refresh token it applies to, so a re-login (a new token) clears it by
// itself: the new account identity simply has no entry.
type refreshAccountEntry struct {
	Tool      string    `json:"tool"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	SpentHash string    `json:"spent_hash"`
	Since     time.Time `json:"since"`
}

type refreshStateData struct {
	Version  int                            `json:"version"`
	Journal  map[string]refreshJournalEntry `json:"journal"`
	Accounts map[string]refreshAccountEntry `json:"accounts"`
}

func newRefreshStateData() refreshStateData {
	return refreshStateData{Version: 1, Journal: map[string]refreshJournalEntry{}, Accounts: map[string]refreshAccountEntry{}}
}

// stateNotice is something the store wants a human to hear about (a save that
// failed, a state file that had to be set aside). The refresher drains them into
// its alert path; the store itself only logs.
type stateNotice struct {
	Kind   string
	Detail string
}

// refreshStateStore persists refreshStateData. With an empty root (tests, no
// switcher root) it keeps the state in memory only.
//
// The file is shared by every process that runs the refresher's code, so the
// in-memory copy is never trusted across calls: reads re-read the file, and every
// change is a read-modify-write under a flock, so one process can neither miss a
// terminal state another set nor overwrite its journal entry. A change whose save
// failed is kept (unsaved) and re-applied on top of whatever is read next, so a
// spend-once fact is not forgotten just because the disk refused it once.
type refreshStateStore struct {
	root string
	path string
	mu   sync.Mutex
	data refreshStateData // memory-only mode, and the last view otherwise

	unsaved     []func(*refreshStateData) // changes not yet on disk, oldest first
	notices     []stateNotice
	repaired    bool
	lastReadErr string // the read error last logged, so a persistent one is logged once

	write func(path string, data []byte, mode os.FileMode) error // test seam
}

func newRefreshStateStore(root string) *refreshStateStore {
	s := &refreshStateStore{root: root, data: newRefreshStateData(), write: atomicWriteBytes}
	if root != "" {
		s.path = filepath.Join(root, refreshStateFileName)
	}
	return s
}

func journalKey(tool, name string) string { return tool + "/" + name }

func accountKey(tool, spentHash string) string { return tool + "/" + spentHash }

// readFile parses the state file. A missing file is an empty state. A file that
// cannot be parsed, or has another version, comes back as an error: the caller
// decides whether to set it aside (a writer) or only to report it (a reader).
func (s *refreshStateStore) readFile() (refreshStateData, error) {
	d := newRefreshStateData()
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	var loaded refreshStateData
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return d, fmt.Errorf("unparseable: %w", err)
	}
	if loaded.Version != 1 {
		return d, fmt.Errorf("unsupported version %d", loaded.Version)
	}
	if loaded.Journal != nil {
		d.Journal = loaded.Journal
	}
	if loaded.Accounts != nil {
		d.Accounts = loaded.Accounts
	}
	return d, nil
}

// view returns the current state: the file with the unsaved changes applied. A
// read problem never wipes anything: it falls back to the last view.
func (s *refreshStateStore) viewLocked() refreshStateData {
	if s.path == "" {
		return s.data
	}
	d, err := s.readFile()
	if err != nil {
		if msg := err.Error(); msg != s.lastReadErr {
			s.lastReadErr = msg
			log.Printf("usages: ERROR refresh state %s: %v (using the last known state)", s.path, err)
		}
		d = s.data
	} else {
		s.lastReadErr = ""
	}
	for _, apply := range s.unsaved {
		apply(&d)
	}
	s.data = d
	return d
}

// quarantineLocked sets an unreadable state file aside, once, so the next save
// does not silently overwrite evidence of what may have been spent. It returns
// the new name.
func (s *refreshStateStore) quarantineLocked(reason error) string {
	base := s.path + ".corrupt"
	target := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			break
		}
		target = fmt.Sprintf("%s.%d", base, i)
	}
	if err := os.Rename(s.path, target); err != nil {
		log.Printf("usages: ERROR refresh state %s is %v and could not be set aside: %v", s.path, reason, err)
		return ""
	}
	detail := fmt.Sprintf("the refresh state file was %v; it was set aside as %s and the state starts empty, so tokens spent before this point may be tried again", reason, filepath.Base(target))
	log.Printf("usages: ERROR refresh state: %s", detail)
	s.notices = append(s.notices, stateNotice{Kind: "state_quarantined", Detail: detail})
	return target
}

// repairLocked runs once, before the first write: it sets an unreadable file
// aside and removes temp dirs nothing refers to any more.
func (s *refreshStateStore) repairLocked() {
	if s.repaired || s.path == "" {
		return
	}
	s.repaired = true
	if _, err := s.readFile(); err != nil && !os.IsNotExist(err) {
		if _, statErr := os.Stat(s.path); statErr == nil {
			s.quarantineLocked(err)
		}
	}
	referenced := map[string]bool{}
	for _, e := range s.viewLocked().Journal {
		if e.TempDir != "" {
			referenced[filepath.Clean(e.TempDir)] = true
		}
	}
	sweepRefreshTemp(s.root, referenced, time.Now())
}

// mutate is one read-modify-write cycle under the cross-process flock. fn is
// kept (unsaved) when the save fails.
func (s *refreshStateStore) mutate(fn func(*refreshStateData)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		fn(&s.data)
		return nil
	}
	lock, err := acquireNamedLock(s.root, refreshStateLockName)
	if err != nil {
		s.unsaved = append(s.unsaved, fn)
		s.noteSaveFailure(fmt.Errorf("lock: %w", err))
		fn(&s.data)
		return err
	}
	defer lock.Unlock()
	s.repairLocked()
	d := s.viewLocked()
	fn(&d)
	raw, err := json.MarshalIndent(d, "", "  ")
	if err == nil {
		err = s.write(s.path, append(raw, '\n'), 0o600)
	}
	if err != nil {
		s.unsaved = append(s.unsaved, fn)
		s.noteSaveFailure(err)
		s.data = d
		return err
	}
	s.unsaved = nil
	s.data = d
	return nil
}

const maxStateBacklog = 256

func (s *refreshStateStore) noteSaveFailure(err error) {
	if len(s.unsaved) > maxStateBacklog {
		s.unsaved = s.unsaved[len(s.unsaved)-maxStateBacklog:]
	}
	detail := fmt.Sprintf("cannot save the refresh state %s: %v; the change is kept in memory and retried on the next write", s.path, err)
	log.Printf("usages: ERROR refresh state: %s", detail)
	if len(s.notices) >= 16 {
		return // already shouting; the log has every one
	}
	s.notices = append(s.notices, stateNotice{Kind: "state_save_failed", Detail: detail})
}

func (s *refreshStateStore) setJournal(e refreshJournalEntry) error {
	return s.mutate(func(d *refreshStateData) { d.Journal[journalKey(e.Tool, e.Name)] = e })
}

// clearJournal removes the entry for (tool, name) and, with it, the throwaway
// credential dir the run left behind (only a direct child of our own temp root).
// The error is the save error; the in-memory view is cleared either way.
func (s *refreshStateStore) clearJournal(tool, name string) error {
	key := journalKey(tool, name)
	var gone refreshJournalEntry
	var had bool
	err := s.mutate(func(d *refreshStateData) {
		gone, had = d.Journal[key]
		delete(d.Journal, key)
	})
	if had && gone.TempDir != "" {
		removeRefreshTempDir(s.root, gone.TempDir)
	}
	return err
}

func (s *refreshStateStore) journal() []refreshJournalEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.viewLocked()
	out := make([]refreshJournalEntry, 0, len(d.Journal))
	for _, e := range d.Journal {
		out = append(out, e)
	}
	return out
}

func (s *refreshStateStore) setAccount(e refreshAccountEntry) error {
	return s.mutate(func(d *refreshStateData) { d.Accounts[accountKey(e.Tool, e.SpentHash)] = e })
}

func (s *refreshStateStore) account(tool, spentHash string) (refreshAccountEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.viewLocked().Accounts[accountKey(tool, spentHash)]
	return e, ok
}

// drainNotices returns and forgets what the store wants reported.
func (s *refreshStateStore) drainNotices() []stateNotice {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.notices
	s.notices = nil
	return out
}

// refreshTempRoot is where a refresh run's throwaway credential dirs live.
func refreshTempRoot(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, refreshTempDirName)
}

// makeRefreshTempDir creates a 0700 dir for one run under root's refresh-tmp
// (the system temp dir when root is empty, as in tests).
func makeRefreshTempDir(root, prefix string) (string, error) {
	parent := refreshTempRoot(root)
	if parent != "" {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", parent, err)
		}
	}
	return os.MkdirTemp(parent, prefix)
}

// removeRefreshTempDir deletes dir only when it is a direct child of root's
// refresh-tmp: a journal entry is data from a file, and must not be able to point
// the cleanup at an arbitrary path.
func removeRefreshTempDir(root, dir string) {
	parent := refreshTempRoot(root)
	if parent == "" || dir == "" {
		return
	}
	dir = filepath.Clean(dir)
	if filepath.Dir(dir) != filepath.Clean(parent) || strings.HasPrefix(filepath.Base(dir), ".") {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("usages: refresh temp dir %s: %v", dir, err)
	}
}

// sweepRefreshTemp removes the temp dirs under root's refresh-tmp that no journal
// entry refers to and that are old enough not to belong to a run still starting.
func sweepRefreshTemp(root string, referenced map[string]bool, now time.Time) {
	parent := refreshTempRoot(root)
	if parent == "" {
		return
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		if referenced[path] {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < refreshTempMinAge {
			continue
		}
		removeRefreshTempDir(root, path)
	}
}
