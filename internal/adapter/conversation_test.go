package adapter

import (
	"strings"
	"testing"
	"unicode/utf8"
)

type conversationRecords struct{ got []string }

func (r *conversationRecords) Record(kind ConversationKind, text string) {
	r.got = append(r.got, string(kind)+"|"+text)
}

// Every text a record carries is masked and bounded where it is written, whatever adapter wrote it.
func TestConversationMasksAndBoundsEveryRecord(t *testing.T) {
	sink := &conversationRecords{}
	conv := NewConversation(sink)
	conv.TurnStart("turn t1")
	conv.Prompt(`[{"body":"use Bearer abc.def.ghi and aak_live_key"}]`)
	conv.Assistant("my key is sk-ant-0123456789abcdefghij")
	conv.Assistant("my key is sk-ant-0123456789abcdefghij") // the same message reported twice
	conv.Assistant("   ")
	conv.ToolCall("c1", "Bash  tool", `{"command":"export GH_TOKEN=ghp_0123456789abcdefghij0123"}`)
	conv.ToolResult("c1", "", "password=hunter2 ok", false)
	conv.ToolResult("c9", "", "no call was seen", true)
	conv.ToolCall("", "", "{}")
	conv.Error("Authorization: Basic dXNlcjpwYXNz")
	conv.TurnEnd("answered, exit 0")
	want := []string{
		"turn_start|turn t1",
		`prompt|[{"body":"use Bearer [redacted] and [redacted]"}]`,
		"assistant|my key is [redacted]",
		`tool_call|Bash_tool {"command":"export GH_TOKEN=[redacted]"}`,
		"tool_result|Bash_tool\npassword=[redacted] ok",
		"tool_result|tool (failed)\nno call was seen",
		"tool_call|tool",
		"error|Authorization: Basic [redacted]",
		"turn_end|answered, exit 0",
	}
	if strings.Join(sink.got, "\n--\n") != strings.Join(want, "\n--\n") {
		t.Fatalf("records:\n%s", strings.Join(sink.got, "\n--\n"))
	}
	for _, record := range sink.got {
		for _, secret := range []string{"abc.def.ghi", "aak_live_key", "sk-ant-", "ghp_", "hunter2", "dXNlcjpwYXNz"} {
			if strings.Contains(record, secret) {
				t.Fatalf("%q is in %q", secret, record)
			}
		}
	}
}

func TestConversationCutsALongTextAtACharacterAndSaysHowMuch(t *testing.T) {
	text := strings.Repeat("é", ConversationToolLimit) // two bytes each: the limit falls between or inside characters
	got := CleanConversationText("x"+text, ConversationToolLimit)
	body, marker, found := strings.Cut(got, "\n... ")
	if !found || !utf8.ValidString(body) || len(body) > ConversationToolLimit || len(body) < ConversationToolLimit-4 {
		t.Fatalf("body %d bytes, valid=%t, marker=%q", len(body), utf8.ValidString(body), marker)
	}
	if want := strings.TrimSuffix(marker, " bytes cut"); want == marker || len(body)+atoi(t, want) != len(text)+1 {
		t.Fatalf("marker %q does not account for %d of %d bytes", marker, len(body), len(text)+1)
	}
	if short := "fits"; CleanConversationText(short, ConversationToolLimit) != short {
		t.Fatal("a short text changed")
	}
	// A secret that straddles the limit is not cut into a readable half.
	padded := strings.Repeat("a", ConversationToolLimit-10) + " aak_0123456789abcdefghijklmnop tail"
	if got := CleanConversationText(padded, ConversationToolLimit); strings.Contains(got, "aak_") {
		t.Fatalf("half a key survived: %q", got[len(got)-80:])
	}
}

func atoi(t *testing.T, value string) int {
	t.Helper()
	n := 0
	for _, c := range value {
		if c < '0' || c > '9' {
			t.Fatalf("not a number: %q", value)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// A turn with no sink, and a keep-alive ping, write nothing: every method is safe on the nil writer.
func TestConversationWithoutASink(t *testing.T) {
	sink := &conversationRecords{}
	for _, conv := range []*Conversation{NewConversation(nil), ConversationFor(TurnSpec{}), ConversationFor(TurnSpec{KeepAlive: true, ConversationSink: sink})} {
		if conv.Active() || conv.Called("c1") {
			t.Fatal("a conversation without a sink is active")
		}
		conv.TurnStart("x")
		conv.Prompt("x")
		conv.Assistant("x")
		conv.Thinking("x")
		conv.ToolCall("c1", "t", "x")
		conv.ToolResult("c1", "t", "x", false)
		conv.Error("x")
		conv.TurnEnd("x")
	}
	if len(sink.got) != 0 {
		t.Fatalf("a keep-alive ping wrote %v", sink.got)
	}
	if !ConversationFor(TurnSpec{ConversationSink: sink}).Active() {
		t.Fatal("a turn with a sink has no conversation")
	}
}
