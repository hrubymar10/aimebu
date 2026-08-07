package switcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/goccy/go-json"
)

const stateVersion = 1

// State is the single source of truth for all switcher runtime state. It is
// read and written atomically; every mutator that touches this file holds
// switcher/.lock for the entire read-modify-write cycle.
type State struct {
	Version int               `json:"version"`
	Enabled bool              `json:"enabled"`
	Active  map[string]string `json:"active"` // tool → profile name, absent = none active
}

func emptyState() State {
	return State{Version: stateVersion, Active: map[string]string{}}
}

func (m *Manager) statePath() string { return filepath.Join(m.root, "state.json") }

// loadState reads state.json. If the file is absent a fresh empty state is
// returned. An unknown version is rejected loudly rather than parsed
// optimistically — a future version of the file would be silently corrupted.
func (m *Manager) loadState() (State, error) {
	data, err := os.ReadFile(m.statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyState(), nil
		}
		return State{}, fmt.Errorf("read state.json: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parse state.json: %w", err)
	}
	if s.Version != stateVersion {
		return State{}, fmt.Errorf("state.json: unsupported version %d (expected %d)", s.Version, stateVersion)
	}
	if s.Active == nil {
		s.Active = map[string]string{}
	}
	return s, nil
}

// saveState writes state.json atomically (temp file + rename) with mode 0600.
func (m *Manager) saveState(s State) error {
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(m.statePath(), data, 0o600)
}

// atomicWrite writes data to a temp file in the same directory, then renames it
// to path. A crash mid-write can never leave a half-written file at path.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// fileLock wraps a flock'd file descriptor.
type fileLock struct {
	f *os.File
}

// acquireLock creates the switcher .lock file and acquires an exclusive flock.
func (m *Manager) acquireLock() (*fileLock, error) {
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(m.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return closeErr
}
