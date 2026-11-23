package opencode

import (
	"path/filepath"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// A recorded `opencode run --format json` turn gives the conversation records: text, and each finished tool part as a call and its
// result (a failed one says so), with secrets masked and a long result cut.
func TestConversationConformance(t *testing.T) {
	type m = map[string]any
	root := t.TempDir()
	long := adaptertest.ConversationLongOutput()
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "step_start", "sessionID": testSession, "part": m{"type": "step-start"}},
		m{"type": "text", "sessionID": testSession, "part": m{"type": "text", "text": "I will run a command."}},
		m{"type": "tool_use", "sessionID": testSession, "part": m{"type": "tool", "callID": "c1", "tool": "bash",
			"state": m{"status": "completed", "input": m{"command": adaptertest.ConversationCommand}, "output": adaptertest.ConversationKeyOutput}}},
		m{"type": "tool_use", "sessionID": testSession, "part": m{"type": "tool", "callID": "c2", "tool": "read",
			"state": m{"status": "error", "input": m{"filePath": "/tmp/big"}, "error": long}}},
		m{"type": "step_finish", "sessionID": testSession, "part": m{"type": "step-finish", "reason": "tool-calls"}},
		m{"type": "step_start", "sessionID": testSession, "part": m{"type": "step-start"}},
		m{"type": "text", "sessionID": testSession, "part": m{"type": "text", "text": `{"messages":[]}`}},
		m{"type": "step_finish", "sessionID": testSession, "part": m{"type": "step-finish", "reason": "stop"}},
		m{"type": "error", "sessionID": testSession, "error": m{"name": "APIError", "data": m{"message": "provider said no", "statusCode": 429}}},
	)
	path := filepath.Join(root, "opencode")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	adaptertest.RunConversation(t, a, spec(t, root), []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationAssistant, Text: "I will run a command."},
		{Kind: adapter.ConversationToolCall, Text: adaptertest.Masked(`bash {"command":"curl -H \"Authorization: Bearer ` + adaptertest.ConversationBearer + `\" https://example.test/status"}`)},
		{Kind: adapter.ConversationToolResult, Text: "bash\n" + adaptertest.Masked(adaptertest.ConversationKeyOutput)},
		{Kind: adapter.ConversationToolCall, Text: `read {"filePath":"/tmp/big"}`},
		{Kind: adapter.ConversationToolResult, Text: "read (failed)\n" + adaptertest.CutToolOutput(long)},
		{Kind: adapter.ConversationAssistant, Text: `{"messages":[]}`},
		{Kind: adapter.ConversationError, Text: "APIError (HTTP 429): provider said no"},
	})
}

// The answer is the text parts of the last step joined, so the log has every part: the same fragment twice in one step, and the same
// text in two steps with no tool call between them, are each written as often as they came.
func TestConversationKeepsRepeatedText(t *testing.T) {
	type m = map[string]any
	root := t.TempDir()
	part := func(text string) m {
		return m{"type": "text", "sessionID": testSession, "part": m{"type": "text", "text": text}}
	}
	step := func(kind, reason string) m {
		return m{"type": kind, "sessionID": testSession, "part": m{"type": kind, "reason": reason}}
	}
	stream := adaptertest.ConversationStream(t, root,
		step("step_start", ""), part("ha"), part("ha"), step("step_finish", "length"),
		step("step_start", ""), part("ha"), step("step_finish", "stop"),
	)
	path := filepath.Join(root, "opencode")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	got := adaptertest.ConversationRecords(t, a, spec(t, root))
	want := adaptertest.ConversationRecord{Kind: adapter.ConversationAssistant, Text: "ha"}
	if len(got) != 3 || got[0] != want || got[1] != want || got[2] != want {
		t.Fatalf("records=%+v", got)
	}
}

// An event of a type this adapter does not know, a line that is no event and a tool part it cannot read each leave an event record
// with the type and none of the content; the turn runs on.
func TestConversationNotesUnknownEvents(t *testing.T) {
	type m = map[string]any
	root := t.TempDir()
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "step_start", "sessionID": testSession, "part": m{"type": "step-start"}},
		m{"type": "hologram", "sessionID": testSession, "part": m{"type": "hologram", "text": adaptertest.ConversationKey}},
		"a plain line "+adaptertest.ConversationKey,
		m{"type": "tool_use", "sessionID": testSession, "part": m{"type": "tool", "secret": adaptertest.ConversationKey}},
		m{"sessionID": testSession, "secret": adaptertest.ConversationKey},
		m{"type": "text", "sessionID": testSession, "part": m{"type": "text", "text": `{"messages":[]}`}},
		m{"type": "step_finish", "sessionID": testSession, "part": m{"type": "step-finish", "reason": "stop"}},
	)
	path := filepath.Join(root, "opencode")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	adaptertest.RunUnknownEvents(t, a, spec(t, root), []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationEvent, Text: "hologram"},
		{Kind: adapter.ConversationEvent, Text: "(not an event)"},
		{Kind: adapter.ConversationEvent, Text: "tool_use (unreadable)"},
		{Kind: adapter.ConversationEvent, Text: "(no type)"},
		{Kind: adapter.ConversationAssistant, Text: `{"messages":[]}`},
	})
}
