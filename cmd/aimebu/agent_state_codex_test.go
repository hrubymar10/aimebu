package main

import "testing"

func TestCodexStateDetectorDetect(t *testing.T) {
	det := codexStateDetector{}
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "mcp tool call started",
			line: `{"type":"item.started","item":{"type":"mcp_tool_call","name":"mcp__aimebu__bus_wait"}}`,
			want: "tool_call",
		},
		{
			name: "command execution started",
			line: `{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}`,
			want: "tool_call",
		},
		{
			name: "file change started",
			line: `{"type":"item.started","item":{"type":"file_change","path":"cmd/aimebu/agent.go"}}`,
			want: "tool_call",
		},
		{
			name: "bus wait started",
			line: `{"type":"item.started","item":{"type":"bus_wait","room":"aimebu"}}`,
			want: "idle",
		},
		{
			name: "bus wait completed with messages",
			line: `{"type":"item.completed","item":{"type":"bus_wait","result":{"messages":[{"id":201,"body":"work"}]}}}`,
			want: "thinking",
		},
		{
			name: "bus wait completed without messages",
			line: `{"type":"item.completed","item":{"type":"bus_wait","result":{"messages":[]}}}`,
			want: "",
		},
		{
			name: "agent message completed",
			line: `{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`,
			want: "idle",
		},
		{
			name: "unknown item",
			line: `{"type":"item.started","item":{"type":"reasoning"}}`,
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

func TestNewStateDetectorCodex(t *testing.T) {
	if got := newStateDetector("codex"); got == nil || got.Name() != "codex" {
		t.Fatalf("newStateDetector(codex) = %T, want codex detector", got)
	}
}
