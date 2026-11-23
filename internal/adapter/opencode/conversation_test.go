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
