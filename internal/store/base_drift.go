package store

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/worktree"
)

// The turn context tells an agent when its room worktree has fallen behind the room's base ref (see docs/reference.md). The git calls take
// tens of milliseconds at best and a stuck repository could hold them for seconds, so the dispatch transaction never makes them: it reads
// the answer cache, and a missing or stale entry starts one background refresh (so the first dispatch after an entry goes stale still sees
// the old answer, and a room's first dispatch sees none). A failed or unresolvable lookup is cached as "no drift".

const (
	defaultBaseDriftMin = 5
	baseDriftSubjects   = 5
	baseDriftTTL        = 30 * time.Second
)

// baseDriftMin is AIMEBU_BASE_DRIFT_MIN: how many commits behind the base ref a worktree must be before the turn context says so. Unset or
// invalid is 5; 0 (or less) turns the line off.
func baseDriftMin() int {
	raw := os.Getenv("AIMEBU_BASE_DRIFT_MIN")
	if raw == "" {
		return defaultBaseDriftMin
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultBaseDriftMin
	}
	return n
}

type baseDriftEntry struct {
	at         time.Time
	drift      worktree.Drift
	refreshing bool
}

type baseDriftCache struct {
	mu      sync.Mutex
	entries map[string]*baseDriftEntry
	// compute is the lookup; a test replaces it. now is the clock.
	compute func(ctx context.Context, dir, baseRef, baseCommit string, min int) (worktree.Drift, error)
	now     func() time.Time
	// done, when set, is called after each background refresh (tests wait on it).
	done func()
}

func (c *baseDriftCache) lookup(dir, baseRef, baseCommit string, min int) (worktree.Drift, bool) {
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", dir, baseRef, baseCommit, min)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*baseDriftEntry{}
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	entry := c.entries[key]
	if entry == nil {
		entry = &baseDriftEntry{}
		c.entries[key] = entry
		if len(c.entries) > 256 { // rooms come and go; keep the cache small
			for k, e := range c.entries {
				if !e.refreshing && k != key {
					delete(c.entries, k)
				}
			}
		}
	}
	known := !entry.at.IsZero()
	if (!known || now().Sub(entry.at) > baseDriftTTL) && !entry.refreshing {
		entry.refreshing = true
		compute := c.compute
		if compute == nil {
			compute = func(ctx context.Context, dir, baseRef, baseCommit string, min int) (worktree.Drift, error) {
				return worktree.BaseDrift(ctx, dir, baseRef, baseCommit, min, baseDriftSubjects)
			}
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			drift, err := compute(ctx, dir, baseRef, baseCommit, min)
			if err != nil {
				drift = worktree.Drift{}
				if os.Getenv("AIMEBU_DEBUG") != "" {
					log.Printf("debug: base drift of %s against %q unknown: %v", dir, baseRef, err)
				}
			}
			c.mu.Lock()
			entry.drift, entry.at, entry.refreshing = drift, now(), false
			c.mu.Unlock()
			if c.done != nil {
				c.done()
			}
		}()
	}
	return entry.drift, known
}

// baseDriftContext answers the turn-context keys for a room's worktree on the host machine, or nil when there is nothing to say: no ready
// worktree, a machine that is not the host (the server cannot run git there), drift below the threshold, or an answer not cached yet.
func (s *Store) baseDriftContext(tx *Tx, roomID, machineID string) map[string]any {
	min := baseDriftMin()
	if min <= 0 {
		return nil
	}
	var path, baseRef, baseCommit string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT i.canonical_path,w.base_ref,i.base_commit FROM room_worktree w
		JOIN room_worktree_instance i ON i.room_id=w.room_id JOIN machine m ON m.id=i.machine_id
		WHERE w.room_id=? AND i.machine_id=? AND m.is_host=1 AND i.state='ready' AND i.canonical_path!=''`, roomID, machineID).Scan(&path, &baseRef, &baseCommit)
	if err != nil {
		return nil
	}
	drift, ok := s.drift.lookup(path, baseRef, baseCommit, min)
	if !ok || drift.Behind < min || drift.Behind == 0 {
		return nil
	}
	out := map[string]any{"line": fmt.Sprintf("your worktree is %d commits behind `%s`", drift.Behind, baseRef)}
	if len(drift.Subjects) > 0 {
		out["newest_subjects"] = drift.Subjects
	}
	return out
}
