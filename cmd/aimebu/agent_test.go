package main

import (
	"bufio"
	"encoding/json"
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
	// Pass an unreachable URL so it falls back to the compiled default template.
	prompt := agentBuildRecoveryPrompt("http://127.0.0.1:0", "codex", "", "worker", []string{"general", "dev"})
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

// TestAgentSpawnPrompt_TokenSubstitution verifies that agentApplyPromptTokens
// correctly substitutes all four tokens, leaves unknown tokens literal, and
// handles empty forceName/roomsSection without breaking output.
func TestAgentSpawnPrompt_TokenSubstitution(t *testing.T) {
	t.Run("all four tokens substituted", func(t *testing.T) {
		tmpl := `harness={{harness}} meta={{meta_json}} force={{force_name}} rooms={{rooms_section}}`
		got := agentApplyPromptTokens(tmpl, "claude-code", `{"k":"v"}`, "alice", "Join these rooms: dev.\n\n")
		want := `harness="claude-code" meta={"k":"v"} force="alice" rooms=Join these rooms: dev.` + "\n\n"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("unknown token left literal", func(t *testing.T) {
		tmpl := `before {{unknown_token}} after`
		got := agentApplyPromptTokens(tmpl, "codex", `{}`, "", "")
		if !contains(got, "{{unknown_token}}") {
			t.Fatalf("unknown token was removed from %q", got)
		}
	})

	t.Run("empty forceName and roomsSection produce valid output", func(t *testing.T) {
		tmpl := agentBootstrapTemplate
		got := agentApplyPromptTokens(tmpl, "codex", `{"protocol":"agent"}`, "", "")
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
	sessionID, agentID, err := agentBootstrapSession("codex", []string{harnessPath}, "keep listening", env, server.URL, spawnTag, "", make(chan os.Signal, 1), debug)
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
