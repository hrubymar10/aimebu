package main

import "testing"

func TestAgentTerminalTitle(t *testing.T) {
	const base = "pi-docker"
	const id = "drew@aimebu"

	tests := []struct {
		name    string
		state   agentTitleState
		agentID string
		base    string
		want    string
	}{
		{
			name:    "starting before registration",
			state:   agentTitleStarting,
			agentID: "",
			base:    base,
			want:    "pi-docker (starting…)",
		},
		{
			name:    "registered shows full agent id and base",
			state:   agentTitleRegistered,
			agentID: id,
			base:    base,
			want:    "drew@aimebu - pi-docker",
		},
		{
			name:    "exited prefixes registered title with cross",
			state:   agentTitleExited,
			agentID: id,
			base:    base,
			want:    "✗ drew@aimebu - pi-docker",
		},
		{
			name:    "registered with empty id degrades to starting",
			state:   agentTitleRegistered,
			agentID: "",
			base:    base,
			want:    "pi-docker (starting…)",
		},
		{
			name:    "exited with empty id degrades to crossed starting",
			state:   agentTitleExited,
			agentID: "",
			base:    base,
			want:    "✗ pi-docker (starting…)",
		},
		{
			name:    "claude-docker renders distinctly from pi",
			state:   agentTitleRegistered,
			agentID: "hal@aimebu",
			base:    "claude-docker",
			want:    "hal@aimebu - claude-docker",
		},
		{
			name:    "unknown state falls back to base",
			state:   agentTitleState(99),
			agentID: id,
			base:    base,
			want:    base,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := agentTerminalTitle(tc.state, tc.agentID, tc.base)
			if got != tc.want {
				t.Fatalf("agentTerminalTitle(%d, %q, %q) = %q, want %q",
					tc.state, tc.agentID, tc.base, got, tc.want)
			}
		})
	}
}

func TestAgentTitleBase(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		want    string
	}{
		{name: "absolute path takes base", command: []string{"/usr/local/bin/pi-docker"}, want: "pi-docker"},
		{name: "bare command passes through", command: []string{"pi-docker"}, want: "pi-docker"},
		{name: "claude-docker distinct from claude", command: []string{"claude-docker"}, want: "claude-docker"},
		{name: "relative path takes base", command: []string{"./codex-docker"}, want: "codex-docker"},
		{name: "empty command yields empty base", command: []string{}, want: ""},
		{name: "nil command yields empty base", command: nil, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := agentTitleBase(tc.command)
			if got != tc.want {
				t.Fatalf("agentTitleBase(%v) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}
