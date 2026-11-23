package codex

import (
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

type m = map[string]any

func conversationWant(long string) []adaptertest.ConversationRecord {
	return []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationThinking, Text: "plan the check"},
		{Kind: adapter.ConversationAssistant, Text: "I will run a command."},
		{Kind: adapter.ConversationToolCall, Text: adaptertest.Masked("shell " + adaptertest.ConversationCommand)},
		{Kind: adapter.ConversationToolResult, Text: "shell\n" + adaptertest.Masked(adaptertest.ConversationKeyOutput)},
		{Kind: adapter.ConversationToolCall, Text: `aimebu.room_history {"room":"r1"}`},
		{Kind: adapter.ConversationToolResult, Text: "aimebu.room_history (failed)\n" + adaptertest.CutToolOutput(long)},
		{Kind: adapter.ConversationAssistant, Text: `{"messages":[]}`},
	}
}

// A recorded `codex exec --json` turn gives the conversation records: reasoning, text, a command as a call when it starts and a result
// when it completes, and a tool that is only reported complete as both, with secrets masked and a long result cut.
func TestConversationConformance(t *testing.T) {
	root := t.TempDir()
	long := adaptertest.ConversationLongOutput()
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "turn.started"},
		m{"type": "item.completed", "item": m{"id": "r1", "type": "reasoning", "text": "plan the check"}},
		m{"type": "item.completed", "item": m{"id": "a1", "type": "agent_message", "text": "I will run a command."}},
		m{"type": "item.started", "item": m{"id": "c1", "type": "command_execution", "command": adaptertest.ConversationCommand, "status": "in_progress"}},
		m{"type": "item.updated", "item": m{"id": "c1", "type": "command_execution", "command": adaptertest.ConversationCommand, "status": "in_progress"}},
		m{"type": "item.completed", "item": m{"id": "c1", "type": "command_execution", "command": adaptertest.ConversationCommand, "aggregated_output": adaptertest.ConversationKeyOutput, "exit_code": 0, "status": "completed"}},
		m{"type": "item.completed", "item": m{"id": "m1", "type": "mcp_tool_call", "server": "aimebu", "tool": "room_history", "arguments": m{"room": "r1"}, "error": long, "status": "failed"}},
		m{"type": "item.completed", "item": m{"id": "a2", "type": "agent_message", "text": `{"messages":[]}`}},
		m{"type": "turn.completed", "usage": m{"input_tokens": 10, "output_tokens": 2}},
	)
	path := filepath.Join(root, "codex")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.Command, a.AllowNative, a.AuthPath = path, true, ""
	spec := adapter.TurnSpec{TurnID: "conv", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: adapter.ModeCorrection}
	adaptertest.RunConversation(t, a, spec, conversationWant(long))
}

type records struct {
	got []adaptertest.ConversationRecord
}

func (r *records) Record(kind adapter.ConversationKind, text string) {
	r.got = append(r.got, adaptertest.ConversationRecord{Kind: kind, Text: text})
}

