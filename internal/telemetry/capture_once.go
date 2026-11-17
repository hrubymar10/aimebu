package telemetry

import (
	"context"
	"sync"

	"github.com/getsentry/sentry-go"
)

// capturedKeys deduplicates named degraded states for the life of the process.
// Keys are stable reason classes, never paths, IDs, or error text.
var capturedKeys sync.Map

// CaptureOnce reports a named condition only on its first occurrence. The key
// is also its fingerprint so changing error details do not create new issues.
func CaptureOnce(ctx context.Context, key string, level sentry.Level, err error) {
	t := Current()
	if t == nil || key == "" || err == nil {
		return
	}
	if _, seen := capturedKeys.LoadOrStore(key, struct{}{}); seen {
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
