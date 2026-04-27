package mcp

import "testing"

func TestDetectHarness(t *testing.T) {
	// Clear all env vars detectHarness may consult, then re-set per case.
	clear := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{
			"AIMEBU_HARNESS",
			"CLAUDECODE",
			"CLAUDE_CODE_ENTRYPOINT",
			"CURSOR_TRACE_ID",
			"CURSOR_SESSION_ID",
			"AIDER_VERSION",
		} {
			t.Setenv(k, "")
		}
	}

	tests := []struct {
		name string
		// env to set after clear; key→value. Empty map means "no env vars".
		env  map[string]string
		want string
	}{
		// Tier 2 — owned AIMEBU_HARNESS env var (post-AI fallback).
		{name: "aimebu_harness codex", env: map[string]string{"AIMEBU_HARNESS": "codex"}, want: "codex"},
		{name: "aimebu_harness cline", env: map[string]string{"AIMEBU_HARNESS": "cline"}, want: "cline"},
		{name: "aimebu_harness pi", env: map[string]string{"AIMEBU_HARNESS": "pi"}, want: "pi"},
		// AIMEBU_HARNESS wins over upstream env sniff.
		{name: "aimebu_harness wins over claudecode", env: map[string]string{"AIMEBU_HARNESS": "codex", "CLAUDECODE": "1"}, want: "codex"},

		// Tier 3 — upstream env sniff for harnesses that actually propagate.
		{name: "claudecode", env: map[string]string{"CLAUDECODE": "1"}, want: "claude-code"},
		{name: "claude_code_entrypoint", env: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}, want: "claude-code"},
		{name: "cursor trace id", env: map[string]string{"CURSOR_TRACE_ID": "abc"}, want: "cursor"},
		{name: "cursor session id", env: map[string]string{"CURSOR_SESSION_ID": "abc"}, want: "cursor"},
		{name: "aider", env: map[string]string{"AIDER_VERSION": "0.1"}, want: "aider"},

		// Tier 4 — nothing set.
		{name: "unknown", env: map[string]string{}, want: "unknown"},

		// Codex env vars are deliberately NOT in tier 3 anymore — codex doesn't
		// propagate them to MCP children, so detection by env was misleading.
		// Setting them alone should yield "unknown".
		{name: "codex session_id alone falls through to unknown", env: map[string]string{"CODEX_SESSION_ID": "abc"}, want: "unknown"},
		{name: "codex thread_id alone falls through to unknown", env: map[string]string{"CODEX_THREAD_ID": "abc"}, want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clear(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := detectHarness(); got != tt.want {
				t.Fatalf("detectHarness() = %q, want %q", got, tt.want)
			}
		})
	}
}
