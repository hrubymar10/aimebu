package supervisor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// captureTransport collects what the supervisor reports, so a test can assert
// the severity a stall is given (a limit kill is a degraded state the server
// recovers from, not an error to triage).
type captureTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (c *captureTransport) Flush(time.Duration) bool              { return true }
func (c *captureTransport) FlushWithContext(context.Context) bool { return true }
func (c *captureTransport) Configure(sentry.ClientOptions)        {}
func (c *captureTransport) SendEvent(event *sentry.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}
func (c *captureTransport) Close() {}
func (c *captureTransport) sent() []*sentry.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*sentry.Event(nil), c.events...)
}

// A stall report goes out once, as a warning, under the fixed cause phrase — the same key the error-level report used
// before, so the change of severity does not split the issue's history.
func TestRunTurnStallIsReportedAsAWarning(t *testing.T) {
	transport := &captureTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	telemetry.ResetCaptured()
	stalledTurn(t, "claude", TurnLimitOverrides{Idle: durationPtr(80 * time.Millisecond)}, adapter.Step{Kind: adapter.StepHang})
	// The test's client has no server to send to, so unrelated send failures may be reported too; only the stall report is asserted.
	var stall *sentry.Event
	for _, event := range transport.sent() {
		if len(event.Fingerprint) == 1 && strings.Contains(event.Fingerprint[0], "a turn stalled; killing harness") {
			stall = event
		}
	}
	if stall == nil {
		t.Fatalf("no stall report among %d events", len(transport.sent()))
	}
	if got := fmt.Sprintf("%v", stall.Level); got != "warning" {
		t.Fatalf("a stall must be a warning, got %q", got)
	}
	if stall.Tags["area"] != "supervisor" {
		t.Fatalf("area tag: %v", stall.Tags)
	}
	if got := stall.Exception[0].Value; got != "a turn stalled; killing harness" {
		t.Fatalf("idle event = %q", got)
	}
}

// A kill by the wall-clock cap (AIMEBU_TURN_MAX) is reported as a warning, like the idle one: the watchdog did its job,
// and the server continues the turn once on its own.
func TestRunTurnWallClockLimitKillIsReportedAsAWarning(t *testing.T) {
	transport := &captureTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	telemetry.ResetCaptured()
	stalledTurn(t, "claude", TurnLimitOverrides{Idle: durationPtr(time.Second), Max: durationPtr(80 * time.Millisecond)}, adapter.Step{Kind: adapter.StepHang})
	var stall *sentry.Event
	for _, event := range transport.sent() {
		if len(event.Fingerprint) == 1 && strings.Contains(event.Fingerprint[0], "turn exceeded the 80ms turn limit") {
			stall = event
		}
	}
	if stall == nil {
		t.Fatalf("no stall report among %d events", len(transport.sent()))
	}
	if got := fmt.Sprintf("%v", stall.Level); got != "warning" {
		t.Fatalf("a wall-clock limit kill must be a warning, got %q", got)
	}
	if got := stall.Exception[0].Value; got != "turn exceeded the 80ms turn limit" {
		t.Fatalf("max event = %q", got)
	}
	if stall.Fingerprint[0] == "supervisor: a turn stalled; killing harness" {
		t.Fatalf("max and idle share a fingerprint: %v", stall.Fingerprint)
	}
}
