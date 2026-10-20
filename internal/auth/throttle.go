package auth

import (
	"sync"
	"time"
)

type throttleEntry struct {
	failures    int
	until       time.Time
	lastSeen    time.Time
	windowStart time.Time
	inflight    bool
}

const maxThrottleEntries = 4096
const freeFailureBudget = 5
const failureWindow = 15 * time.Minute

// Throttler bounds online guesses independently by login and remote IP.
type Throttler struct {
	mu      sync.Mutex
	entries map[string]throttleEntry
	base    time.Duration
	max     time.Duration
}

func NewThrottler() *Throttler {
	return &Throttler{entries: make(map[string]throttleEntry),
		base: 250 * time.Millisecond, max: 30 * time.Second}
}

// reserve serializes guesses for both the login and IP while password work is
// in progress. A second parallel guess cannot pass the backoff window.
func (throttle *Throttler) reserve(login, remoteIP string, now time.Time) bool {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	keys := []string{"login:" + login, "ip:" + remoteIP}
	for _, key := range keys {
		entry := throttle.entries[key]
		if entry.inflight || now.Before(entry.until) {
			return false
		}
	}
	throttle.pruneLocked(now)
	for len(throttle.entries) > maxThrottleEntries-len(keys) {
		oldest := ""
		var when time.Time
		for key, entry := range throttle.entries {
			if entry.inflight {
				continue
			}
			if oldest == "" || entry.lastSeen.Before(when) {
				oldest, when = key, entry.lastSeen
			}
		}
		if oldest == "" {
			return false
		}
		delete(throttle.entries, oldest)
	}
	for _, key := range keys {
		entry := throttle.entries[key]
		entry.inflight = true
		entry.lastSeen = now
		throttle.entries[key] = entry
	}
	return true
}

func (throttle *Throttler) pruneLocked(now time.Time) {
	for key, entry := range throttle.entries {
		if !entry.inflight && now.Sub(entry.lastSeen) > failureWindow {
			delete(throttle.entries, key)
		}
	}
}

func (throttle *Throttler) fail(login, remoteIP string, now time.Time) {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	for _, key := range []string{"login:" + login, "ip:" + remoteIP} {
		entry := throttle.entries[key]
		entry.inflight = false
		entry.lastSeen = now
		if entry.windowStart.IsZero() || now.Sub(entry.windowStart) >= failureWindow {
			entry.failures = 0
			entry.windowStart = now
		}
		entry.failures++
		delay := time.Duration(0)
		if entry.failures > freeFailureBudget {
			delay = throttle.base
			for i := freeFailureBudget + 1; i < entry.failures && delay < throttle.max; i++ {
				delay *= 2
			}
			if delay > throttle.max {
				delay = throttle.max
			}
		}
		entry.until = now.Add(delay)
		throttle.entries[key] = entry
	}
}

func (throttle *Throttler) RetryAfter(login, remoteIP string, now time.Time) time.Duration {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	delay := time.Duration(0)
	for _, key := range []string{"login:" + login, "ip:" + remoteIP} {
		entry := throttle.entries[key]
		if entry.inflight && delay < time.Second {
			delay = time.Second
		}
		if d := entry.until.Sub(now); d > delay {
			delay = d
		}
	}
	return delay
}

func (throttle *Throttler) release(login, remoteIP string) {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	for _, key := range []string{"login:" + login, "ip:" + remoteIP} {
		entry, ok := throttle.entries[key]
		if !ok {
			continue
		}
		entry.inflight = false
		throttle.entries[key] = entry
	}
}

// reset clears the login key after a success. The IP key keeps its failures:
// one valid credential must not wipe a source's record of guesses against
// other accounts, or a sprayer could interleave its own login to stay under
// the per-IP budget. The IP entry ages out with the failure window.
func (throttle *Throttler) reset(login string) {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	delete(throttle.entries, "login:"+login)
}