// The app-server of a warm process reports the same items with camelCase keys: they give the same records. A failed command says its
// exit status, and a failure event is an error record.
func TestConversationOfAppServerItems(t *testing.T) {
	long := adaptertest.ConversationLongOutput()
	sink := &records{}
	conv := adapter.NewConversation(sink)
	item := func(completed bool, value m) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		transcribeItem(conv, "item", raw, completed)
	}
	item(true, m{"id": "r1", "type": "reasoning", "summary": []string{"plan the check"}, "content": []string{}})
	item(false, m{"id": "a1", "type": "agentMessage", "text": ""})
	item(true, m{"id": "a1", "type": "agentMessage", "text": "I will run a command."})
	item(false, m{"id": "c1", "type": "commandExecution", "command": adaptertest.ConversationCommand, "status": "inProgress"})
	item(true, m{"id": "c1", "type": "commandExecution", "command": adaptertest.ConversationCommand, "aggregatedOutput": adaptertest.ConversationKeyOutput, "exitCode": 0, "status": "completed"})
	item(true, m{"id": "m1", "type": "mcpToolCall", "server": "aimebu", "tool": "room_history", "arguments": m{"room": "r1"}, "error": long, "status": "failed"})
	item(true, m{"id": "a2", "type": "agentMessage", "text": `{"messages":[]}`, "phase": "final_answer"})
	adaptertest.CheckConversation(t, sink.got, conversationWant(long))

	sink.got = nil
	item(true, m{"id": "c2", "type": "commandExecution", "command": "false", "aggregatedOutput": "", "exitCode": 1, "status": "failed"})
	transcribeFailure(conv, json.RawMessage(`{"error":{"message":"stream disconnected"},"willRetry":false}`))
	want := []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationToolCall, Text: "shell false"},
		{Kind: adapter.ConversationToolResult, Text: "shell (failed)\n\n(exit 1)"},
		{Kind: adapter.ConversationError, Text: "stream disconnected"},
	}
	if len(sink.got) != len(want) {
		t.Fatalf("records=%+v", sink.got)
	}
	for i := range want {
		if sink.got[i] != want[i] {
			t.Fatalf("record %d=%+v want %+v", i, sink.got[i], want[i])
		}
	}
}

// An event of a type this adapter does not know, a line that is no event and an item it cannot read each leave an event record with the
// type and none of the content. An item of an unknown type is written like a tool, under its own type name; the turn runs on.
func TestConversationNotesUnknownEvents(t *testing.T) {
	root := t.TempDir()
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "hologram.frame", "secret": adaptertest.ConversationKey},
		"a plain line "+adaptertest.ConversationKey,
		m{"type": "item.completed", "item": "not an item " + adaptertest.ConversationKey},
		m{"type": "item.completed", "item": m{"id": "x1", "secret": adaptertest.ConversationKey}},
		m{"type": "item.completed", "item": m{"id": "x2", "type": "hologram_call", "payload": m{"a": 1}, "status": "completed"}},
		m{"type": "item.completed", "item": m{"id": "a1", "type": "agent_message", "text": `{"messages":[]}`}},
		m{"type": "turn.completed", "usage": m{"input_tokens": 10, "output_tokens": 2}},
	)
	path := filepath.Join(root, "codex")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.Command, a.AllowNative, a.AuthPath = path, true, ""
	spec := adapter.TurnSpec{TurnID: "conv", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: adapter.ModeCorrection}
	adaptertest.RunUnknownEvents(t, a, spec, []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationEvent, Text: "hologram.frame"},
		{Kind: adapter.ConversationEvent, Text: "(not an event)"},
		{Kind: adapter.ConversationEvent, Text: "item.completed (unreadable)"},
		{Kind: adapter.ConversationEvent, Text: "item.completed item (no type)"},
		{Kind: adapter.ConversationToolCall, Text: `hologram_call {"payload":{"a":1}}`},
		{Kind: adapter.ConversationToolResult, Text: "hologram_call\ncompleted"},
		{Kind: adapter.ConversationAssistant, Text: `{"messages":[]}`},
	})
}

// The app-server of a warm process: a notification this adapter does not read leaves an event record, the streamed pieces of an item
// and the thread's state do not, and an item that is not an object is noted and does not stop the turn.
func TestConversationNotesUnknownAppServerNotifications(t *testing.T) {
	sink := &records{}
	turn := &codexWarmTurn{id: "t1", events: make(chan adapter.Event, 16), done: make(chan struct{}), conv: adapter.NewConversation(sink), warm: &codexWarmSession{}}
	for _, call := range [][2]string{
		{"hologram/frame", `{"secret":"` + adaptertest.ConversationKey + `"}`},
		{"hologram/frame", `{"secret":"again"}`},
		{"item/agentMessage/delta", `{"delta":"pie"}`},
		{"thread/status/changed", `{}`},
		{"item/completed", `{"item":"` + adaptertest.ConversationKey + `"}`},
		{"item/completed", `[1,2]`},
	} {
		turn.handleNotification(call[0], json.RawMessage(call[1]))
	}
	want := []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationEvent, Text: "hologram/frame"},
		{Kind: adapter.ConversationEvent, Text: "item/completed (unreadable)"},
	}
	if len(sink.got) != len(want) || sink.got[0] != want[0] || sink.got[1] != want[1] {
		t.Fatalf("records=%+v", sink.got)
	}
}

