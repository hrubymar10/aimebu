package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

type startupTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (s *startupTransport) Flush(time.Duration) bool              { return true }
func (s *startupTransport) FlushWithContext(context.Context) bool { return true }
func (s *startupTransport) Configure(sentry.ClientOptions)        {}
func (s *startupTransport) Close()                                {}
func (s *startupTransport) SendEvent(event *sentry.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

// A server that cannot start says so on stderr and to Sentry (area `startup`), flushed before the process exits, and the path in the error stays out of the event.
// Not parallel: the reporter is process-wide.
func TestFailStartupReportsToSentryAndStderr(t *testing.T) {
	telemetry.ResetCaptured()
	transport := &startupTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("telemetry.Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	var stderr bytes.Buffer
	failStartup(&stderr, errors.New("open /Users/alice/.aimebu/server/aimebu.sqlite: permission denied"))
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.events) != 1 || transport.events[0].Tags["area"] != "startup" || strings.Contains(transport.events[0].Exception[0].Value, "alice") {
		t.Fatalf("want one startup event without the path, got %+v", transport.events)
	}
}
