package harness

import "testing"

func TestParseClaude(t *testing.T) {
	event, known := ParseClaude([]byte(` {"type":"result","result":"{}"} `))
	if !known || event.Type != "result" {
		t.Fatalf("ParseClaude = %#v, %v", event, known)
	}
	if _, known := ParseClaude([]byte(`{"type":"future"}`)); known {
		t.Fatal("unknown type treated as known")
	}
	if _, known := ParseClaude([]byte(`not json`)); known {
		t.Fatal("non-json treated as known")
	}
}

func TestParseClaudeCompaction(t *testing.T) {
	event, _ := ParseClaude([]byte(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"pre_tokens":120,"post_tokens":40}}`))
	got, ok := ParseClaudeCompaction(event)
	if !ok || got.PreTokens != 120 || got.PostTokens != 40 {
		t.Fatalf("ParseClaudeCompaction = %#v, %v", got, ok)
	}
}
