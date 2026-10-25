package usages

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// The one-hour floor survives restarts and is keyed by the account's refresh
// token hash, so aliases of the same login share it.
type warmupLastStore struct {
	mu   sync.Mutex
	path string
	last map[string]time.Time
}

func newWarmupLastStore(root string) *warmupLastStore {
	s := &warmupLastStore{last: map[string]time.Time{}}
	if root == "" {
		return s
	}
	s.path = filepath.Join(root, "warmup-last.json")
	data, err := os.ReadFile(s.path)
	if err == nil {
		var file struct {
			Version int                  `json:"version"`
			Last    map[string]time.Time `json:"last"`
		}
		if json.Unmarshal(data, &file) == nil && file.Version == 1 && file.Last != nil {
			s.last = file.Last
		}
	}
	return s
}

func (s *warmupLastStore) get(key string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[key]
}
func (s *warmupLastStore) set(key string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last[key] = at.UTC()
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(struct {
		Version int                  `json:"version"`
		Last    map[string]time.Time `json:"last"`
	}{1, s.last}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteBytes(s.path, append(data, '\n'), 0o600)
}

func (r *TargetRefresher) warmupLastStore() *warmupLastStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warmLast == nil {
		r.warmLast = newWarmupLastStore(r.root())
	}
	return r.warmLast
}