// The item shapes real `codex exec --json` turns print (redacted samples written by hand in their shape): an MCP tool's result is its text
// content, its error an object with a message, a file change lists kind and path, the sub-agent (collab) tool is named by its tool, and an
// error item is an error and a retry notice a warning. None of them is an unknown event.
func TestConversationOfRecordedExecShapes(t *testing.T) {
	root := t.TempDir()
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "thread.started", "thread_id": "th1"},
		m{"type": "turn.started"},
		m{"type": "item.started", "item": m{"id": "item_0", "type": "mcp_tool_call", "server": "aimebu", "tool": "room_history", "arguments": m{"room": "r1"}, "result": nil, "error": nil, "status": "in_progress"}},
		m{"type": "item.completed", "item": m{"id": "item_0", "type": "mcp_tool_call", "server": "aimebu", "tool": "room_history", "arguments": m{"room": "r1"},
			"result": m{"content": []m{{"type": "text", "text": "first line"}, {"type": "text", "text": "second line"}}, "structured_content": nil}, "error": nil, "status": "completed"}},
		m{"type": "item.started", "item": m{"id": "item_1", "type": "mcp_tool_call", "server": "aimebu", "tool": "memory_recall", "arguments": m{"query": "q"}, "result": nil, "error": nil, "status": "in_progress"}},
		m{"type": "item.completed", "item": m{"id": "item_1", "type": "mcp_tool_call", "server": "aimebu", "tool": "memory_recall", "arguments": m{"query": "q"},
			"result": nil, "error": m{"message": "tool call failed"}, "status": "failed"}},
		m{"type": "item.started", "item": m{"id": "item_2", "type": "file_change", "changes": []m{{"kind": "update", "path": "/work/a.go"}, {"kind": "delete", "path": "/work/b.go"}}, "status": "in_progress"}},
		m{"type": "item.completed", "item": m{"id": "item_2", "type": "file_change", "changes": []m{{"kind": "update", "path": "/work/a.go"}, {"kind": "delete", "path": "/work/b.go"}}, "status": "completed"}},
		m{"type": "item.started", "item": m{"id": "item_3", "type": "collab_tool_call", "tool": "wait", "sender_thread_id": "th1", "receiver_thread_ids": []string{}, "prompt": nil, "agents_states": m{}, "status": "in_progress"}},
		m{"type": "item.completed", "item": m{"id": "item_3", "type": "collab_tool_call", "tool": "wait", "sender_thread_id": "th1", "receiver_thread_ids": []string{}, "prompt": nil, "agents_states": m{}, "status": "completed"}},
		m{"type": "item.completed", "item": m{"id": "item_4", "type": "error", "message": "a tool was not available"}},
		m{"type": "error", "message": "Reconnecting... 1/5 (request timed out)"},
		m{"type": "item.completed", "item": m{"id": "item_5", "type": "agent_message", "text": `{"messages":[]}`}},
		m{"type": "turn.completed", "usage": m{"input_tokens": 10, "cached_input_tokens": 0, "output_tokens": 2, "reasoning_output_tokens": 0}},
	)
	path := filepath.Join(root, "codex")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.Command, a.AllowNative, a.AuthPath = path, true, ""
	spec := adapter.TurnSpec{TurnID: "conv", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: adapter.ModeCorrection}
	sameRecords(t, adaptertest.ConversationRecords(t, a, spec), []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationToolCall, Text: `aimebu.room_history {"room":"r1"}`},
		{Kind: adapter.ConversationToolResult, Text: "aimebu.room_history\nfirst line\nsecond line"},
		{Kind: adapter.ConversationToolCall, Text: `aimebu.memory_recall {"query":"q"}`},
		{Kind: adapter.ConversationToolResult, Text: "aimebu.memory_recall (failed)\ntool call failed"},
		{Kind: adapter.ConversationToolCall, Text: "file_change update /work/a.go\ndelete /work/b.go"},
		{Kind: adapter.ConversationToolResult, Text: "file_change\ncompleted"},
		{Kind: adapter.ConversationToolCall, Text: "collab.wait"},
		{Kind: adapter.ConversationToolResult, Text: "collab.wait\ncompleted"},
		{Kind: adapter.ConversationError, Text: "a tool was not available"},
		{Kind: adapter.ConversationEvent, Text: "warning: Reconnecting... 1/5 (request timed out)"},
		{Kind: adapter.ConversationAssistant, Text: `{"messages":[]}`},
	})
}

