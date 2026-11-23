package claude

import (
	"path/filepath"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/v2/internal/harness"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

type m = map[string]any

// A recorded stream-json turn gives the conversation records: thinking, text, each tool call with its input and each result under its
// tool's name, with secrets masked and a long result cut.
func TestConversationConformance(t *testing.T) {
	root := t.TempDir()
	long := adaptertest.ConversationLongOutput()
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "system", "subtype": "init", "session_id": "s1"},
		m{"type": "assistant", "message": m{"id": "m1", "content": []m{
			{"type": "thinking", "thinking": "plan the check"},
			{"type": "text", "text": "I will run a command."},
			{"type": "tool_use", "id": "t1", "name": "Bash", "input": m{"command": adaptertest.ConversationCommand}}}}},
		m{"type": "user", "message": m{"content": []m{{"type": "tool_result", "tool_use_id": "t1", "content": adaptertest.ConversationKeyOutput}}}},
		m{"type": "assistant", "message": m{"id": "m2", "content": []m{{"type": "tool_use", "id": "t2", "name": "Read", "input": m{"file_path": "/tmp/big"}}}}},
		m{"type": "user", "message": m{"content": []m{{"type": "tool_result", "tool_use_id": "t2", "is_error": true, "content": []m{{"type": "text", "text": long}}}}}},
		m{"type": "assistant", "message": m{"id": "m3", "content": []m{{"type": "text", "text": "Done."}}}},
		m{"type": "result", "subtype": "success", "is_error": false, "result": `{"messages":[]}`, "session_id": "s1"},
	)
	path := filepath.Join(root, "claude")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, AllowNative: true}
	spec := adapter.TurnSpec{TurnID: "conv", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeCorrection}
	adaptertest.RunConversation(t, a, spec, []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationThinking, Text: "plan the check"},
		{Kind: adapter.ConversationAssistant, Text: "I will run a command."},
		{Kind: adapter.ConversationToolCall, Text: adaptertest.Masked(`Bash {"command":"curl -H \"Authorization: Bearer ` + adaptertest.ConversationBearer + `\" https://example.test/status"}`)},
		{Kind: adapter.ConversationToolResult, Text: "Bash\n" + adaptertest.Masked(adaptertest.ConversationKeyOutput)},
		{Kind: adapter.ConversationToolCall, Text: `Read {"file_path":"/tmp/big"}`},
		{Kind: adapter.ConversationToolResult, Text: "Read (failed)\n" + adaptertest.CutToolOutput(long)},
		{Kind: adapter.ConversationAssistant, Text: "Done."},
	})
}

type records struct {
	got []adaptertest.ConversationRecord
}

func (r *records) Record(kind adapter.ConversationKind, text string) {
	r.got = append(r.got, adaptertest.ConversationRecord{Kind: kind, Text: text})
}

// In a live process the CLI repeats the turn's input: that repeat is not written again, an input the CLI gave the model on its own is,
// and an error result is an error record.
func TestConversationOfALiveProcessSkipsTheRepeatedInput(t *testing.T) {
	sink := &records{}
	live := &turn{echoMarker: `{"turn_id":"t1"`, conv: adapter.NewConversation(sink)}
	for _, line := range []string{
		`{"type":"user","message":{"content":[{"type":"text","text":"{\"turn_id\":\"t1\",\"notes\":[]}"}]}}`,
		`{"type":"user","message":{"content":"a background task ended"}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"sign in again"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"{}"}`,
	} {
		event, _ := harness.ParseClaude([]byte(line))
		live.transcribe(event)
	}
	want := []adaptertest.ConversationRecord{{Kind: adapter.ConversationPrompt, Text: "a background task ended"}, {Kind: adapter.ConversationError, Text: "sign in again"}}
	if len(sink.got) != len(want) || sink.got[0] != want[0] || sink.got[1] != want[1] {
		t.Fatalf("records=%+v", sink.got)
	}
	// A turn with no sink writes nothing and does not fail.
	event, _ := harness.ParseClaude([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}`))
	(&turn{}).transcribe(event)
}

// An event, or a content block, of a type this adapter does not know, a line that is no event, and an event it cannot read each leave
// an event record with the type and none of the content; the turn runs on.
func TestConversationNotesUnknownEvents(t *testing.T) {
	root := t.TempDir()
	stream := adaptertest.ConversationStream(t, root,
		m{"type": "system", "subtype": "init", "session_id": "s1"},
		m{"type": "hologram_frame", "secret": adaptertest.ConversationKey},
		m{"type": "hologram_frame", "secret": adaptertest.ConversationKey},
		"a plain line "+adaptertest.ConversationKey,
		m{"type": "assistant", "message": m{"id": "m1", "content": []m{{"type": "hologram", "data": adaptertest.ConversationKey}, {"type": "text", "text": "Still here."}}}},
		m{"type": "assistant", "message": m{"id": "m2", "content": 42}},
		m{"type": "result", "subtype": "success", "is_error": false, "result": `{"messages":[]}`, "session_id": "s1"},
	)
	path := filepath.Join(root, "claude")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+stream+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, AllowNative: true}
	spec := adapter.TurnSpec{TurnID: "conv", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeCorrection}
	adaptertest.RunUnknownEvents(t, a, spec, []adaptertest.ConversationRecord{
		{Kind: adapter.ConversationEvent, Text: "hologram_frame"},
		{Kind: adapter.ConversationEvent, Text: "(not an event)"},
		{Kind: adapter.ConversationEvent, Text: "assistant block hologram"},
		{Kind: adapter.ConversationAssistant, Text: "Still here."},
		{Kind: adapter.ConversationEvent, Text: "assistant (unreadable)"},
	})
}
