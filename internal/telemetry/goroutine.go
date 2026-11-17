package telemetry

import "context"

// Go runs fn on a new goroutine under Recover: a panic in it is reported to Sentry (when telemetry is on) and then raised again, so the process
// dies exactly as an unrecovered goroutine panic always did. Reporting changes nothing about how the process behaves; it only makes sure the
// crash reaches Sentry before the process is gone, which a bare `go` never did.
func Go(ctx context.Context, fn func()) {
	go func() {
		defer Recover(ctx)
		fn()
	}()
}
