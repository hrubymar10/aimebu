package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

func TestClassifyNamesTheCauseNotTheThing(t *testing.T) {
	ResetCaptured()
	a, ra := Classify("supervisor", errors.New("agent agt_01M7KH0FE9BS turn turn_01M7KH0FE9XX: journal write failed in /Users/alice/.aimebu/agents/x after 4096 bytes (deadbeefdeadbeef01)"))
	b, _ := Classify("supervisor", errors.New("agent agt_ZZZZZZZZZZZZ turn turn_QQQQQQQQQQQQ: journal write failed in /home/bob/data/y after 9999 bytes (0123456789abcdef99)"))
	if a != b {
		t.Fatalf("the same cause under other ids and paths gave two keys:\n%s\n%s", a, b)
	}
	for _, leak := range []string{"agt_", "turn_", "alice", "4096", "deadbeef", "/Users"} {
		if strings.Contains(a, leak) || strings.Contains(ra.Error(), leak) {
			t.Fatalf("%q survived in %q / %q", leak, a, ra)
		}
	}
	other, _ := Classify("supervisor", errors.New("stalled"))
	if other == a {
		t.Fatal("different causes share a key")
	}
	if long, _ := Classify("x", errors.New(strings.Repeat("word ", 100))); len([]rune(long)) > maxClass+10 {
		t.Fatalf("the key is not bounded: %d", len([]rune(long)))
	}
	if key, _ := Classify("x", nil); key != "" {
		t.Fatal("no error, no key")
	}
}

func TestReportErrorOncePerCauseWithAreaTag(t *testing.T) {
	ResetCaptured()
	_, transport := startTest(t)
	ctx := context.Background()
	ReportError(ctx, "machine", errors.New("store failed for agt_AAAAAAAAAAAA"))
	ReportError(ctx, "machine", errors.New("store failed for agt_BBBBBBBBBBBB")) // the same cause, another agent
	ReportWarning(ctx, "dispatcher", errors.New("hold check failed"))
	ReportError(ctx, "", errors.New("no area"))
	ReportError(ctx, "machine", nil)
	events := transport.sent()
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	first, second := events[0], events[1]
	if first.Level != sentry.LevelError || first.Tags["area"] != "machine" || len(first.Fingerprint) != 1 || strings.Contains(first.Fingerprint[0], "agt_") {
		t.Fatalf("first event wrong: level=%v tags=%v fingerprint=%v", first.Level, first.Tags, first.Fingerprint)
	}
	if first.Exception[0].Value != "store failed for <id>" {
		t.Fatalf("the event carries the redacted text, got %q", first.Exception[0].Value)
	}
	if second.Level != sentry.LevelWarning || second.Tags["area"] != "dispatcher" {
		t.Fatalf("second event wrong: level=%v tags=%v", second.Level, second.Tags)
	}
}

func TestReportDoesNothingWhileTelemetryIsOff(t *testing.T) {
	ReportError(context.Background(), "machine", errors.New("x")) // no panic, nothing to assert beyond that
}

func TestClassifyTakesOutTimesEmailsRequestIdsAndWindowsPaths(t *testing.T) {
	a, ra := Classify("x", errors.New(`req_0123456789ab failed at 2026-11-17T02:41:09.123Z for alice@example.com in C:\Users\alice\aimebu\x.json`))
	b, _ := Classify("x", errors.New(`req_ZZZZZZZZZZZZ failed at 2026-11-18 11:00:00+01:00 for bob@corp.example.org in D:\data\y.json`))
	if a != b {
		t.Fatalf("the same cause gave two keys:\n%s\n%s", a, b)
	}
	for _, leak := range []string{"req_", "2026", "alice", "example.com", "Users", "C:"} {
		if strings.Contains(ra.Error(), leak) {
			t.Fatalf("%q survived: %q", leak, ra)
		}
	}
}

func TestReportCauseSendsOnlyTheFixedPhrase(t *testing.T) {
	_, transport := startTest(t)
	ResetCaptured()
	ctx := context.Background()
	ReportCause(ctx, "credentials", "the credential reconcile pass failed")
	ReportCause(ctx, "credentials", "the credential reconcile pass failed")
	WarnCause(ctx, "switcher", "a link fell back to a plain copy")
	ReportCause(ctx, "credentials", "")
	events := transport.sent()
	if len(events) != 2 || events[0].Exception[0].Value != "the credential reconcile pass failed" || events[0].Level != sentry.LevelError || events[1].Level != sentry.LevelWarning {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Fingerprint[0] != "credentials: the credential reconcile pass failed" || events[0].Tags["area"] != "credentials" {
		t.Fatalf("fingerprint/tags: %v %v", events[0].Fingerprint, events[0].Tags)
	}
}

func TestACancelledContextIsNotAFailure(t *testing.T) {
	_, transport := startTest(t)
	ResetCaptured()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ReportError(ctx, "machine", errors.New("event poll failed"))
	ReportError(context.Background(), "machine", fmt.Errorf("replay: %w", context.Canceled))
	if got := transport.sent(); len(got) != 0 {
		t.Fatalf("a disconnect was reported: %+v", got)
	}
}

func TestTheDedupeSetExpiresAndIsBounded(t *testing.T) {
	ResetCaptured()
	now := time.Now()
	if !firstTime("k", now) || firstTime("k", now.Add(time.Hour)) {
		t.Fatal("a key is new once, then not for a while")
	}
	if !firstTime("k", now.Add(dedupeFor+time.Minute)) {
		t.Fatal("a key goes out again after dedupeFor")
	}
	for i := 0; i < 3*dedupeKeys; i++ {
		firstTime(fmt.Sprintf("key-%d", i), now.Add(time.Duration(i)*time.Second))
	}
	capturedKeys.Lock()
	size := len(capturedKeys.at)
	capturedKeys.Unlock()
	if size > dedupeKeys {
		t.Fatalf("the set grew to %d, over its cap of %d", size, dedupeKeys)
	}
}
