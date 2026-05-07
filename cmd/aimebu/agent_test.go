package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestAgentNamePattern(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"alice", true},
		{"otto", true},
		{"abc", true},
		{"abcdefghijkl", true},   // 12 chars
		{"ab", false},            // too short
		{"abcdefghijklm", false}, // 13 chars
		{"Alice", false},         // uppercase
		{"al1ce", false},         // digit
		{"al-ce", false},         // hyphen
		{"", false},
	}
	for _, tc := range cases {
		got := agentNamePattern.MatchString(tc.input)
		if got != tc.want {
			t.Errorf("agentNamePattern.MatchString(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestAgentResolveResume(t *testing.T) {
	sessions := []agentSession{
		{SessionID: "uuid-alice", Name: "alice", Harness: "claude-code", CWD: "/proj", LastUsed: time.Now()},
		{SessionID: "uuid-bob", Name: "bob", Harness: "codex", CWD: "/proj", LastUsed: time.Now()},
	}

	t.Run("resume-id hit", func(t *testing.T) {
		e, err := agentResolveResume("uuid-alice", "", "", "claude-code", sessions)
		if err != nil {
			t.Fatal(err)
		}
		if e.Name != "alice" {
			t.Errorf("got name %q, want alice", e.Name)
		}
	})

	t.Run("resume-id miss with --name escape hatch", func(t *testing.T) {
		e, err := agentResolveResume("uuid-unknown", "", "carol", "claude-code", sessions)
		if err != nil {
			t.Fatal(err)
		}
		if e.Name != "carol" || e.SessionID != "uuid-unknown" {
			t.Errorf("got {Name:%q SessionID:%q}, want {carol uuid-unknown}", e.Name, e.SessionID)
		}
	})

	t.Run("resume-id miss without --name errors with hint", func(t *testing.T) {
		_, err := agentResolveResume("uuid-unknown", "", "", "claude-code", sessions)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if got := err.Error(); !contains(got, "--name") {
			t.Errorf("error %q does not mention --name", got)
		}
	})

	t.Run("resume-name hit", func(t *testing.T) {
		e, err := agentResolveResume("", "alice", "", "claude-code", sessions)
		if err != nil {
			t.Fatal(err)
		}
		if e.SessionID != "uuid-alice" {
			t.Errorf("got session %q, want uuid-alice", e.SessionID)
		}
	})

	t.Run("resume-name miss errors with bootstrap hint", func(t *testing.T) {
		_, err := agentResolveResume("", "carol", "", "claude-code", sessions)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if got := err.Error(); !contains(got, "carol") {
			t.Errorf("error %q does not mention the name", got)
		}
	})

	t.Run("resume-id harness mismatch errors", func(t *testing.T) {
		_, err := agentResolveResume("uuid-alice", "", "", "codex", sessions)
		if err == nil {
			t.Fatal("expected error on harness mismatch, got nil")
		}
	})

	t.Run("resume-name harness mismatch errors", func(t *testing.T) {
		_, err := agentResolveResume("", "bob", "", "claude-code", sessions)
		if err == nil {
			t.Fatal("expected error on harness mismatch, got nil")
		}
	})
}

func TestAgentCommandSetsProcessGroup(t *testing.T) {
	cmd := agentCommand([]string{"echo"}, []string{"hello"}, nil, io.Discard, io.Discard)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("agentCommand should isolate the child in its own process group")
	}
}

func TestAgentFullID(t *testing.T) {
	if got := agentFullID("worker@aimebu"); got != "worker@aimebu" {
		t.Fatalf("agentFullID kept full ID as %q, want %q", got, "worker@aimebu")
	}
	if got := agentFullID("worker"); got != "worker@aimebu" {
		t.Fatalf("agentFullID derived %q, want %q", got, "worker@aimebu")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
