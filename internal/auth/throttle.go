package auth

import (
	"net/netip"
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

// aggregateFreeFailures is how many wrong guesses at one login, from anywhere, pass before guesses at it are spaced out by a second. It is
// the limit on a guesser who spreads a login's guesses over many addresses; it is a fixed pause and never grows, so it cannot be used to keep
// the person out.
const aggregateFreeFailures = 50
const aggregateSpacing = time.Second

// clientKey is the part of a remote address a throttle counts as one source: the address itself for IPv4, and the /64 for IPv6, which is what
// one subscriber is handed, so changing the low 64 bits is not a new source.
func clientKey(remoteIP string) string {
	addr, err := netip.ParseAddr(remoteIP)
	if err != nil {
		return remoteIP
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}

// Throttler bounds online guesses independently by name (a login, a token) and remote source.
//
// A Throttler made with Scoped set (the login one) counts the guesses at a login per source instead of for everyone: the name's failures
// are kept per (name, source), so someone guessing at a login from their own address cannot lock the person out of it from theirs, and
// the source's own total is kept next to it. A scoped throttle adds one thing, the aggregate: a fixed one-second spacing on guesses at
// a login once many have failed across all sources.
type Throttler struct {
	mu      sync.Mutex
	entries map[string]throttleEntry
	base    time.Duration
	max     time.Duration
	Scoped  bool
}

func NewThrottler() *Throttler {
	return &Throttler{entries: make(map[string]throttleEntry),
		base: 250 * time.Millisecond, max: 30 * time.Second}
}

// NewLoginThrottler is the throttle for password guesses: per (login, source), per source, and the aggregate for the login.
func NewLoginThrottler() *Throttler {
	throttle := NewThrottler()
	throttle.Scoped = true
	return throttle
}

type throttleKey struct {
	name string
	// free is how many failures pass without a delay, base and max the delay that follows (it doubles from base up to max).
	free      int
	base, max time.Duration
}

func (throttle *Throttler) keys(login, remoteIP string) []throttleKey {
	client := clientKey(remoteIP)
	if !throttle.Scoped {
		return []throttleKey{{"login:" + login, freeFailureBudget, throttle.base, throttle.max}, {"ip:" + client, freeFailureBudget, throttle.base, throttle.max}}
	}
	return []throttleKey{
		{"pair:" + login + "\x00" + client, freeFailureBudget, throttle.base, throttle.max},
		{"ip:" + client, freeFailureBudget, throttle.base, throttle.max},
		{"all:" + login, aggregateFreeFailures, aggregateSpacing, aggregateSpacing},
	}
}

// reserve serializes guesses for the name and the source while password work is
// in progress. A second parallel guess cannot pass the backoff window.
func (throttle *Throttler) reserve(login, remoteIP string, now time.Time) bool {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	keys := throttle.keys(login, remoteIP)
	// A source that has not failed at this login in the window is let through the aggregate's pause: the person signing in with the right password
	// is such a source whenever a guesser from elsewhere has the pause running. (A source that guesses wrong is held by the pause from then on.)
	pair := throttle.entries[keys[0].name]
	freshSource := pair.failures == 0 || now.Sub(pair.windowStart) >= failureWindow
	for _, key := range keys {
		entry := throttle.entries[key.name]
		// The aggregate is not held for the length of a guess: only its spacing applies, so many people logging in at once do not queue.
		if isAggregate(key.name) {
			if now.Before(entry.until) && !freshSource {
				return false
			}
			continue
		}
		if now.Before(entry.until) || entry.inflight {
			return false
		}
	}
	throttle.pruneLocked(now)
	if !throttle.makeRoomLocked(keys, now) {
		return false
	}
	for _, key := range keys {
		if isAggregate(key.name) {
			continue
		}
		entry := throttle.entries[key.name]
		entry.inflight = true
		entry.lastSeen = now
		throttle.entries[key.name] = entry
	}
	return true
}

func isAggregate(name string) bool { return len(name) > 4 && name[:4] == "all:" }

// makeRoomLocked frees entries for the keys about to be added when the table is full. An entry that has not earned a delay (it holds no more
// than the free failures) is forgotten oldest first: forgetting it gives a source at most its free guesses back. One that carries a delay
// is kept while it runs, so a flood of throwaway sources cannot wash a penalty away, and when nothing else can go the new guess is refused.
func (throttle *Throttler) makeRoomLocked(keys []throttleKey, now time.Time) bool {
	for len(throttle.entries) > maxThrottleEntries-len(keys) {
		oldest, penalized := "", ""
		var when, penalizedUntil time.Time
		for name, entry := range throttle.entries {
			if entry.inflight {
				continue
			}
			if entry.failures > freeFailureBudget && now.Before(entry.until.Add(failureWindow)) {
				if penalized == "" || entry.until.Before(penalizedUntil) {
					penalized, penalizedUntil = name, entry.until
				}
				continue
			}
			if oldest == "" || entry.lastSeen.Before(when) {
				oldest, when = name, entry.lastSeen
			}
		}
		switch {
		case oldest != "":
			delete(throttle.entries, oldest)
		case penalized != "" && !now.Before(penalizedUntil):
			delete(throttle.entries, penalized)
		default:
			return false
		}
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
	for _, key := range throttle.keys(login, remoteIP) {
		entry := throttle.entries[key.name]
		entry.inflight = false
		entry.lastSeen = now
		if entry.windowStart.IsZero() || now.Sub(entry.windowStart) >= failureWindow {
			entry.failures = 0
			entry.windowStart = now
		}
		entry.failures++
		delay := time.Duration(0)
		if entry.failures > key.free {
			delay = key.base
			for i := key.free + 1; i < entry.failures && delay < key.max; i++ {
				delay *= 2
			}
			if delay > key.max {
				delay = key.max
			}
		}
		entry.until = now.Add(delay)
		throttle.entries[key.name] = entry
	}
}

func (throttle *Throttler) RetryAfter(login, remoteIP string, now time.Time) time.Duration {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	delay := time.Duration(0)
	for _, key := range throttle.keys(login, remoteIP) {
		entry := throttle.entries[key.name]
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
	for _, key := range throttle.keys(login, remoteIP) {
		entry, ok := throttle.entries[key.name]
		if !ok {
			continue
		}
		entry.inflight = false
		throttle.entries[key.name] = entry
	}
}

// reset clears the name's key after a success (for a scoped throttle, the name's key for this source). The source's own key keeps its
// failures: one valid credential must not wipe a source's record of guesses against other accounts, or a sprayer could interleave its own
// login to stay under the per-source budget. The source entry ages out with the failure window.
func (throttle *Throttler) reset(login, remoteIP string) {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	delete(throttle.entries, throttle.keys(login, remoteIP)[0].name)
}
