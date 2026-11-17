package telemetry

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

// capturedKeys deduplicates named degraded states: a key is sent once, then not again for dedupeFor (a failure that goes on for days is a new report
// each day, not one line in Sentry for the life of a long-running server). The set is bounded, so a flood of distinct keys cannot grow it without end.
// Keys are stable reason classes, never paths, IDs, or error text.
var capturedKeys = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

const (
	dedupeFor  = 24 * time.Hour
	dedupeKeys = 2000
)

// firstTime says whether key is new (or older than dedupeFor), and notes it. When the set is full the expired entries go first, then the oldest half.
func firstTime(key string, now time.Time) bool {
	capturedKeys.Lock()
	defer capturedKeys.Unlock()
	if at, ok := capturedKeys.at[key]; ok && now.Sub(at) < dedupeFor {
		return false
	}
	if len(capturedKeys.at) >= dedupeKeys {
		for k, at := range capturedKeys.at {
			if now.Sub(at) >= dedupeFor {
				delete(capturedKeys.at, k)
			}
		}
		if len(capturedKeys.at) >= dedupeKeys { // all recent: forget the oldest half
			var stamps []time.Time
			for _, at := range capturedKeys.at {
				stamps = append(stamps, at)
			}
			sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
			cut := stamps[len(stamps)/2]
			for k, at := range capturedKeys.at {
				if !at.After(cut) {
					delete(capturedKeys.at, k)
				}
			}
		}
	}
	capturedKeys.at[key] = now
	return true
}

// ResetCaptured forgets every key sent so far, so a test that reports the same cause again (under -count=2, or in a second test) sees it go out.
func ResetCaptured() {
	capturedKeys.Lock()
	defer capturedKeys.Unlock()
	capturedKeys.at = map[string]time.Time{}
}

// CaptureOnce reports a named condition once, and again only after dedupeFor (a
// day) has passed. The key is also its fingerprint so changing error details do
// not create new issues.
func CaptureOnce(ctx context.Context, key string, level sentry.Level, err error) {
	t := Current()
	if t == nil || key == "" || err == nil {
		return
	}
	if !firstTime(key, time.Now()) {
		return
	}
	scope := scopeFor(ctx)
	scope.SetLevel(level)
	scope.SetFingerprint([]string{key})
	t.hub.Client().CaptureException(err, &sentry.EventHint{Context: ctx}, scope)
}

// CaptureWarning reports a degraded state once with warning severity.
func CaptureWarning(ctx context.Context, key string, err error) {
	CaptureOnce(ctx, key, sentry.LevelWarning, err)
}
