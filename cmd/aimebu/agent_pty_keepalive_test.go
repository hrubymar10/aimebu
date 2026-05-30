package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestAgentPTYHeartbeatIntervalBelowHalfMinimumStaleWindow(t *testing.T) {
	minimumStaleWindow := 60 * time.Second
	if agentPTYHeartbeatInterval >= minimumStaleWindow/2 {
		t.Fatalf("agentPTYHeartbeatInterval = %s, want < %s", agentPTYHeartbeatInterval, minimumStaleWindow/2)
	}
}

func TestAgentPTYIdleTrackerNudgeOnlyAfterReadySignal(t *testing.T) {
	tracker := &agentPTYIdleTracker{}

	if _, ok := tracker.MarkNudgeReady(0); ok {
		t.Fatal("nudge was ready before any idle signal")
	}

	tracker.Observe([]byte("\x1b[95mstatus · ← for agents"))
	if !tracker.IsIdle() {
		t.Fatal("tracker did not enter idle after ready signal")
	}
	if _, ok := tracker.MarkNudgeReady(time.Hour); ok {
		t.Fatal("nudge was ready before the idle delay")
	}
	if _, ok := tracker.MarkNudgeReady(0); !ok {
		t.Fatal("nudge was not ready after idle delay")
	}
	if _, ok := tracker.MarkNudgeReady(0); ok {
		t.Fatal("nudge was ready twice in the same idle episode")
	}
}

func TestAgentPTYIdleTrackerThinkingClearsIdleAndNudge(t *testing.T) {
	tracker := &agentPTYIdleTracker{}
	tracker.Observe([]byte("\x1b[95mstatus · ← for agents"))
	if _, ok := tracker.MarkNudgeReady(0); !ok {
		t.Fatal("initial nudge was not ready")
	}

	tracker.Observe([]byte("\x1b[2K\x1b[1G✻ Thinking · thinking) · ↓ 10 tokens · esc to interrupt"))
	if tracker.IsIdle() {
		t.Fatal("tracker stayed idle after active-turn output")
	}
	if _, ok := tracker.MarkNudgeReady(0); ok {
		t.Fatal("nudge was ready while thinking")
	}

	tracker.Observe([]byte("\x1b[95mstatus · ← for agents"))
	if _, ok := tracker.MarkNudgeReady(0); !ok {
		t.Fatal("nudge was not re-armed after a new idle episode")
	}
}

func TestAgentPTYIdleTrackerLatchesCompletionRepaintWithSpinnerGlyph(t *testing.T) {
	tracker := &agentPTYIdleTracker{}

	chunk := "\x1b[2C\x1b[3A\x1b[2D\x1b[3B" +
		"\r\x1b[6A\x1b[37m✻\x1b[3GBrewed for 1m 21s\x1b[39m\x1b[K" +
		"\r\x1b[1B\x1b[K\r\x1b[1B\x1b[37m────────────────────────────────────────────────────────────────────────────────" +
		"\r\x1b[1B\x1b[39m❯\u00a0\x1b[7m \x1b[27m\x1b[K" +
		"\r\x1b[1B\x1b[37m────────────────────────────────────────────────────────────────────────────────" +
		"\r\x1b[1B\x1b[39m  \x1b[95m⏵⏵ bypass permissions on\x1b[37m (shift+tab to cycle) · ← for agents\x1b[39m\x1b[K"
	tracker.Observe([]byte(chunk))

	if !tracker.IsIdle() {
		t.Fatal("tracker did not latch idle from combined completion repaint")
	}
	if _, ok := tracker.MarkNudgeReady(0); !ok {
		t.Fatal("nudge was not ready after combined completion repaint")
	}
}

func TestAgentPTYIdleTrackerActiveTurnWithSpinnerGlyphIsNotIdle(t *testing.T) {
	tracker := &agentPTYIdleTracker{}

	tracker.Observe([]byte("\x1b[31m✻\x1b[39m Working · thinking) · ↓ 45.4k tokens · esc to interrupt"))

	if tracker.IsIdle() {
		t.Fatal("tracker entered idle during active turn")
	}
	if _, ok := tracker.MarkNudgeReady(0); ok {
		t.Fatal("nudge was ready during active turn")
	}
}

func TestAgentPTYIdleNudgeClearsComposerLine(t *testing.T) {
	var buf bytes.Buffer
	agentPTYSubmitDelay = 0
	defer func() { agentPTYSubmitDelay = 150 * time.Millisecond }()

	agentPTYWriteIdleNudge(&buf, "keep listening", nil)

	got := buf.String()
	if !strings.HasPrefix(got, "\x15") {
		t.Fatalf("nudge bytes %q do not start with Ctrl-U", got)
	}
	if !strings.HasSuffix(got, "keep listening\r") {
		t.Fatalf("nudge bytes %q do not submit keep listening", got)
	}
}