// The same tools as the app-server spells them: camelCase keys, a file change's kind an object, structured content when a result has no
// text block, and a collab call with a prompt and the threads it addresses.
func TestConversationOfAppServerToolShapes(t *testing.T) {
	sink := &records{}
	conv := adapter.NewConversation(sink)
	item := func(value m) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		transcribeItem(conv, "item/completed", raw, true)
	}
	item(m{"id": "m1", "type": "mcpToolCall", "server": "aimebu", "tool": "list_rooms", "arguments": m{}, "status": "completed",
		"result": m{"content": []m{}, "structuredContent": m{"rooms": []string{"r1"}}}, "error": nil})
	item(m{"id": "m2", "type": "mcpToolCall", "server": "aimebu", "tool": "react", "arguments": m{"no": 1}, "status": "completed",
		"result": m{"content": []m{{"type": "image", "data": "AAAA"}}, "structuredContent": nil}, "error": nil})
	item(m{"id": "f1", "type": "fileChange", "changes": []m{{"path": "/work/c.go", "kind": m{"type": "add"}, "diff": "+x"}}, "status": "completed"})
	item(m{"id": "g1", "type": "collabAgentToolCall", "tool": "spawnAgent", "prompt": "check the tests", "receiverThreadIds": []string{"th2"}, "agentsStates": m{"th2": m{"status": "running"}}, "status": "completed"})
	sameRecords(t, sink.got, []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationToolCall, Text: "aimebu.list_rooms"},
		{Kind: adapter.ConversationToolResult, Text: `aimebu.list_rooms` + "\n" + `{"rooms":["r1"]}`},
		{Kind: adapter.ConversationToolCall, Text: `aimebu.react {"no":1}`},
		{Kind: adapter.ConversationToolResult, Text: `aimebu.react` + "\n" + `{"content":[{"data":"AAAA","type":"image"}],"structuredContent":null}`},
		{Kind: adapter.ConversationToolCall, Text: "file_change add /work/c.go"},
		{Kind: adapter.ConversationToolResult, Text: "file_change\ncompleted"},
		{Kind: adapter.ConversationToolCall, Text: `collab.spawnAgent {"prompt":"check the tests","receiverThreadIds":["th2"]}`},
		{Kind: adapter.ConversationToolResult, Text: "collab.spawnAgent\ncompleted\n" + `{"th2":{"status":"running"}}`},
	})
}

// sameRecords compares records one by one, without the contract's checks of a cut result and the kinds a whole turn writes.
func sameRecords(t *testing.T, got, want []adaptertest.ConversationRecord) {
	t.Helper()
	for i := 0; i < len(got) || i < len(want); i++ {
		switch {
		case i >= len(got):
			t.Errorf("record %d is missing: want %+v", i, want[i])
		case i >= len(want):
			t.Errorf("record %d is not expected: %+v", i, got[i])
		case got[i] != want[i]:
			t.Errorf("record %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
