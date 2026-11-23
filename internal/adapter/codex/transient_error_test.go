package codex

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// runStream runs one exec turn whose stdout is the given events (hand-written samples) and returns its final answer, exit and records.
func runStream(t *testing.T, events ...any) (string, adapter.ExitInfo, []adaptertest.ConversationRecord) {
	t.Helper()
	root := t.TempDir()
	stream := adaptertest.ConversationStream(t, root, events...)
	path := filepath.Join(root, "codex")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.Command, a.AllowNative, a.AuthPath = path, true, ""
	sink := &records{}
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "transient", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: adapter.ModeCorrection, ConversationSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	answer := ""
	for event := range turn.Events() {
		if event.Kind == adapter.EventFinalAnswer {
			answer = string(event.Answer)
		}
	}
	exit, err := turn.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return answer, exit, sink.got
}

func hasRecord(got []adaptertest.ConversationRecord, kind adapter.ConversationKind, part string) bool {
	for _, r := range got {
		if r.Kind == kind && strings.Contains(r.Text, part) {
			return true
		}
	}
	return false
}

func TestReconnectThenCompleteKeepsAnswer(t *testing.T) {
	answer, exit, got := runStream(t,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "turn.started"},
		m{"type": "error", "message": "Reconnecting... 1/5 (stream disconnected before completion)"},
		m{"type": "error", "message": "Reconnecting... 2/5 (stream disconnected before completion)"},
		m{"type": "item.completed", "item": m{"id": "a1", "type": "agent_message", "text": "done"}},
		m{"type": "turn.completed", "usage": m{"input_tokens": 10, "output_tokens": 2}},
	)
	if answer != "done" || !exit.TerminalEvent {
		t.Fatalf("answer=%q exit=%+v", answer, exit)
	}
	if !hasRecord(got, adapter.ConversationEvent, "warning: Reconnecting... 2/5") || hasRecord(got, adapter.ConversationError, "") {
		t.Fatalf("records=%+v", got)
	}
}

func TestReconnectExhaustedThenTurnFailedFails(t *testing.T) {
	answer, exit, got := runStream(t,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "error", "message": "Reconnecting... 5/5 (stream disconnected before completion)"},
		m{"type": "turn.failed", "error": m{"message": "stream disconnected before completion"}},
	)
	if answer != "" || exit.TerminalEvent || !strings.Contains(exit.ErrorEvent, "turn.failed") {
		t.Fatalf("answer=%q exit=%+v", answer, exit)
	}
	if !hasRecord(got, adapter.ConversationEvent, "warning: Reconnecting... 5/5") || !hasRecord(got, adapter.ConversationError, "stream disconnected") {
		t.Fatalf("records=%+v", got)
	}
}

func TestReconnectThenPlainErrorStillWithholdsAnswer(t *testing.T) {
	answer, exit, got := runStream(t,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "item.completed", "item": m{"id": "a1", "type": "agent_message", "text": "partial"}},
		m{"type": "error", "message": "Reconnecting... 5/5 (stream disconnected before completion)"},
		m{"type": "error", "message": "unexpected status 401 Unauthorized"},
	)
	if answer != "" || exit.TerminalEvent || !strings.Contains(exit.ErrorEvent, "401") || !hasRecord(got, adapter.ConversationError, "401") {
		t.Fatalf("answer=%q exit=%+v records=%+v", answer, exit, got)
	}
}

func TestPlainErrorBeforeCompletionIsRecoverable(t *testing.T) {
	answer, exit, got := runStream(t,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "error", "message": "stream error: connection reset"},
		m{"type": "item.completed", "item": m{"id": "a1", "type": "agent_message", "text": "done"}},
		m{"type": "turn.completed", "usage": m{"input_tokens": 10, "output_tokens": 2}},
	)
	if answer != "done" || !exit.TerminalEvent || !hasRecord(got, adapter.ConversationEvent, "warning: stream error") {
		t.Fatalf("answer=%q exit=%+v records=%+v", answer, exit, got)
	}
}

func TestErrorAfterCompletionFails(t *testing.T) {
	answer, exit, _ := runStream(t,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "item.completed", "item": m{"id": "a1", "type": "agent_message", "text": "done"}},
		m{"type": "turn.completed", "usage": m{"input_tokens": 10, "output_tokens": 2}},
		m{"type": "error", "message": "fatal after completion"},
	)
	if answer != "" || exit.TerminalEvent {
		t.Fatalf("answer=%q exit=%+v", answer, exit)
	}
}

func TestPlainErrorWithoutCompletionFails(t *testing.T) {
	answer, exit, got := runStream(t,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "error", "message": "unexpected status 401 Unauthorized"},
	)
	if answer != "" || exit.TerminalEvent || !hasRecord(got, adapter.ConversationError, "401") {
		t.Fatalf("answer=%q exit=%+v records=%+v", answer, exit, got)
	}
}

// The app-server marks a retried error with willRetry: a warning; a final one, or one without the flag, stays an error.
func TestAppServerRetryErrorIsWarning(t *testing.T) {
	sink := &records{}
	turn := &codexWarmTurn{id: "t1", events: make(chan adapter.Event, 16), done: make(chan struct{}), conv: adapter.NewConversation(sink), warm: &codexWarmSession{}}
	turn.handleNotification("error", []byte(`{"error":{"message":"Reconnecting... 1/5"},"willRetry":true}`))
	turn.handleNotification("error", []byte(`{"error":{"message":"Reconnecting... 2/5"},"willRetry":false}`))
	turn.handleNotification("error", []byte(`{"error":{"message":"unexpected status 401"},"willRetry":false}`))
	if len(sink.got) != 3 || sink.got[0].Kind != adapter.ConversationEvent || sink.got[1].Kind != adapter.ConversationEvent || sink.got[2].Kind != adapter.ConversationError {
		t.Fatalf("records=%+v", sink.got)
	}
}
