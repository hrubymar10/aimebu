package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

type reportTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (r *reportTransport) Flush(time.Duration) bool              { return true }
func (r *reportTransport) FlushWithContext(context.Context) bool { return true }
func (r *reportTransport) Configure(sentry.ClientOptions)        {}
func (r *reportTransport) Close()                                {}
func (r *reportTransport) SendEvent(event *sentry.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}
func (r *reportTransport) sent() []*sentry.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*sentry.Event(nil), r.events...)
}

func startReporting(t *testing.T) *reportTransport {
	t.Helper()
	transport := &reportTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("telemetry.Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	return transport
}

// What the supervisor says went wrong reaches Sentry, once per cause, as the call site's fixed phrase, and still reaches the local log whole; degraded is a warning;
// a failure with a Sentry report of its own (reportLocal) goes to the log only.
func TestReportSendsTheSupervisorsFailuresToSentry(t *testing.T) {
	telemetry.ResetCaptured()
	transport := startReporting(t)
	var logged []string
	c := &Client{OnError: func(err error) { logged = append(logged, err.Error()) }}
	c.report("journal write failed; killing harness", errors.New("agent agt_01M7KH0FE9BS turn turn_01M7KH0FE9XX: journal write failed; killing harness: disk full at /Users/alice/x"))
	c.report("journal write failed; killing harness", errors.New("agent agt_01M7ZZZZZZZZ turn turn_01M7ZZZZZZXX: journal write failed; killing harness: disk full")) // the same cause
	c.degraded("handing the turn to its warm process failed; the turn runs cold", errors.New("agent agt_01M7KH0FE9BS: handing the turn to its warm process: broken pipe; the turn runs cold"))
	c.reportLocal(errors.New("agent agt_01M7KH0FE9BS: refused; the turn runs cold"))
	c.report("anything", nil)
	events := transport.sent()
	if len(events) != 2 {
		t.Fatalf("want 2 events (one per cause), got %d", len(events))
	}
	if events[0].Level != sentry.LevelError || events[0].Tags["area"] != "supervisor" || events[0].Exception[0].Value != "journal write failed; killing harness" {
		t.Fatalf("the journal failure is an error with its fixed phrase: level=%v tags=%v value=%q", events[0].Level, events[0].Tags, events[0].Exception[0].Value)
	}
	if events[1].Level != sentry.LevelWarning {
		t.Fatalf("falling back to cold is a warning, got %v", events[1].Level)
	}
	if len(logged) != 4 || !strings.Contains(logged[0], "alice") {
		t.Fatalf("every failure still reaches the local log, whole (OnError), got %d lines: %v", len(logged), logged)
	}
}

// What a harness wrote (its stderr, in the tail of an error) never goes to Sentry: the event is the call site's phrase, the local log keeps all of it.
func TestReportKeepsAHarnessOwnWordsOutOfSentry(t *testing.T) {
	telemetry.ResetCaptured()
	transport := startReporting(t)
	var logged []string
	c := &Client{OnError: func(err error) { logged = append(logged, err.Error()) }}
	c.degraded("an idle warm process ended by itself", errors.New("agent agt_01M7KH0FE9BS: its idle warm process ended by itself: ERROR user alice@example.com token=hunter2 in /Users/alice/x"))
	c.report("harness failed to start", errors.New("exec: \"claude\": cannot run /home/bob/bin/claude: permission denied"))
	got := transport.sent()
	if len(got) != 2 || got[0].Exception[0].Value != "an idle warm process ended by itself" || got[1].Exception[0].Value != "harness failed to start" {
		t.Fatalf("only the phrase of each call site should go out, got %+v", got)
	}
	if len(logged) != 2 || !strings.Contains(logged[0], "hunter2") || !strings.Contains(logged[1], "permission denied") {
		t.Fatalf("the local log keeps the whole error: %v", logged)
	}
}

// The server's classified refusals are the protocol working and are not reported; any other error code is, by its code alone.
func TestServerErrorFramesAreReportedOnlyWhenTheyAreFailures(t *testing.T) {
	telemetry.ResetCaptured()
	transport := startReporting(t)
	var logged int
	c := &Client{OnError: func(error) { logged++ }}
	for _, code := range []string{"stale_generation", "stale_rev", "conflict", "forbidden", "invalid_transition"} {
		c.handleServerError(protocol.Frame{Type: protocol.Error, Code: code, Message: "refused"})
	}
	if got := transport.sent(); len(got) != 0 {
		t.Fatalf("a classified refusal was reported: %+v", got)
	}
	c.handleServerError(protocol.Frame{Type: protocol.Error, Code: "invalid_request", Message: "bad frame from /Users/alice/x"})
	c.handleServerError(protocol.Frame{Type: protocol.Error, Code: "invalid_request", Message: "another"}) // the same code
	got := transport.sent()
	if len(got) != 1 || got[0].Level != sentry.LevelError || got[0].Exception[0].Value != "server error frame: invalid_request" {
		t.Fatalf("an unclassified code is reported once, by its code: %+v", got)
	}
	if logged != 7 {
		t.Fatalf("every error frame stays in the local log, got %d", logged)
	}
}
