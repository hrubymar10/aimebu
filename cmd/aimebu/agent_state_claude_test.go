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
			name: "star spinner with cursor movement",
			line: "\x1b[2K\x1b[1G* Thinking…",
			want: "thinking",
		},
		{
			name: "heavy spinner",
			line: "\x1b[2K\x1b[1G✻ Thinking",
			want: "thinking",
		},
		{
			name: "six pointed spinner",
			line: "\x1b[2K\x1b[1G✶ Processing",
			want: "thinking",
		},
		{
			name: "pinwheel spinner",
			line: "\x1b[2K\x1b[1G✽ Working",
			want: "thinking",
		},
		{
			name: "diamond spinner",
			line: "\x1b[2K\x1b[1G✢ Reading",
			want: "thinking",
		},
		{
			name: "input ready canary",
			line: "\x1b[2K\x1b[1G❯ ",
			want: "idle",
		},
		{
			name: "plain markdown star is ignored",
			line: "* bullet from transcript",
			want: "",
		},
		{
			name: "ansi clutter is ignored",
			line: "\x1b[?25l\x1b[?2004h",
			want: "",
		},
		{
			name: "ordinary text is ignored",
			line: "Welcome to Claude Code",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := det.Detect([]byte(tt.line)); got != tt.want {
				t.Fatalf("Detect(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestNewStateDetectorClaudeCode(t *testing.T) {
	if got := newStateDetector("claude-code"); got == nil || got.Name() != "claude-code" {
		t.Fatalf("newStateDetector(claude-code) = %T, want claude-code detector", got)
	}
}
