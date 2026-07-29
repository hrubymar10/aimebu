package main

import "testing"

func TestClaudeCodeStateDetectorDetect(t *testing.T) {
	det := claudeCodeStateDetector{}
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "assistant tool use",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__aimebu__bus_wait"}]}}`,
			want: "tool_call",
		},
		{
			name: "assistant text",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`,
			want: "thinking",
		},
		{
			name: "assistant thinking",
			line: `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"..."}]}}`,
			want: "thinking",
		},
		{
			name: "system requesting",
			line: `{"type":"system","subtype":"status","status":"requesting"}`,
			want: "thinking",
		},
		{
			name: "stream message start nested",
			line: `{"type":"stream_event","event":{"type":"message_start"}}`,
			want: "thinking",
		},
		{
			name: "result",
			line: `{"type":"result","subtype":"success","session_id":"claude-session-123"}`,
			want: "idle",
		},
		{
			name: "tool result ignored",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
			want: "",
		},
		{
			name: "rate limit ignored",
			line: `{"type":"rate_limit_event","message":"limit updated"}`,
			want: "",
		},
		{
			name: "invalid json",
			line: `not-json`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := det.Detect([]byte(tt.line)); got != tt.want {
				t.Fatalf("Detect(%s) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestClaudeCodeStateDetectorCapturedTranscript(t *testing.T) {
	det := claudeCodeStateDetector{}
	lines := readStateFixture(t, "testdata/claude-state-sanitized.jsonl")
	want := []string{"thinking", "tool_call", "idle"}
	for i, line := range lines {
		if got := det.Detect([]byte(line)); got != want[i] {
			t.Fatalf("Detect captured line %d = %q, want %q", i+1, got, want[i])
		}
	}
}

func TestNewStateDetectorClaudeCode(t *testing.T) {
	if got := newStateDetector("claude-code"); got == nil || got.Name() != "claude-code" {
		t.Fatalf("newStateDetector(claude-code) = %T, want claude-code detector", got)
	}
}
