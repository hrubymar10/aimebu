package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hrubymar10/aimebu/internal/config"
)

func TestAgentNamePattern(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		// plain alpha — still valid
		{"alice", true},
		{"otto", true},
		{"abc", true},
		// min length (3)
		{"ab1", true},
		// max length (21)
		{"abcdefghijklmnopqrstu", true},
		// hyphens/underscores mid-name
		{"foo-bar", true},
		{"foo_bar", true},
		{"a-b-c", true},
		{"a_b_c", true},
		// digits mid-name
		{"al1ce", true},
		// too short (2 chars)
		{"ab", false},
		// too long (22 chars)
		{"abcdefghijklmnopqrstuv", false},
		// uppercase not allowed
		{"Alice", false},
		// leading hyphen
		{"-alice", false},
		// leading underscore
		{"_alice", false},
		// trailing hyphen
		{"alice-", false},
		// trailing underscore
		{"alice_", false},
		// empty
		{"", false},
	}
	for _, tc := range cases {
		got := agentNamePattern.MatchString(tc.input)
		if got != tc.want {
			t.Errorf("agentNamePattern.MatchString(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestAgentRoomFromCWD(t *testing.T) {
	got, err := agentRoomFromCWD(filepath.Join(string(filepath.Separator), "Users", "martin", "aimebu"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "aimebu" {
		t.Fatalf("got %q, want aimebu", got)
	}

	if _, err := agentRoomFromCWD(string(filepath.Separator)); err == nil {
		t.Fatal("expected error for filesystem root")
	}
	if _, err := agentRoomFromCWD("."); err == nil {
		t.Fatal("expected error for relative current directory marker")
	}
}

func TestAgentResolveRoomsAutoRoom(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "project-room")
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(projectDir)

	got, err := agentResolveRooms([]string{"general"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "general,project-room" {
		t.Fatalf("got rooms %v, want [general project-room]", got)
	}

	got, err = agentResolveRooms(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "project-room" {
		t.Fatalf("got rooms %v, want single auto room project-room", got)
	}

	got, err = agentResolveRooms([]string{"project-room"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "project-room" {
		t.Fatalf("got rooms %v, want single deduped project-room", got)
	}
}

func TestAgentPushStateFireAndForgetSendsExpectedBody(t *testing.T) {
	seen := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/agents/worker@aimebu/state" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body["state"] != "respawning" {
			t.Errorf("state = %q, want respawning", body["state"])
		}
		seen <- struct{}{}
	}))
	defer srv.Close()

	agentPushState(srv.URL, "worker@aimebu", "respawning")

	select {
	case <-seen:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("server did not receive state push")
	}
}

func TestAgentPushStateShortNameUsesFullAgentID(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "aimebu")
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(projectDir)

	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path
	}))
	defer srv.Close()

	agentPushState(srv.URL, "worker", "respawning")

	select {
	case got := <-seen:
		if got != "/agents/worker@aimebu/state" {
			t.Fatalf("path = %q, want /agents/worker@aimebu/state", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("server did not receive state push")
	}
}

func TestAgentPushStateServerUnreachableSwallowsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	agentPushState(url, "worker@aimebu", "respawning")
}

func TestAgentPushStateEmptyArgsAreNoop(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer srv.Close()

	agentPushState("", "worker@aimebu", "respawning")
	agentPushState(srv.URL, "", "respawning")
	agentPushState(srv.URL, "worker@aimebu", "")

	if calls != 0 {
		t.Fatalf("expected no calls, got %d", calls)
	}
}

func TestAgentResolveResume(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(t.TempDir(), filepath.Base(cwd))
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessions := []agentSession{
		{SessionID: "uuid-alice", Name: "alice", Harness: "claude-code", CWD: projectDir, LastUsed: time.Now()},
		{SessionID: "uuid-bob", Name: "bob", Harness: "codex", CWD: projectDir, LastUsed: time.Now()},
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

	t.Run("resume-name scoped to current project", func(t *testing.T) {
		dir := t.TempDir()
		alphaDir := filepath.Join(dir, "alpha")
		betaDir := filepath.Join(dir, "beta")
		if err := os.Mkdir(alphaDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(betaDir, 0o755); err != nil {
			t.Fatal(err)
		}
		scoped := []agentSession{
			{SessionID: "uuid-alpha", Name: "sam@alpha", Harness: "codex", CWD: alphaDir, LastUsed: time.Now()},
			{SessionID: "uuid-beta", Name: "sam@beta", Harness: "codex", CWD: betaDir, LastUsed: time.Now()},
		}

		t.Chdir(betaDir)
		e, err := agentResolveResume("", "sam", "", "codex", scoped)
		if err != nil {
			t.Fatal(err)
		}
		if e.SessionID != "uuid-beta" {
			t.Errorf("got session %q, want uuid-beta", e.SessionID)
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
	// Pass an unreachable URL so it falls back to the compiled default template.
	prompt := agentBuildRecoveryPrompt("http://127.0.0.1:0", "codex", "", "worker", []string{"general", "dev"}, "", "")
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

func TestAgentBuildBootstrapPromptAssumeRole(t *testing.T) {
	prompt := agentBuildBootstrapPrompt("http://127.0.0.1:0", "codex", "", []string{"general"}, "", "reviewer", "")
	if !contains(prompt, `role_key "reviewer"`) {
		t.Fatalf("prompt %q does not include assume-role key", prompt)
	}
	if contains(prompt, "assign yourself that room role") == false {
		t.Fatalf("prompt %q does not explain role assignment semantics", prompt)
	}
	if !contains(prompt, "immediately send one concise room message") {
		t.Fatalf("prompt %q does not surface assignment failure", prompt)
	}
}

// TestAgentSpawnPrompt_TokenSubstitution verifies that agentApplyPromptTokens
// correctly substitutes prompt tokens, leaves unknown tokens literal, and
// handles empty forceName/roomsSection without breaking output.
func TestAgentSpawnPrompt_TokenSubstitution(t *testing.T) {
	t.Run("all four tokens substituted", func(t *testing.T) {
		tmpl := `harness={{harness}} meta={{meta_json}} force={{force_name}} rooms={{rooms_section}}`
		got := agentApplyPromptTokens(tmpl, "claude-code", `{"k":"v"}`, "alice", "Join these rooms: dev.\n\n", "", "")
		want := `harness="claude-code" meta={"k":"v"} force="alice" rooms=Join these rooms: dev.` + "\n\n"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("unknown token left literal", func(t *testing.T) {
		tmpl := `before {{unknown_token}} after`
		got := agentApplyPromptTokens(tmpl, "codex", `{}`, "", "", "", "")
		if !contains(got, "{{unknown_token}}") {
			t.Fatalf("unknown token was removed from %q", got)
		}
	})

	t.Run("empty forceName and roomsSection produce valid output", func(t *testing.T) {
		tmpl := agentBootstrapTemplate
		got := agentApplyPromptTokens(tmpl, "codex", `{"protocol":"agent"}`, "", "", "", "")
		if contains(got, "{{") {
			t.Fatalf("unreplaced token in output: %q", got)
		}
		if len(got) == 0 {
			t.Fatal("output is empty")
		}
	})
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

// ── PTY interactive mode tests ───────────────────────────────────────────────

func TestAgentBootstrapArgsClaudeCode(t *testing.T) {
	sessionID := "test-session-uuid"
	args := agentBootstrapArgs("claude-code", "the prompt text", sessionID, "http://localhost:9997", []string{"--extra"}, "")
	joined := strings.Join(args, " ")

	for _, must := range []string{
		"--session-id test-session-uuid",
		"--dangerously-skip-permissions",
		"--extra",
	} {
		if !contains(joined, must) {
			t.Errorf("bootstrap args missing %q; got: %s", must, joined)
		}
	}
	// PTY mode: no stream-json flags; prompt goes through the PTY after the ready signal.
	for _, forbidden := range []string{
		"--output-format",
		"--input-format",
		"--verbose",
		"-p ",
		" -p",
		"--resume",
		"--mcp-config",
	} {
		if contains(joined, forbidden) {
			t.Errorf("bootstrap args must not contain %q; got: %s", forbidden, joined)
		}
	}
}

func TestAgentResumeArgsClaudeCode(t *testing.T) {
	sessionID := "test-session-uuid"
	args := agentResumeArgs("claude-code", sessionID, "keep listening", "http://localhost:9997", nil, "")
	joined := strings.Join(args, " ")

	for _, must := range []string{
		"--resume test-session-uuid",
		"--dangerously-skip-permissions",
	} {
		if !contains(joined, must) {
			t.Errorf("resume args missing %q; got: %s", must, joined)
		}
	}
	// PTY mode: no stream-json flags; "keep listening" goes through the PTY after
	// the ready signal. --session-id must NOT appear alongside --resume.
	for _, forbidden := range []string{
		"--output-format",
		"--input-format",
		"--verbose",
		"-p ",
		" -p",
		"--session-id",
		"--mcp-config",
	} {
		if contains(joined, forbidden) {
			t.Errorf("resume args must not contain %q; got: %s", forbidden, joined)
		}
	}
}

func TestAgentClaudeCodeArgsDoNotInjectMCPConfig(t *testing.T) {
	bootstrap := agentBootstrapArgs("claude-code", "prompt", "sid", "http://localhost:9997", nil, "")
	resume := agentResumeArgs("claude-code", "sid", "keep listening", "http://localhost:9997", nil, "")

	for name, args := range map[string][]string{"bootstrap": bootstrap, "resume": resume} {
		for _, arg := range args {
			if arg == "--mcp-config" {
				t.Fatalf("%s args must not inject --mcp-config: %v", name, args)
			}
		}
	}
}

func TestAgentPTYReadySignalConstant(t *testing.T) {
	if agentPTYReadySignal != "← for agents" {
		t.Errorf("agentPTYReadySignal = %q, want %q", agentPTYReadySignal, "← for agents")
	}
}

func TestAgentPTYReadySignalAllowsCursorPositioning(t *testing.T) {
	line := "\x1b[3G\x1b[95m⏵⏵\x1b[6Gbypass\x1b[13Gpermissions\x1b[25Gon\x1b[37m (shift+tab\x1b[39Gto\x1b[42Gcycle)\x1b[49G·\x1b[51G←\x1b[53Gfor\x1b[57Gagents\x1b[39m\r\r"
	if !agentPTYHasReadySignal([]byte(line)) {
		t.Fatalf("split-rendered ready signal was not detected: %q", line)
	}
}

func TestAgentPTYWaitCanaryCrashesWithMeaningfulErrorOnModal(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	var copied bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- agentPTYWaitCanary(master, &copied, 50*time.Millisecond)
	}()

	modal := "\r\r" +
		"\x1b[97m────────────────────────────────────────────────────────────────────────────────\x1b[39m\r\r" +
		"\x1b[3G\x1b[97m\x1b[1mTry\x1b[7Gthe\x1b[11Gnew\x1b[15Gfullscreen\x1b[26Grenderer?\x1b[22m\x1b[39m\r\r" +
		"\r\r" +
		"\x1b[3G\x1b[37m·\x1b[5GFlicker-free\x1b[18Goutput\x1b[25G—\x1b[27Gfixes\x1b[33Gthe\x1b[37Gflashing\x1b[46Gyou\x1b[50Gsee\x1b[54Gduring\x1b[61Glong\x1b[66Gresponses\x1b[39m\r\r" +
		"\x1b[3G\x1b[37m·\x1b[5GMouse\x1b[11Gsupport\x1b[19G—\x1b[21Gclick\x1b[27Gto\x1b[30Gmove\x1b[35Gyour\x1b[40Gcursor\x1b[47Gor\x1b[50Gexpand\x1b[57Gresults\x1b[39m\r\r" +
		"\x1b[3G\x1b[37m·\x1b[5GSelected\x1b[14Gtext\x1b[19Gauto-copies\x1b[31Gto\x1b[34Gyour\x1b[39Gclipboard\x1b[39m\r\r" +
		"\r\r" +
		"\x1b[3G\x1b[97m❯\x1b[5G\x1b[37m1.\x1b[8G\x1b[97mYes,\x1b[13Gtry\x1b[17Git\x1b[39m\r\r" +
		"\x1b[5G\x1b[37m2.\x1b[8G\x1b[39mNot\x1b[12Gnow\r\r" +
		"\r\r" +
		"\x1b[3G\x1b[37m\x1b[3mEnter\x1b[9Gto\x1b[12Gconfirm\x1b[20G·\x1b[22GEsc\x1b[26Gto\x1b[29Gcancel\x1b[23m\x1b[39m\r\r"
	if _, err := io.WriteString(slave, modal); err != nil {
		t.Fatal(err)
	}

	err = <-done
	if err == nil {
		t.Fatal("expected timeout without ready signal")
	}
	for _, want := range []string{
		"claude-code did not reach its chat composer",
		"first-run prompt",
		"Try the new fullscreen renderer?",
		"will not choose for you",
		"Run `claude` once interactively",
		"-- last screen seen --",
		"Yes, try it",
	} {
		if !contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err.Error(), want)
		}
	}
	if !bytes.Contains(copied.Bytes(), []byte("Try")) {
		t.Fatalf("copied output missing modal text: %q", copied.String())
	}

	_ = slave.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 16)
	n, err := slave.Read(buf)
	if err == nil || n != 0 {
		t.Fatalf("expected no PTY writes from canary, read n=%d bytes=%q err=%v", n, string(buf[:n]), err)
	}
}

func TestAgentPTYWaitCanaryModalOnlyDoesNotSatisfyReady(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	done := make(chan error, 1)
	go func() {
		done <- agentPTYWaitCanary(master, io.Discard, 50*time.Millisecond)
	}()

	modal := "\x1b[3G\x1b[93m\x1b[1mAllow\x1b[9Gexternal\x1b[18GCLAUDE.md\x1b[28Gfile\x1b[33Gimports?\x1b[22m\x1b[39m\r\r" +
		"\x1b[3G\x1b[97m❯\x1b[5G\x1b[37m1.\x1b[8G\x1b[97mYes,\x1b[13Gallow\x1b[19Gexternal\x1b[28Gimports\x1b[39m\r\r"
	if _, err := io.WriteString(slave, modal); err != nil {
		t.Fatal(err)
	}

	err = <-done
	if err == nil {
		t.Fatal("expected timeout without ready signal")
	}
	if !contains(err.Error(), "first-run prompt") {
		t.Fatalf("error = %q, want first-run prompt guidance", err)
	}
}

func TestAgentPTYBackoff(t *testing.T) {
	d := time.Second
	d = agentPTYBackoff(d)
	if d != 2*time.Second {
		t.Fatalf("first double: got %v, want 2s", d)
	}
	for range 20 {
		d = agentPTYBackoff(d)
	}
	if d != agentRecoveryMaxBackoff {
		t.Fatalf("after many doublings: got %v, want %v (agentRecoveryMaxBackoff)", d, agentRecoveryMaxBackoff)
	}
}

func TestAgentBuildEnvStripping(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "vscode")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "token-value")
	t.Setenv("NODE_OPTIONS", "--inspect-brk")
	t.Setenv("VSCODE_INSPECTOR_OPTIONS", "some-value")
	// Inherited MCP_CONNECTION_NONBLOCKING must be overwritten to "true".
	t.Setenv("MCP_CONNECTION_NONBLOCKING", "false")

	env := agentBuildEnv("http://localhost:9997", "claude-code", "abc123")

	envMap := make(map[string]string)
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}

	for _, stripped := range []string{"CLAUDE_CODE_ENTRYPOINT", "NODE_OPTIONS", "VSCODE_INSPECTOR_OPTIONS"} {
		if _, ok := envMap[stripped]; ok {
			t.Errorf("%s should be stripped but is present", stripped)
		}
	}
	if envMap["CLAUDE_CODE_OAUTH_TOKEN"] != "token-value" {
		t.Error("CLAUDE_CODE_OAUTH_TOKEN (auth var) must be preserved")
	}
	if envMap["MCP_CONNECTION_NONBLOCKING"] != "true" {
		t.Errorf("MCP_CONNECTION_NONBLOCKING = %q, want true (inherited false must be overwritten)", envMap["MCP_CONNECTION_NONBLOCKING"])
	}
}

func TestAgentPTYWritePromptSendsSeparateEnter(t *testing.T) {
	oldDelay := agentPTYSubmitDelay
	agentPTYSubmitDelay = 0
	defer func() { agentPTYSubmitDelay = oldDelay }()

	var buf bytes.Buffer
	agentPTYWritePrompt(&buf, "line one\nline two", nil)

	want := "line one\nline two\r"
	if got := buf.String(); got != want {
		t.Fatalf("prompt bytes = %q, want %q", got, want)
	}
}

// TestAgentNoSessionIDParsingForClaudeCode is a regression guard: the old
// protocol extracted session_id from JSON output (-p path). The PTY path
// pre-generates the session ID driver-side, so neither -p nor any output
// parsing should appear in claude-code bootstrap args.
func TestAgentNoSessionIDParsingForClaudeCode(t *testing.T) {
	sid := "pre-generated-uuid-abc"
	args := agentBootstrapArgs("claude-code", "prompt", sid, "http://localhost:9997", nil, "")

	// Pre-generated ID must appear as --session-id value.
	found := false
	for i, a := range args {
		if a == "--session-id" && i+1 < len(args) && args[i+1] == sid {
			found = true
		}
	}
	if !found {
		t.Fatal("pre-generated session ID not found in --session-id arg")
	}
	// -p must not be present (was the old parse-from-output path's delivery vehicle).
	for _, a := range args {
		if a == "-p" {
			t.Fatal("claude-code bootstrap must not use -p; session ID is pre-generated")
		}
	}
}

func TestAgentDebugLogRenamesPreRegisterFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", dir)
	t.Setenv("AIMEBU_AGENT_DEBUG", "yes")

	debug := newAgentDebugLog("", "feedbeefcafebabe")
	if debug == nil || !debug.enabled {
		t.Fatal("expected debug logging to be enabled")
	}
	debug.log("wrapper_start", map[string]any{"resume_mode": "bootstrap"})

	finalPath := filepath.Join(dir, "agents", "agent-logs", "worker.log")
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("{\"event\":\"existing\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	preRegisterPath := filepath.Join(dir, "agents", "agent-logs", "_pre-register-feedbeefcafebabe.log")
	if _, err := os.Stat(preRegisterPath); err != nil {
		t.Fatalf("expected pre-register log %s: %v", preRegisterPath, err)
	}

	if err := debug.setAgentName("worker@aimebu"); err != nil {
		t.Fatal(err)
	}
	debug.log("register_observed", map[string]any{"agent_id": "worker@aimebu"})
	if err := debug.close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("expected final log %s: %v", finalPath, err)
	}
	if _, err := os.Stat(preRegisterPath); !os.IsNotExist(err) {
		t.Fatalf("expected pre-register log to be renamed away, stat err=%v", err)
	}

	records := readAgentDebugRecords(t, finalPath)
	if len(records) != 3 {
		t.Fatalf("expected 3 debug records, got %d", len(records))
	}
	if got := records[0]["event"]; got != "existing" {
		t.Fatalf("first event = %v, want existing", got)
	}
	if got := records[1]["event"]; got != "wrapper_start" {
		t.Fatalf("second event = %v, want wrapper_start", got)
	}
	if got := records[2]["event"]; got != "register_observed" {
		t.Fatalf("third event = %v, want register_observed", got)
	}
	if _, ok := records[1]["ts"].(string); !ok {
		t.Fatalf("wrapper_start record missing ts string: %#v", records[1])
	}
}

func TestAgentBootstrapSessionDebugLogging(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", dir)
	t.Setenv("AIMEBU_AGENT_DEBUG", "1")

	spawnTag := "abc123def4567890"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"agents":[{"id":"worker@aimebu","kind":"ai","meta":{"spawn_tag":"abc123def4567890"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "fake-codex.sh")
	script := "#!/bin/sh\nprintf '{\"type\":\"thread.started\",\"thread_id\":\"thread-123\"}\\n'\nprintf '{\"type\":\"note\",\"message\":\"ready\"}\\n'\n"
	if err := os.WriteFile(harnessPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	debug := newAgentDebugLog("", spawnTag)
	agentLogWrapperStart(debug, []string{"--room", "general", "--", "codex"}, "codex", []string{"general"}, spawnTag, "bootstrap", server.URL, "codex")

	env := agentBuildEnv(server.URL, "codex", spawnTag)
	sessionID, agentID, err := agentBootstrapSession("codex", []string{harnessPath}, "keep listening", "", env, server.URL, spawnTag, "", make(chan os.Signal, 1), debug)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "thread-123" {
		t.Fatalf("sessionID = %q, want thread-123", sessionID)
	}
	if agentID != "worker@aimebu" {
		t.Fatalf("agentID = %q, want worker@aimebu", agentID)
	}
	if err := debug.close(); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "agents", "agent-logs", "worker.log")
	records := readAgentDebugRecords(t, logPath)
	events := debugEventNames(records)
	wantSequence := []string{"wrapper_start", "harness_spawn", "harness_stdout_raw", "harness_exit", "session_id_parsed"}
	assertEventSubsequence(t, events, wantSequence)
	if !contains(eventsCSV(events), "register_observed") {
		t.Fatalf("expected register_observed event in %v", events)
	}

	sessionRecord := firstDebugEvent(records, "session_id_parsed")
	if got := sessionRecord["parsed_id"]; got != "thread-123" {
		t.Fatalf("parsed_id = %v, want thread-123", got)
	}
	if got := sessionRecord["source_line_index"]; got != float64(1) {
		t.Fatalf("source_line_index = %v, want 1", got)
	}

	exitRecord := firstDebugEvent(records, "harness_exit")
	if got := exitRecord["exit_code"]; got != float64(0) {
		t.Fatalf("exit_code = %v, want 0", got)
	}

	stdoutRecord := firstDebugEvent(records, "harness_stdout_raw")
	if got := stdoutRecord["line"]; got != `{"type":"thread.started","thread_id":"thread-123"}` {
		t.Fatalf("first stdout line = %v", got)
	}
}

func TestAgentBootstrapSessionFakePi(t *testing.T) {
	oldTimeout := agentRegistrationLookupTimeout
	agentRegistrationLookupTimeout = 50 * time.Millisecond
	defer func() { agentRegistrationLookupTimeout = oldTimeout }()

	spawnTag := "abc123def4567890"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"agents":[{"id":"piper@aimebu","kind":"ai","meta":{"spawn_tag":"abc123def4567890"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	env := agentBuildEnv(server.URL, "pi", spawnTag)
	sessionID, agentID, err := agentBootstrapSession(
		"pi",
		[]string{filepath.Join("testdata", "fake-pi.sh")},
		"register please",
		"ollama-cloud/gemma4:31b",
		env,
		server.URL,
		spawnTag,
		"",
		make(chan os.Signal, 1),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "pi-session-123" {
		t.Fatalf("sessionID = %q, want pi-session-123", sessionID)
	}
	if agentID != "piper@aimebu" {
		t.Fatalf("agentID = %q, want piper@aimebu", agentID)
	}
}

func TestAgentBootstrapSessionRequiresRegistration(t *testing.T) {
	oldTimeout := agentRegistrationLookupTimeout
	agentRegistrationLookupTimeout = 10 * time.Millisecond
	defer func() { agentRegistrationLookupTimeout = oldTimeout }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"agents":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	harnessDir := t.TempDir()
	codexPath := filepath.Join(harnessDir, "fake-codex.sh")
	codexScript := "#!/bin/sh\nprintf '{\"type\":\"thread.started\",\"thread_id\":\"thread-123\"}\\n'\n"
	if err := os.WriteFile(codexPath, []byte(codexScript), 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(harnessDir, "fake-claude.sh")
	claudeScript := "#!/bin/sh\nprintf '← for agents'\nsleep 0.05\n"
	if err := os.WriteFile(claudePath, []byte(claudeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	piPath := filepath.Join("testdata", "fake-pi.sh")

	for _, tc := range []struct {
		name      string
		harness   string
		command   string
		knownName string
		wantHint  string
	}{
		{name: "codex fresh bootstrap", harness: "codex", command: codexPath, wantHint: "codex mcp list"},
		{name: "codex recovery with known name", harness: "codex", command: codexPath, knownName: "worker", wantHint: "codex mcp list"},
		{name: "claude pty fresh bootstrap", harness: "claude-code", command: claudePath, wantHint: "claude mcp list"},
		{name: "claude pty recovery with known name", harness: "claude-code", command: claudePath, knownName: "worker", wantHint: "claude mcp list"},
		{name: "pi fresh bootstrap", harness: "pi", command: piPath, wantHint: "cat ~/.pi/agent/mcp.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := agentBootstrapSession(
				tc.harness,
				[]string{tc.command},
				"register please",
				"",
				agentBuildEnv(server.URL, tc.harness, "missingregister1234"),
				server.URL,
				"missingregister1234",
				tc.knownName,
				make(chan os.Signal, 1),
				nil,
			)
			if err == nil {
				t.Fatal("expected missing registration error")
			}
			if got := err.Error(); !contains(got, "bus_register") || !contains(got, tc.wantHint) {
				t.Fatalf("error %q does not explain missing MCP registration", got)
			}
		})
	}
}

func TestAgentRegistrationMissingErrorIsHarnessAware(t *testing.T) {
	codexErr := agentRegistrationMissingError("codex").Error()
	if !contains(codexErr, "codex mcp list") || !contains(codexErr, "docs/codex.md") {
		t.Fatalf("codex error is not codex-specific: %q", codexErr)
	}

	claudeErr := agentRegistrationMissingError("claude-code").Error()
	if !contains(claudeErr, "claude mcp list") || !contains(claudeErr, "docs/claude-code.md") {
		t.Fatalf("claude error is not claude-specific: %q", claudeErr)
	}

	piErr := agentRegistrationMissingError("pi").Error()
	if !contains(piErr, "cat ~/.pi/agent/mcp.json") || !contains(piErr, "docs/pi.md") {
		t.Fatalf("pi error is not pi-specific: %q", piErr)
	}

	vibeErr := agentRegistrationMissingError("vibe").Error()
	if !contains(vibeErr, "cat ~/.vibe/config.toml") || !contains(vibeErr, "docs/vibe.md") {
		t.Fatalf("vibe error is not vibe-specific: %q", vibeErr)
	}
}

func TestAgentBootstrapSessionPTYRegistrationStall(t *testing.T) {
	oldStallTimeout := agentPTYRegistrationStallTimeout
	oldLookupTimeout := agentRegistrationLookupTimeout
	agentPTYRegistrationStallTimeout = 20 * time.Millisecond
	agentRegistrationLookupTimeout = 200 * time.Millisecond
	defer func() {
		agentPTYRegistrationStallTimeout = oldStallTimeout
		agentRegistrationLookupTimeout = oldLookupTimeout
	}()

	dir := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", dir)
	t.Setenv("AIMEBU_AGENT_DEBUG", "1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"agents":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	harnessDir := t.TempDir()
	claudePath := filepath.Join(harnessDir, "fake-claude.sh")
	claudeScript := "#!/bin/sh\ntrap 'exit 0' TERM\nprintf '← for agents'\nwhile :; do sleep 1 & wait $!; done\n"
	if err := os.WriteFile(claudePath, []byte(claudeScript), 0o700); err != nil {
		t.Fatal(err)
	}

	spawnTag := "stallabc12345678"
	debug := newAgentDebugLog("", spawnTag)
	_, _, err := agentBootstrapSession(
		"claude-code",
		[]string{claudePath},
		"register please",
		"",
		agentBuildEnv(server.URL, "claude-code", spawnTag),
		server.URL,
		spawnTag,
		"",
		make(chan os.Signal, 1),
		debug,
	)
	if closeErr := debug.close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil {
		t.Fatal("expected registration stall error")
	}
	if got := err.Error(); !contains(got, "bus_register") || !contains(got, "claude mcp list") {
		t.Fatalf("error %q does not explain stalled registration", got)
	}

	logPath := filepath.Join(dir, "agents", "agent-logs", "_pre-register-"+spawnTag+".log")
	records := readAgentDebugRecords(t, logPath)
	if firstDebugEvent(records, "registration_stalled") == nil {
		t.Fatalf("expected registration_stalled event in %#v", records)
	}
}

func TestAgentPiArgs(t *testing.T) {
	// Harvest path: modelSlug is the stripped slug; userArgs has no --model.
	// Wrapper injects --model <stripped-slug> before userArgs.
	bootstrap := agentBootstrapArgs("pi", "register now", "", "http://localhost:9997", []string{"--no-tools"}, "gemma4:31b")
	if got := strings.Join(bootstrap, " "); got != "--mode json --model gemma4:31b --no-tools register now" {
		t.Fatalf("bootstrap args (harvest) = %q", got)
	}

	// No model at all.
	bootstrapNoModel := agentBootstrapArgs("pi", "register now", "", "http://localhost:9997", nil, "")
	if got := strings.Join(bootstrapNoModel, " "); got != "--mode json register now" {
		t.Fatalf("bootstrap args without model = %q", got)
	}

	// Passthrough path: userArgs already has --model; wrapper must NOT inject a second one.
	bootstrapPassthrough := agentBootstrapArgs("pi", "register now", "", "http://localhost:9997", []string{"--model", "ollama-cloud/minimax-m3", "--no-tools"}, "minimax-m3")
	if got := strings.Join(bootstrapPassthrough, " "); got != "--mode json --model ollama-cloud/minimax-m3 --no-tools register now" {
		t.Fatalf("bootstrap args (passthrough) = %q", got)
	}

	// Harvest resume path.
	resume := agentResumeArgs("pi", "pi-session-123", "keep listening", "http://localhost:9997", []string{"--no-tools"}, "gemma4:31b")
	if got := strings.Join(resume, " "); got != "--resume --session pi-session-123 --mode json --model gemma4:31b --no-tools keep listening" {
		t.Fatalf("resume args (harvest) = %q", got)
	}

	// Passthrough resume: no double --model.
	resumePassthrough := agentResumeArgs("pi", "pi-session-123", "keep listening", "http://localhost:9997", []string{"--model", "ollama-cloud/minimax-m3"}, "minimax-m3")
	if got := strings.Join(resumePassthrough, " "); got != "--resume --session pi-session-123 --mode json --model ollama-cloud/minimax-m3 keep listening" {
		t.Fatalf("resume args (passthrough) = %q", got)
	}
}

func TestAgentVibeArgs(t *testing.T) {
	bootstrap := agentBootstrapArgs("vibe", "register now", "", "http://localhost:9997", []string{"--max-turns", "1"}, "mistral-medium-3.5")
	if got := strings.Join(bootstrap, " "); got != "-p register now --output json --yolo --max-turns 1" {
		t.Fatalf("bootstrap args = %q", got)
	}

	resume := agentResumeArgs("vibe", "", "keep listening", "http://localhost:9997", []string{"--trust"}, "mistral-medium-3.5")
	if got := strings.Join(resume, " "); got != "-c -p keep listening --output json --yolo --trust" {
		t.Fatalf("resume args = %q", got)
	}
}

func TestHarnessDetectVibe(t *testing.T) {
	if got := harnessDetect["vibe"]; got != "vibe" {
		t.Fatalf("harnessDetect[vibe] = %q, want vibe", got)
	}
	if got := harnessDetect["vibe-docker"]; got != "vibe" {
		t.Fatalf("harnessDetect[vibe-docker] = %q, want vibe", got)
	}
}

func TestPiModelFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"absent", nil, ""},
		{"space form", []string{"--model", "ollama-cloud/minimax-m3"}, "minimax-m3"},
		{"equals form", []string{"--model=ollama-cloud/minimax-m3"}, "minimax-m3"},
		{"no provider prefix", []string{"--model", "gemma4:31b"}, "gemma4:31b"},
		{"provider with equals in value", []string{"--model=ollama-cloud/m3"}, "m3"},
		{"last wins", []string{"--model", "ollama-cloud/first", "--model", "ollama-cloud/last"}, "last"},
		{"last wins mixed forms", []string{"--model=ollama-cloud/first", "--model", "ollama-cloud/last"}, "last"},
		{"no false positive --model-path", []string{"--model-path", "/some/path"}, ""},
		{"surrounding args preserved", []string{"--no-tools", "--model", "ollama-cloud/minimax-m3", "--verbose"}, "minimax-m3"},
		{"provider prefix stripped on first slash only", []string{"--model", "ollama-cloud/a/b"}, "a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := piModelFromArgs(tc.args); got != tc.want {
				t.Fatalf("piModelFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestCodexModelFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"absent", nil, ""},
		{"short model flag", []string{"-m", "gpt-5"}, "gpt-5"},
		{"long model flag", []string{"--model", "gpt-5.5"}, "gpt-5.5"},
		{"equals model flag", []string{"--model=gpt-5"}, "gpt-5"},
		{"short config flag", []string{"-c", "model=gpt-5"}, "gpt-5"},
		{"long config flag", []string{"--config", "model='gpt-5.5'"}, "gpt-5.5"},
		{"equals config flag", []string{"--config=model=\"gpt-5\""}, "gpt-5"},
		{"short equals config flag", []string{"-c=model=gpt-5"}, "gpt-5"},
		{"last wins mixed forms", []string{"--model", "gpt-5", "-c", "model=gpt-5.5"}, "gpt-5.5"},
		{"ignores other config keys", []string{"--config", "profile=work"}, ""},
		{"no false positive --model-path", []string{"--model-path", "/some/path"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexModelFromArgs(tc.args); got != tc.want {
				t.Fatalf("codexModelFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestAgentHarvestCodexDefaultModel(t *testing.T) {
	dir := t.TempDir()

	t.Run("top-level model", func(t *testing.T) {
		codexHome := filepath.Join(dir, "codex")
		writeAgentFile(t, filepath.Join(codexHome, "config.toml"), "model = \"gpt-5.5\"\n")
		got := agentHarvestCodexDefaultModel([]string{"CODEX_HOME=" + codexHome})
		if got != "gpt-5.5" {
			t.Fatalf("got %q, want gpt-5.5", got)
		}
	})

	t.Run("top-level single quoted model", func(t *testing.T) {
		codexHome := filepath.Join(dir, "quoted")
		writeAgentFile(t, filepath.Join(codexHome, "config.toml"), "model = 'gpt-5'\n")
		got := agentHarvestCodexDefaultModel([]string{"CODEX_HOME=" + codexHome})
		if got != "gpt-5" {
			t.Fatalf("got %q, want gpt-5", got)
		}
	})

	t.Run("profile model ignored", func(t *testing.T) {
		codexHome := filepath.Join(dir, "profile")
		writeAgentFile(t, filepath.Join(codexHome, "config.toml"), "[profiles.work]\nmodel = \"gpt-5.5\"\n")
		got := agentHarvestCodexDefaultModel([]string{"CODEX_HOME=" + codexHome})
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		got := agentHarvestCodexDefaultModel([]string{"CODEX_HOME=" + filepath.Join(dir, "missing")})
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestAgentHarvestPiDefaultModel(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("settings model", func(t *testing.T) {
		writeAgentFile(t, filepath.Join(agentDir, "settings.json"), `{"defaultProvider":"ollama-cloud","defaultModel":"gemma4:31b"}`)
		got := agentHarvestPiDefaultModel([]string{"PI_CODING_AGENT_DIR=" + agentDir})
		if got != "gemma4:31b" {
			t.Fatalf("got %q, want gemma4:31b", got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		got := agentHarvestPiDefaultModel([]string{"PI_CODING_AGENT_DIR=" + filepath.Join(dir, "missing")})
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		badDir := filepath.Join(dir, "bad")
		writeAgentFile(t, filepath.Join(badDir, "settings.json"), `{bad`)
		got := agentHarvestPiDefaultModel([]string{"PI_CODING_AGENT_DIR=" + badDir})
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("partial settings", func(t *testing.T) {
		partialDir := filepath.Join(dir, "partial")
		writeAgentFile(t, filepath.Join(partialDir, "settings.json"), `{"defaultProvider":"ollama-cloud"}`)
		got := agentHarvestPiDefaultModel([]string{"PI_CODING_AGENT_DIR=" + partialDir})
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("settings model without provider", func(t *testing.T) {
		noProviderDir := filepath.Join(dir, "no-provider")
		writeAgentFile(t, filepath.Join(noProviderDir, "settings.json"), `{"defaultModel":"gemma4:31b"}`)
		got := agentHarvestPiDefaultModel([]string{"PI_CODING_AGENT_DIR=" + noProviderDir})
		if got != "gemma4:31b" {
			t.Fatalf("got %q, want gemma4:31b", got)
		}
	})
}

func TestAgentHarvestVibeDefaultModel(t *testing.T) {
	dir := t.TempDir()

	t.Run("top-level active_model", func(t *testing.T) {
		vibeHome := filepath.Join(dir, "vibe")
		writeAgentFile(t, filepath.Join(vibeHome, "config.toml"), "active_model = \"mistral-medium-3.5\"\n")
		got := agentHarvestVibeDefaultModel([]string{"VIBE_HOME=" + vibeHome})
		if got != "mistral-medium-3.5" {
			t.Fatalf("got %q, want mistral-medium-3.5", got)
		}
	})

	t.Run("top-level single quoted active_model", func(t *testing.T) {
		vibeHome := filepath.Join(dir, "quoted")
		writeAgentFile(t, filepath.Join(vibeHome, "config.toml"), "active_model = 'devstral-2'\n")
		got := agentHarvestVibeDefaultModel([]string{"VIBE_HOME=" + vibeHome})
		if got != "devstral-2" {
			t.Fatalf("got %q, want devstral-2", got)
		}
	})

	t.Run("section active_model ignored", func(t *testing.T) {
		vibeHome := filepath.Join(dir, "section")
		writeAgentFile(t, filepath.Join(vibeHome, "config.toml"), "[profile]\nactive_model = \"mistral-medium-3.5\"\n")
		got := agentHarvestVibeDefaultModel([]string{"VIBE_HOME=" + vibeHome})
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		got := agentHarvestVibeDefaultModel([]string{"VIBE_HOME=" + filepath.Join(dir, "missing")})
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestAgentCanHarvestVibeModel(t *testing.T) {
	if !agentCanHarvestVibeModel([]string{"/usr/local/bin/vibe"}) {
		t.Fatal("host vibe should harvest model")
	}
	if agentCanHarvestVibeModel([]string{"/usr/local/bin/vibe-docker"}) {
		t.Fatal("vibe-docker should not harvest host-side model")
	}
	if agentCanHarvestVibeModel(nil) {
		t.Fatal("empty command should not harvest")
	}
}

func TestAgentBootstrapPromptPiHarvestedModel(t *testing.T) {
	prompt := agentBuildBootstrapPrompt("http://127.0.0.1:0", "pi", "", []string{"general"}, "", "", "gemma4:31b")
	if !contains(prompt, `pass model="gemma4:31b" exactly`) {
		t.Fatalf("prompt %q does not include harvested pi model slug", prompt)
	}
	if contains(prompt, "ollama-cloud/gemma4:31b") {
		t.Fatalf("prompt %q unexpectedly includes provider-prefixed pi model", prompt)
	}
}

func TestAgentBootstrapPromptCodexHarvestedModel(t *testing.T) {
	prompt := agentBuildBootstrapPrompt("http://127.0.0.1:0", "codex", "", []string{"general"}, "", "", "gpt-5.5")
	if !contains(prompt, `pass model="gpt-5.5" exactly`) {
		t.Fatalf("prompt %q does not include harvested codex model slug", prompt)
	}

	unknownPrompt := agentBuildBootstrapPrompt("http://127.0.0.1:0", "codex", "", []string{"general"}, "", "", "")
	if contains(unknownPrompt, "pass model=") {
		t.Fatalf("prompt %q unexpectedly includes model instruction", unknownPrompt)
	}
}

func TestAgentBootstrapPromptVibeHarvestedModel(t *testing.T) {
	prompt := agentBuildBootstrapPrompt("http://127.0.0.1:0", "vibe", "", []string{"general"}, "", "", "mistral-medium-3.5")
	if !contains(prompt, `pass model="mistral-medium-3.5" exactly`) {
		t.Fatalf("prompt %q does not include harvested vibe model slug", prompt)
	}
}

func TestAgentBootstrapSessionFakeVibeAllowsEmptySessionID(t *testing.T) {
	oldTimeout := agentRegistrationLookupTimeout
	agentRegistrationLookupTimeout = 50 * time.Millisecond
	defer func() { agentRegistrationLookupTimeout = oldTimeout }()

	spawnTag := "abc123def4567890"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"agents":[{"id":"vivi@aimebu","kind":"ai","meta":{"spawn_tag":"abc123def4567890"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	harnessDir := t.TempDir()
	vibePath := filepath.Join(harnessDir, "fake-vibe.sh")
	script := "#!/bin/sh\nprintf '[{\"role\":\"assistant\",\"content\":\"registered\"}]\\n'\n"
	if err := os.WriteFile(vibePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	env := agentBuildEnv(server.URL, "vibe", spawnTag)
	sessionID, agentID, err := agentBootstrapSession(
		"vibe",
		[]string{vibePath},
		"register please",
		"mistral-medium-3.5",
		env,
		server.URL,
		spawnTag,
		"",
		make(chan os.Signal, 1),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "" {
		t.Fatalf("sessionID = %q, want empty for vibe --continue flow", sessionID)
	}
	if agentID != "vivi@aimebu" {
		t.Fatalf("agentID = %q, want vivi@aimebu", agentID)
	}
}

func TestAgentPiJSONEvents(t *testing.T) {
	output := []byte("not json\n{\"type\":\"session\",\"version\":3,\"id\":\"pi-session-123\",\"cwd\":\"/tmp\"}\n{\"type\":\"message_update\"}\n{\"type\":\"agent_end\",\"messages\":[]}\n")
	id, line := agentParsePiSessionID(output)
	if id != "pi-session-123" || line != 2 {
		t.Fatalf("got id=%q line=%d, want pi-session-123 line 2", id, line)
	}
	if !agentPiHasAgentEnd(output) {
		t.Fatal("expected agent_end detection")
	}

	id, line = agentParsePiSessionID([]byte("{\"type\":\"agent_end\"}\n"))
	if id != "" || line != -1 {
		t.Fatalf("got id=%q line=%d, want empty/-1", id, line)
	}
}

// TestAgentDebugLogsResumeFailureEvents is a logger-level fixture, not a loop
// integration test. It verifies that agentLogHarnessExit and
// agentLogRecoveryDecision produce the correct JSONL records. A regression
// where agentResumeLoop stops calling these functions would not be caught here;
// that would require a full harness mock exercising the loop's doneCh path.
func TestAgentDebugLogsResumeFailureEvents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", dir)
	t.Setenv("AIMEBU_AGENT_DEBUG", "1")

	debug := newAgentDebugLog("worker", "resumetag")
	if debug == nil || !debug.enabled {
		t.Fatal("expected debug logging to be enabled")
	}

	// Simulate the sequence agentResumeLoop emits when a harness exits and the
	// child output indicates the server became unreachable.
	agentLogHarnessExit(debug, nil, 250*time.Millisecond, []byte("connection refused to 127.0.0.1:9997"))
	agentLogRecoveryDecision(debug, agentRecoveryServerUnreachable, "child output reported server unreachable", 1, time.Second)

	if err := debug.close(); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "agents", "agent-logs", "worker.log")
	records := readAgentDebugRecords(t, logPath)
	events := debugEventNames(records)

	if !contains(eventsCSV(events), "harness_exit") {
		t.Fatalf("expected harness_exit in %v", events)
	}
	if !contains(eventsCSV(events), "recovery_decision") {
		t.Fatalf("expected recovery_decision in %v", events)
	}

	exit := firstDebugEvent(records, "harness_exit")
	if got := exit["exit_code"]; got != float64(0) {
		t.Fatalf("exit_code = %v, want 0", got)
	}
	if got, ok := exit["stderr_tail"].(string); !ok || !contains(got, "connection refused") {
		t.Fatalf("stderr_tail = %v, want string containing 'connection refused'", exit["stderr_tail"])
	}

	decision := firstDebugEvent(records, "recovery_decision")
	if got := decision["class"]; got != string(agentRecoveryServerUnreachable) {
		t.Fatalf("class = %v, want %v", got, agentRecoveryServerUnreachable)
	}
	if got := decision["retry_count"]; got != float64(1) {
		t.Fatalf("retry_count = %v, want 1", got)
	}
	if got := decision["backoff_ms"]; got != float64(time.Second.Milliseconds()) {
		t.Fatalf("backoff_ms = %v, want %v", got, time.Second.Milliseconds())
	}
}

func readAgentDebugRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var records []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("unmarshal %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func debugEventNames(records []map[string]any) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		if name, ok := record["event"].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

func assertEventSubsequence(t *testing.T, events []string, want []string) {
	t.Helper()
	pos := 0
	for _, event := range events {
		if pos < len(want) && event == want[pos] {
			pos++
		}
	}
	if pos != len(want) {
		t.Fatalf("event sequence %v does not contain subsequence %v", events, want)
	}
}

func firstDebugEvent(records []map[string]any, name string) map[string]any {
	for _, record := range records {
		if record["event"] == name {
			return record
		}
	}
	return nil
}

func eventsCSV(events []string) string { return strings.Join(events, ",") }
