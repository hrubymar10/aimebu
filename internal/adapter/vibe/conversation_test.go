package vibe

import (
	"path/filepath"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// A recorded `vibe -p --output streaming` turn gives the conversation records: reasoning, messages, and each effect as a call and its
// result, with secrets masked and a long result cut. What a resumed session replays before this turn's own prompt is not written.
func TestConversationConformance(t *testing.T) {
	type m = map[string]any
	root := t.TempDir()
	long := adaptertest.ConversationLongOutput()
	entry := func(id, kind string, rest m) m {
		rest["id"], rest["sessionId"], rest["type"], rest["generationStatus"] = id, testSession, kind, "completed"
		return rest
	}
	text := func(value string) []m { return []m{{"type": "text", "text": value}} }
	stream := adaptertest.ConversationStream(t, root,
		entry("e0", "message", m{"role": "assistant", "content": text("an earlier turn's answer")}),
		entry("e1", "message", m{"role": "user", "source": "turn_start", "content": text("the prompt")}),
		entry("e2", "reasoning", m{"text": "plan the check", "summary": []string{}}),
		entry("e3", "message", m{"role": "assistant", "content": text("I will run a command.")}),
		entry("e4", "effect", m{"title": "bash", "detail": m{"toolName": "bash", "kind": "shell", "input": m{"command": adaptertest.ConversationCommand}},
			"state": m{"status": "completed", "outputText": adaptertest.ConversationKeyOutput, "durationMs": 12.5}}),
		entry("e5", "effect", m{"title": "read_file", "detail": m{"toolName": "read_file", "input": m{"path": "/tmp/big"}},
			"state": m{"status": "failed", "outputText": long}}),
		entry("e6", "notice", m{"level": "error", "message": "the provider was slow"}),
		entry("e7", "message", m{"role": "assistant", "content": text(`{"messages":[]}`)}),
	)
	path := filepath.Join(root, "vibe")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	adaptertest.RunConversation(t, a, spec(t, root), []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationThinking, Text: "plan the check"},
		{Kind: adapter.ConversationAssistant, Text: "I will run a command."},
		{Kind: adapter.ConversationToolCall, Text: adaptertest.Masked(`bash {"command":"curl -H \"Authorization: Bearer ` + adaptertest.ConversationBearer + `\" https://example.test/status"}`)},
		{Kind: adapter.ConversationToolResult, Text: "bash\n" + adaptertest.Masked(adaptertest.ConversationKeyOutput)},
		{Kind: adapter.ConversationToolCall, Text: `read_file {"path":"/tmp/big"}`},
		{Kind: adapter.ConversationToolResult, Text: "read_file (failed)\n" + adaptertest.CutToolOutput(long)},
		{Kind: adapter.ConversationError, Text: "the provider was slow"},
		{Kind: adapter.ConversationAssistant, Text: `{"messages":[]}`},
	})
}
