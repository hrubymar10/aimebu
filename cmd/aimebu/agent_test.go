package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/config"
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

func TestAgentClassifyChildResult(t *testing.T) {
	t.Run("registration lost", func(t *testing.T) {
		got := agentClassifyChildResult("codex", nil, []byte("Error: not registered — call bus_register first"))
		if got != agentRecoveryRegistrationLost {
			t.Fatalf("got %q, want %q", got, agentRecoveryRegistrationLost)
		}
	})

	t.Run("server unreachable", func(t *testing.T) {
		got := agentClassifyChildResult("claude-code", nil, []byte("dial tcp 127.0.0.1:9997: connect: connection refused"))
		if got != agentRecoveryServerUnreachable {
			t.Fatalf("got %q, want %q", got, agentRecoveryServerUnreachable)
		}
	})

	t.Run("codex thread missing", func(t *testing.T) {
		stderr := []byte("ERROR codex_core::session: failed to record rollout items:\nthread 1234-abcd not found\n")
		got := agentClassifyChildResult("codex", nil, stderr)
		if got != agentRecoveryCodexThreadMissing {
			t.Fatalf("got %q, want %q", got, agentRecoveryCodexThreadMissing)
		}
	})

	t.Run("normal end", func(t *testing.T) {
		got := agentClassifyChildResult("codex", []byte(`{"type":"turn.completed"}`), nil)
		if got != agentRecoveryNormalEnd {
			t.Fatalf("got %q, want %q", got, agentRecoveryNormalEnd)
		}
	})
}

func TestAgentRoomsContainExpected(t *testing.T) {
	actual := []struct {
		ID string `json:"id"`
	}{
		{ID: "general"},
		{ID: "dev"},
	}
	if !agentRoomsContainExpected(actual, []string{"general"}) {
		t.Fatal("expected room match")
	}
	if agentRoomsContainExpected(actual, []string{"ops"}) {
		t.Fatal("unexpected room match")
	}
}

func TestAgentPreflight(t *testing.T) {
	makeServer := func(roomPayload string, agentsPayload string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/health":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			case "/agents/worker@aimebu/rooms":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(roomPayload))
			case "/agents":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(agentsPayload))
			default:
				http.NotFound(w, r)
			}
		}))
	}

	t.Run("healthy and present in expected room", func(t *testing.T) {
		srv := makeServer(`{"rooms":[{"id":"general"}]}`, `{"agents":[{"id":"worker@aimebu"}]}`)
		defer srv.Close()
		got := agentPreflight(srv.URL, "worker@aimebu", []string{"general"})
		if got != agentRecoveryNormalEnd {
			t.Fatalf("got %q, want %q", got, agentRecoveryNormalEnd)
		}
	})

	t.Run("missing expected room means registration lost", func(t *testing.T) {
		srv := makeServer(`{"rooms":[{"id":"dev"}]}`, `{"agents":[{"id":"worker@aimebu"}]}`)
		defer srv.Close()
		got := agentPreflight(srv.URL, "worker@aimebu", []string{"general"})
		if got != agentRecoveryRegistrationLost {
			t.Fatalf("got %q, want %q", got, agentRecoveryRegistrationLost)
		}
	})

	t.Run("zero-room sessions fall back to agents list", func(t *testing.T) {
		srv := makeServer(`{"rooms":[]}`, `{"agents":[{"id":"worker@aimebu"}]}`)
		defer srv.Close()
		got := agentPreflight(srv.URL, "worker@aimebu", nil)
		if got != agentRecoveryNormalEnd {
			t.Fatalf("got %q, want %q", got, agentRecoveryNormalEnd)
		}
	})

	t.Run("zero-room sessions detect missing registration", func(t *testing.T) {
		srv := makeServer(`{"rooms":[]}`, `{"agents":[]}`)
		defer srv.Close()
		got := agentPreflight(srv.URL, "worker@aimebu", nil)
		if got != agentRecoveryRegistrationLost {
			t.Fatalf("got %q, want %q", got, agentRecoveryRegistrationLost)
		}
	})
}

func TestAgentAdvanceFailure(t *testing.T) {
	last := agentRecoveryNormalEnd
	count := 0

	count = agentAdvanceFailure(agentRecoveryRegistrationLost, &last, count)
	if count != 1 || last != agentRecoveryRegistrationLost {
		t.Fatalf("first failure => count=%d last=%q, want 1/%q", count, last, agentRecoveryRegistrationLost)
	}

	count = agentAdvanceFailure(agentRecoveryRegistrationLost, &last, count)
	if count != 2 {
		t.Fatalf("second identical failure => count=%d, want 2", count)
	}

	count = agentAdvanceFailure(agentRecoveryServerUnreachable, &last, count)
	if count != 1 || last != agentRecoveryServerUnreachable {
		t.Fatalf("class switch => count=%d last=%q, want 1/%q", count, last, agentRecoveryServerUnreachable)
	}

	count = agentAdvanceFailure(agentRecoveryNormalEnd, &last, count)
	if count != 0 || last != agentRecoveryNormalEnd {
		t.Fatalf("normal end => count=%d last=%q, want 0/%q", count, last, agentRecoveryNormalEnd)
	}
}

func TestAgentBuildRecoveryPrompt(t *testing.T) {
	prompt := agentBuildRecoveryPrompt("codex", "", "worker", []string{"general", "dev"})
	if !contains(prompt, `name="worker", force=true`) {
		t.Fatalf("prompt %q does not include forced reclaim", prompt)
	}
	if !contains(prompt, `meta={"protocol":"agent"}`) {
		t.Fatalf("prompt %q does not include protocol-only meta", prompt)
	}
	if !contains(prompt, "Join these rooms: general, dev.") {
		t.Fatalf("prompt %q does not include room joins", prompt)
	}
}

func TestAgentInitMigratesLegacyState(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", rootDir)
	writeAgentFile(t, filepath.Join(rootDir, "agent-sessions.json"), `[{"session_id":"s1","name":"alice"}]`)
	writeAgentFile(t, filepath.Join(rootDir, "agent-warning-acknowledged"), "yes")

	agentInit()

	path := agentSessionsPath()
	if want := filepath.Join(config.AgentsDir(), "agent-sessions.json"); path != want {
		t.Fatalf("agentSessionsPath() = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated session file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != `[{"session_id":"s1","name":"alice"}]` {
		t.Fatalf("migrated sessions = %q", got)
	}
	if _, err := os.Stat(filepath.Join(config.AgentsDir(), agentWarningMarker)); err != nil {
		t.Fatalf("warning marker not migrated: %v", err)
	}
}

func writeAgentFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
