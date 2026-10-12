package server

import (
	"context"
	"log"
	"sync"
	"time"
)

// switcherLinkInterval is how often the server re-runs the switcher's
// active-profile link check. Switching to an empty profile removes the live
// credentials and leaves no stored file; once the user logs in and the harness
// writes the live file, this loop links the profile's stored path to it. The
// check takes no lock and writes nothing when everything is already linked.
const switcherLinkInterval = 60 * time.Second

// maxDedupedErrors bounds the set of already-logged messages so a stream of
// distinct errors cannot grow it without limit.
const maxDedupedErrors = 64

// onceLogger logs each distinct error message once, so a persistent failure in
// a periodic loop does not repeat in the log every tick. A clean run forgets
// what was logged, so a failure that comes back after recovering is logged
// again.
type onceLogger struct {
	logf func(format string, args ...any)
	seen map[string]struct{}
}

func newOnceLogger(logf func(format string, args ...any)) *onceLogger {
	return &onceLogger{logf: logf, seen: map[string]struct{}{}}
}

func (l *onceLogger) report(err error) {
	if err == nil {
		clear(l.seen)
		return
	}
	msg := err.Error()
	if _, ok := l.seen[msg]; ok {
		return
	}
	if len(l.seen) >= maxDedupedErrors {
		clear(l.seen)
	}
	l.seen[msg] = struct{}{}
	l.logf("switcher: ensure active-profile links: %s", msg)
}

// startSwitcherLinkLoop runs ensure once immediately (the one-time migration
// for installs that predate the link) and then every interval until ctx is
// cancelled. The goroutine is tracked by wg so shutdown waits for it.
func startSwitcherLinkLoop(ctx context.Context, wg *sync.WaitGroup, interval time.Duration, ensure func() error, logf func(string, ...any)) {
	if logf == nil {
		logf = log.Printf
	}
	logger := newOnceLogger(logf)
	logger.report(ensure())
	ticker := time.NewTicker(interval)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.report(ensure())
			}
		}
	}()
}
