package pi

import (
	"path/filepath"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// A recorded `pi --mode json` turn gives the conversation records: a message's thinking and text once (it is reported again at the end
// of its turn), each tool call with its arguments and its result, and a provider failure, with secrets masked and a long result cut.
func TestConversationConformance(t *testing.T) {
	type m = map[string]any
	root := t.TempDir()
	long := adaptertest.ConversationLongOutput()
	first := m{"role": "assistant", "stopReason": "toolUse", "content": []m{
		{"type": "thinking", "thinking": "plan the check"}, {"type": "text", "text": "I will run a command."},
		{"type": "toolCall", "id": "c1", "name": "bash", "arguments": m{"command": adaptertest.ConversationCommand}}}}
	last := m{"role": "assistant", "stopReason": "stop", "content": []m{{"type": "text", "text": `{"messages":[]}`}}}
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "session", "id": testSession},
		m{"type": "turn_start"},
		m{"type": "message_end", "message": m{"role": "user", "content": []m{{"type": "text", "text": "the prompt"}}}},
		m{"type": "message_end", "message": first},
		m{"type": "tool_execution_start", "toolCallId": "c1", "toolName": "bash", "args": m{"command": adaptertest.ConversationCommand}},
		m{"type": "tool_execution_end", "toolCallId": "c1", "toolName": "bash", "isError": false,
			"result": m{"content": []m{{"type": "text", "text": adaptertest.ConversationKeyOutput}}}},
		m{"type": "tool_execution_start", "toolCallId": "c2", "toolName": "read", "args": m{"path": "/tmp/big"}},
		m{"type": "tool_execution_end", "toolCallId": "c2", "toolName": "read", "isError": true, "result": m{"content": []m{{"type": "text", "text": long}}}},
		m{"type": "turn_end", "message": first},
		m{"type": "auto_retry_start", "errorMessage": "overloaded"},
		m{"type": "auto_retry_end", "success": true},
		m{"type": "turn_start"},
		m{"type": "message_end", "message": last},
		m{"type": "turn_end", "message": last},
		m{"type": "agent_settled"},
	)
	path := filepath.Join(root, "pi")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}
	spec := adapter.TurnSpec{TurnID: "conv", Agent: adapter.AgentSpec{ID: "agt_test", Model: "fake-model"}, CheckoutPath: root,
		AgentTools: testToolsConfig(t, "agt_test"), NativeSessionHandle: testSession, Mode: adapter.ModeNormal, NotesJSON: []byte(`[]`)}
	adaptertest.RunConversation(t, a, spec, []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationThinking, Text: "plan the check"},
		{Kind: adapter.ConversationAssistant, Text: "I will run a command."},
		{Kind: adapter.ConversationToolCall, Text: adaptertest.Masked(`bash {"command":"curl -H \"Authorization: Bearer ` + adaptertest.ConversationBearer + `\" https://example.test/status"}`)},
		{Kind: adapter.ConversationToolResult, Text: "bash\n" + adaptertest.Masked(adaptertest.ConversationKeyOutput)},
		{Kind: adapter.ConversationToolCall, Text: `read {"path":"/tmp/big"}`},
		{Kind: adapter.ConversationToolResult, Text: "read (failed)\n" + adaptertest.CutToolOutput(long)},
		{Kind: adapter.ConversationError, Text: "overloaded"},
		{Kind: adapter.ConversationAssistant, Text: `{"messages":[]}`},
	})
}

// An event of a type this adapter does not know, a line that is no event and a message it cannot read each leave an event record with
// the type and none of the content; the events that only frame or stream a message leave none, and the turn runs on.
func TestConversationNotesUnknownEvents(t *testing.T) {
	type m = map[string]any
	root := t.TempDir()
	last := m{"role": "assistant", "stopReason": "stop", "content": []m{{"type": "text", "text": `{"messages":[]}`}}}
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "session", "id": testSession},
		m{"type": "turn_start"},
		m{"type": "hologram", "secret": adaptertest.ConversationKey},
		"a plain line "+adaptertest.ConversationKey,
		m{"type": "message_start", "message": m{"role": "assistant"}},
		m{"type": "message_update", "delta": "pie"},
		m{"type": "message_end", "message": m{"role": "assistant", "content": "not blocks " + adaptertest.ConversationKey}},
		m{"type": "tool_execution_update", "toolCallId": "c1"},
		m{"type": "message_end", "message": last},
		m{"type": "turn_end", "message": last},
		m{"type": "agent_settled"},
	)
	path := filepath.Join(root, "pi")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}
	spec := adapter.TurnSpec{TurnID: "conv", Agent: adapter.AgentSpec{ID: "agt_test", Model: "fake-model"}, CheckoutPath: root,
		AgentTools: testToolsConfig(t, "agt_test"), NativeSessionHandle: testSession, Mode: adapter.ModeNormal, NotesJSON: []byte(`[]`)}
	adaptertest.RunUnknownEvents(t, a, spec, []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationEvent, Text: "hologram"},
		{Kind: adapter.ConversationEvent, Text: "(not an event)"},
		{Kind: adapter.ConversationEvent, Text: "message_end (unreadable)"},
		{Kind: adapter.ConversationAssistant, Text: `{"messages":[]}`},
	})
}
