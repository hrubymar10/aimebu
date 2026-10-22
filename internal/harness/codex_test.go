package harness

import "testing"

func TestCodexEventClassification(t *testing.T) {
	event, known := ParseCodex([]byte(`{"type":"thread.started","thread_id":"thread-1"}`))
	if !known || CodexThreadID(event) != "thread-1" {
		t.Fatalf("thread event=%+v known=%t", event, known)
	}
	event, known = ParseCodex([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"{\"messages\":[]}"}}`))
	text, ok := CodexAgentMessage(event)
	if !known || !ok || text != `{"messages":[]}` {
		t.Fatalf("message event=%+v text=%q ok=%t", event, text, ok)
	}
	event, known = ParseCodex([]byte(`{"type":"turn.completed","usage":{"input_tokens":123,"output_tokens":7}}`))
	in, out := CodexUsage(event)
	if !known || in == nil || *in != 123 || out == nil || *out != 7 {
		t.Fatalf("usage event=%+v input=%v output=%v", event, in, out)
	}
	if _, known := ParseCodex([]byte(`{"type":"future.event"}`)); known {
		t.Fatal("unknown event reported as supported")
	}
	if _, known := ParseCodex([]byte(`not-json`)); known {
		t.Fatal("invalid JSON reported as supported")
	}
}

func TestCodexCachedInputIsReadOnlyWhenReported(t *testing.T) {
	read := func(line string) *int64 {
		event, _ := ParseCodex([]byte(line))
		return CodexCachedInput(event)
	}
	if got := read(`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":7}}`); got == nil || *got != 80 {
		t.Fatalf("cached input: %v", got)
	}
	if got := read(`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":7}}`); got != nil {
		t.Fatalf("a turn that does not report it has none: %v", *got)
	}
	if got := read(`{"type":"turn.started","usage":{"cached_input_tokens":5}}`); got != nil {
		t.Fatalf("only a completed turn reports usage: %v", *got)
	}
}
