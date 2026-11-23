package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestDockerLauncherReceivesInlineMCPAndResume(t *testing.T) {
	root := t.TempDir()
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	path := filepath.Join(root, "claude-docker")
	argsPath, envPath := filepath.Join(root, "args"), filepath.Join(root, "env")
	script := "#!/bin/sh\nprintf '%s\\n' \"$AIMEBU_AGENT_KEY\" \"$HARNESS_DOCKER_FORWARD_ENV\" > " + envPath + "\nprintf '%s\\n' \"$CLAUDE_LAUNCH_FLAGS\" \"$@\" > " + argsPath + "\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"session-1\"}' '{\"type\":\"result\",\"result\":\"{\\\"messages\\\":[]}\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "docker"}
	spec := adapter.TurnSpec{TurnID: "docker", Agent: adapter.AgentSpec{ID: "agt_test", Tool: "claude", Variant: "docker", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeNormal, AgentTools: config, NativeSessionStarted: true, NativeSessionHandle: "session-1", NotesJSON: json.RawMessage(`[]`)}
	turn, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	if exit, err := turn.Wait(); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--resume\nsession-1", "--mcp-config\n", "host.docker.internal:9897/api/agent/mcp", "Bearer ${AIMEBU_AGENT_KEY}"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %q in %q", want, args)
		}
	}
	env, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "aak_secret\nAIMEBU_AGENT_KEY,CLAUDE_CODE_DISABLE_BACKGROUND_TASKS\n" {
		t.Fatalf("the key must reach the harness through the forwarded variable, got %q", env)
	}
	if strings.Contains(string(args), "aak_secret") {
		t.Fatalf("the agent key is in the argument list, which ps shows to every local user: %q", args)
	}
}

// A normal turn leaves the person's own MCP servers and connectors available (no --strict-mcp-config), whatever the variant; only a diagnostic
// turn, which must touch nothing, is strict. The aimebu entry is passed with --mcp-config either way.
func TestOnlyADiagnosticTurnIsStrictAboutMCP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		variant string
		mode    adapter.Mode
		strict  bool
	}{
		{"native normal", "native", adapter.ModeNormal, false},
		{"docker normal", "docker", adapter.ModeNormal, false},
		{"native diagnostic", "native", adapter.ModeDiagnostic, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
			path := filepath.Join(root, "claude")
			argsPath := filepath.Join(root, "args")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"session-1\"}' '{\"type\":\"result\",\"result\":\"{\\\"messages\\\":[]}\"}'\n"
			if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			a := &Adapter{Command: path, Variant: tc.variant, AllowNative: true}
			spec := adapter.TurnSpec{TurnID: "strict", Agent: adapter.AgentSpec{ID: "agt_test", Tool: "claude", Variant: tc.variant, Model: "haiku"}, CheckoutPath: root, Mode: tc.mode, AgentTools: config, NativeSessionHandle: "session-1", NotesJSON: json.RawMessage(`[]`)}
			turn, err := a.Start(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			for range turn.Events() {
			}
			if _, err := turn.Wait(); err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(args), "--strict-mcp-config\n"); got != tc.strict {
				t.Fatalf("strict MCP = %v, want %v:\n%s", got, tc.strict, args)
			}
			if tc.mode == adapter.ModeNormal && !strings.Contains(string(args), "--mcp-config\n") {
				t.Fatalf("a normal turn must pass the aimebu entry with --mcp-config:\n%s", args)
			}
		})
	}
}

func TestClaudeSchemaDefinesStructuredInboxQuestion(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Items struct {
				Properties map[string]struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(responseSchema, &schema); err != nil {
		t.Fatal(err)
	}
	ask := schema.Properties["messages"].Items.Properties["ask"]
	for _, field := range []string{"kind", "text", "options", "free_text"} {
		if _, ok := ask.Properties[field]; !ok {
			t.Fatalf("ask schema lacks %s", field)
		}
	}
	if len(ask.Required) != 4 || !strings.Contains(systemPrompt, "A room returned by create_room") || !strings.Contains(systemPrompt, `"ask":{"kind":`) {
		t.Fatalf("lead question contract missing: required=%v prompt=%q", ask.Required, systemPrompt)
	}
	for _, want := range []string{`"reply_to":null,"attachments":null`, `"handoff":false,"visual_plan":null,"appendix_pages":null`, `"needs_attention":false,"proposed_answers":null,"open_questions":null`, "a live @slug or @agent_id mention", "after four automatic replies with no human message between them, replies stop waking anyone", "Do not send acknowledgement-only", "publish the home-room summary"} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("missing prompt guidance %q", want)
		}
	}
}

func TestResponseSchemaUsesStrictObjectRules(t *testing.T) {
	adaptertest.AssertStrictOutputSchema(t, responseSchema)
}

func TestFailedLauncherCapturesRedactedStderr(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho 'Bearer aak_secret: checkout is not mounted' >&2\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, AllowNative: true}
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "failed-stderr", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeCorrection})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	exit, err := turn.Wait()
	if err != nil || exit.ExitCode != 17 || exit.TerminalEvent || !strings.Contains(exit.StderrTail, "checkout is not mounted") || strings.Contains(exit.StderrTail, "aak_secret") {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
}

func TestFailedClaudeResultKeepsRedactedLimitEvidence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	program := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"result\":\"limit reached; resets at 2026-10-15T12:00:00Z aak_secret\"}'\nexit 1\n"
	if err := testenv.WriteExecutable(path, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{
		TurnID: "limit-result", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeCorrection,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	exit, err := turn.Wait()
	if err != nil || exit.TerminalEvent || !strings.Contains(exit.ErrorEvent, "limit reached") || !strings.Contains(exit.ErrorEvent, "resets at") || strings.Contains(exit.ErrorEvent, "aak_secret") {
		t.Fatalf("exit=%+v error=%v", exit, err)
	}
}

func TestConformance(t *testing.T) {
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	path := filepath.Join(t.TempDir(), "claude")
	marker := filepath.Join(t.TempDir(), "effect")
	checkout := t.TempDir()
	toolsConfig := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	script := `#!/bin/sh
input=$(cat)
case "$input" in
  *turn-events*)
    printf '%s\n' '{"type":"assistant"}' '{"type":"result","result":"{\"messages\":[]}","usage":{"input_tokens":11,"output_tokens":7},"modelUsage":{"x":{"contextWindow":200,"costUSD":0.1}}}' '{"type":"system","subtype":"compact_boundary","compact_metadata":{"pre_tokens":100,"post_tokens":20}}' ;;
  *turn-no_terminal*) printf '%s\n' '{"type":"assistant"}' ;;
  *turn-cancel*) exec sleep 30 ;;
  *turn-crash_after_effect*) : > "EFFECT_MARKER"; exit 17 ;;
esac
`
	script = strings.ReplaceAll(script, "EFFECT_MARKER", marker)
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	adaptertest.Run(t, func(t *testing.T, scenario adaptertest.Scenario) (adapter.Adapter, adapter.TurnSpec, <-chan struct{}) {
		effected := make(chan struct{})
		if scenario == adaptertest.ScenarioCrashAfterEffect {
			go func() {
				for {
					if _, err := os.Stat(marker); err == nil {
						close(effected)
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
		} else {
			close(effected)
		}
		return &Adapter{Command: path, AllowNative: true}, adapter.TurnSpec{TurnID: "turn-" + string(scenario), Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: checkout, AgentTools: toolsConfig, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}, effected
	}, adaptertest.ChildProcess())
}

func TestResultUsageIncludesCacheTokens(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"assistant","message":{"usage":{"input_tokens":11,"cache_read_input_tokens":10,"cache_creation_input_tokens":4}}}' '{"type":"assistant","parent_tool_use_id":"toolu_child","message":{"usage":{"input_tokens":90}}}' '{"type":"result","result":"{\"messages\":[]}","usage":{"input_tokens":111,"output_tokens":7,"cache_read_input_tokens":110,"cache_creation_input_tokens":40},"modelUsage":{"x":{"contextWindow":200,"costUSD":0.1}}}'
`
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := &adapter.AgentToolsAccess{AgentID: "agt_usage", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "usage", Agent: adapter.AgentSpec{ID: "agt_usage", Model: "haiku"}, CheckoutPath: root, AgentTools: config, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	var usage *adapter.Usage
	for event := range turn.Events() {
		if event.Kind == adapter.EventUsage {
			usage = event.Usage
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.ContextUsed == nil || *usage.ContextUsed != 25 || usage.ContextWindow == nil || *usage.ContextWindow != 200 || usage.CostEstimate == nil || *usage.CostEstimate != 0.1 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestResumeAndRawCandidate(t *testing.T) {
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	path := filepath.Join(t.TempDir(), "claude")
	var argsPath = filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > " + argsPath + "\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"{\\\"messages\\\":[]}\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	implementation := &Adapter{Command: path, AllowNative: true}
	toolsConfig := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	// The session's transcript is in the config dir the launch uses, so it is resumed.
	transcript := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "p", "session-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn, err := implementation.Start(context.Background(), adapter.TurnSpec{TurnID: "resume", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: t.TempDir(), AgentTools: toolsConfig, NativeSessionHandle: "session-1", NativeSessionStarted: true, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	var answer string
	for event := range turn.Events() {
		if event.Kind == adapter.EventFinalAnswer {
			answer = string(event.Answer)
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if answer != `{"messages":[]}` {
		t.Fatalf("candidate %q", answer)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) == "" || !contains(string(args), "--resume session-1") || !contains(string(args), "--output-format stream-json") {
		t.Fatalf("args %q", args)
	}
}

func TestNewSessionReportsServerIssuedHandle(t *testing.T) {
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	argsPath := filepath.Join(root, "args")
	path := filepath.Join(root, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"4c014434-9712-45a7-9d2e-a7866ef8f8dc\"}' '{\"type\":\"result\",\"result\":\"{\\\"messages\\\":[]}\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "new", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, AgentTools: config, NativeSessionHandle: "4c014434-9712-45a7-9d2e-a7866ef8f8dc", Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{"role_body":"Reply concisely."}`)})
	if err != nil {
		t.Fatal(err)
	}
	var handle string
	for event := range turn.Events() {
		if event.Kind == adapter.EventNativeSessionStarted {
			handle = event.NativeSessionHandle
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if handle != "4c014434-9712-45a7-9d2e-a7866ef8f8dc" {
		t.Fatalf("handle %q", handle)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil || !contains(string(args), "--session-id\n4c014434-9712-45a7-9d2e-a7866ef8f8dc") || !contains(string(args), "Reply concisely.") {
		t.Fatalf("args %q err %v", args, err)
	}
}

// A forked turn resumes the original with --fork-session; Claude saves the copy under a new id, which the turn reports once and which is
// what the later turns resume. The original's id (a stray line carrying it) is never reported as the copy.
func TestForkedTurnReportsTheCopyOfTheSession(t *testing.T) {
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	argsPath := filepath.Join(root, "args")
	path := filepath.Join(root, "claude")
	const original, copyID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"" + original + "\"}' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"" + copyID + "\"}' '{\"type\":\"result\",\"session_id\":\"" + copyID + "\",\"result\":\"{\\\"messages\\\":[]}\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	spec := adapter.TurnSpec{TurnID: "fork", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, AgentTools: config,
		NativeSessionHandle: original, NativeSessionStarted: true, ForkSession: true, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var handles []string
	for event := range turn.Events() {
		if event.Kind == adapter.EventNativeSessionStarted {
			handles = append(handles, event.NativeSessionHandle)
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || handles[0] != copyID {
		t.Fatalf("reported sessions %v", handles)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil || !contains(string(args), "--resume\n"+original+"\n--fork-session") {
		t.Fatalf("args %q err %v", args, err)
	}
	// Only a first turn forks; a plain resume never reports a session and never passes the flag.
	spec.ForkSession, spec.NativeSessionHandle, spec.TurnID = false, copyID, "after"
	turn, err = (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for event := range turn.Events() {
		if event.Kind == adapter.EventNativeSessionStarted {
			t.Fatal("an ordinary resumed turn reported a session")
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if args, err := os.ReadFile(argsPath); err != nil || contains(string(args), "--fork-session") {
		t.Fatalf("args %q err %v", args, err)
	}
	// A fork without a session to fork is a mistake of the caller.
	spec.ForkSession, spec.NativeSessionStarted = true, false
	if _, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), spec); err == nil {
		t.Fatal("a fork of no session was started")
	}
}

func TestFailedFirstSessionDoesNotReportStarted(t *testing.T) {
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "failed", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, AgentTools: config, NativeSessionHandle: "4c014434-9712-45a7-9d2e-a7866ef8f8dc", Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	for event := range turn.Events() {
		if event.Kind == adapter.EventNativeSessionStarted {
			t.Fatal("failed process reported a native session")
		}
	}
	exit, err := turn.Wait()
	if err != nil || exit.ExitCode != 17 {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
}

func contains(s, part string) bool {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func TestLiveCanary(t *testing.T) {
	docker := os.Getenv("AIMEBU_LIVE_CC_DOCKER") == "1"
	if !docker && os.Getenv("AIMEBU_LIVE_CC") != "1" {
		t.Skip("set AIMEBU_LIVE_CC=1 or AIMEBU_LIVE_CC_DOCKER=1 to run Claude Code canary")
	}
	root := t.TempDir()
	if docker {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Join(cwd, "..", "..", "..", "tmp")
		if err := os.MkdirAll(base, 0700); err != nil {
			t.Fatal(err)
		}
		root, err = os.MkdirTemp(base, "claude-docker-canary-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	config := &adapter.AgentToolsAccess{AgentID: "agt_live", ServerURL: "http://127.0.0.1:9897", Key: "aak_canary", Command: "/usr/bin/aimebu"}
	implementation := New()
	var stderr bytes.Buffer
	implementation.stderrCapture = &stderr
	variant := "native"
	if docker {
		implementation = NewDocker()
		implementation.stderrCapture = &stderr
		variant = "docker"
	} else {
		t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
		implementation.AllowNative = true
	}
	implementation.MaxBudgetUSD = "0.25"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const nonce = "aimebu-claude-two-turn-nonce"
	run := func(id, handle string, started bool, body string) (string, string) {
		notes, _ := json.Marshal([]map[string]string{{"body": body}})
		turn, err := implementation.Start(ctx, adapter.TurnSpec{TurnID: id, Agent: adapter.AgentSpec{ID: "agt_live", Variant: variant, Model: "haiku"}, CheckoutPath: root, AgentTools: config, NativeSessionHandle: handle, NativeSessionStarted: started, Mode: adapter.ModeNormal, NotesJSON: notes, ContextJSON: json.RawMessage(`{"role_body":"Follow the user note exactly."}`)})
		if err != nil {
			t.Fatal(err)
		}
		var answer, session string
		for event := range turn.Events() {
			if event.Kind == adapter.EventFinalAnswer {
				answer = string(event.Answer)
			}
			if event.Kind == adapter.EventNativeSessionStarted {
				session = event.NativeSessionHandle
			}
		}
		exit, err := turn.Wait()
		if err != nil || !exit.TerminalEvent {
			t.Fatalf("exit=%+v err=%v stderr=%q", exit, err, stderr.String())
		}
		return answer, session
	}
	var sessionBytes [16]byte
	if _, err := rand.Read(sessionBytes[:]); err != nil {
		t.Fatal(err)
	}
	initialSession := hex.EncodeToString(sessionBytes[0:4]) + "-" + hex.EncodeToString(sessionBytes[4:6]) + "-" + hex.EncodeToString(sessionBytes[6:8]) + "-" + hex.EncodeToString(sessionBytes[8:10]) + "-" + hex.EncodeToString(sessionBytes[10:])
	_, session := run("live-canary-first", initialSession, false, "Remember this nonce for the next turn: "+nonce+". Return a valid empty messages response.")
	if session == "" {
		t.Fatal("first turn did not establish a native session")
	}
	answer, _ := run("live-canary-second", session, true, "What nonce did I ask you to remember? Return it in a valid message response.")
	if !contains(answer, nonce) {
		t.Fatalf("resume answer did not recall nonce: %s", answer)
	}
}

func TestToolUseAndResultMarkToolEvents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"x"},{"type":"tool_use","id":"toolu_a"},{"type":"tool_use","id":"toolu_b"}]}}' '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_a"}]}}' '{"type":"user","message":{"content":"plain text"}}' '{"type":"result","result":"{\"messages\":[]}"}'
`
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := &adapter.AgentToolsAccess{AgentID: "agt_tools", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "tools", Agent: adapter.AgentSpec{ID: "agt_tools", Model: "haiku"}, CheckoutPath: root, AgentTools: config, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	var got [][]adapter.ToolEvent
	for event := range turn.Events() {
		if event.Kind == adapter.EventActivity {
			got = append(got, event.Tools)
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if lo, ok := turn.(interface{ LastOutput() time.Time }); !ok || time.Since(lo.LastOutput()) > time.Minute {
		t.Fatalf("turn must report raw output liveness, got %v", turn)
	}
	want := [][]adapter.ToolEvent{{{ID: "toolu_a"}, {ID: "toolu_b"}}, {{ID: "toolu_a", Done: true}}, nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool events = %+v, want %+v", got, want)
	}
}

func TestOversizedStdoutLineKillsProcessGroupBeforeWait(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	// One stdout line beyond the 8 MiB scanner limit, then the CLI stays
	// alive holding stdout and stderr open, as a wedged harness would.
	script := "#!/bin/sh\ncat >/dev/null\nhead -c 9437184 /dev/zero | tr '\\0' a\necho\nsleep 30\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, AllowNative: true}
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "oversized", Agent: adapter.AgentSpec{ID: "agt_test", Tool: "claude", Model: "haiku"},
		CheckoutPath: root, Mode: adapter.ModeCorrection, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = turn.Cancel() })
	done := make(chan struct{})
	var exit adapter.ExitInfo
	var waitErr error
	go func() {
		for range turn.Events() {
		}
		exit, waitErr = turn.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("turn hung after the stdout scanner failed")
	}
	if waitErr == nil || exit.TerminalEvent || exit.ExitCode == 0 {
		t.Fatalf("exit=%+v err=%v", exit, waitErr)
	}
}

// Claude keys its sessions by the folder it is started in, as spelled. A turn with a LaunchPath starts it there (Go sets PWD to Dir), so a
// session saved under a link to the folder is found; without one it starts in the canonical checkout.
func TestLaunchPathIsTheFolderTheHarnessStartsIn(t *testing.T) {
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	pwdPath := filepath.Join(root, "pwd")
	path := filepath.Join(root, "claude")
	const session = "11111111-1111-4111-8111-111111111111"
	script := "#!/bin/sh\nprintf '%s' \"$PWD\" > " + pwdPath + "\nprintf '%s\\n' '{\"type\":\"result\",\"session_id\":\"" + session + "\",\"result\":\"{\\\"messages\\\":[]}\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	run := func(launch string) string {
		t.Helper()
		spec := adapter.TurnSpec{TurnID: "launch", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: real, LaunchPath: launch, AgentTools: config,
			NativeSessionHandle: session, NativeSessionStarted: true, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}
		turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		for range turn.Events() {
		}
		if _, err := turn.Wait(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(pwdPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(got)
	}
	if got := run(""); got != real {
		t.Fatalf("without a launch path the harness started in %q, want the checkout %q", got, real)
	}
	if got := run(link); got != link {
		t.Fatalf("with a launch path the harness started in %q, want %q", got, link)
	}
}

// An explicit error result of a turn that ran no tool and produced no output is a definite failure with the harness's own sentence; one that
// came after tool use or output is not: effects may have happened, so the outcome stays uncertain.
func TestClaudeErrorResultIsADefiniteFailureOnlyWhenTheTurnDidNothing(t *testing.T) {
	const sentence = "Failed to authenticate: OAuth session expired · Please run /login aak_secret"
	line := func(extra string) string {
		return `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":"` + sentence + `","usage":{"input_tokens":3,"output_tokens":` + extra + `}}`
	}
	tool := `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"toolu_1","name":"Bash"}]}}`
	for name, c := range map[string]struct {
		events   []string
		definite bool
	}{
		"nothing happened":    {[]string{line("0")}, true},
		"after a tool ran":    {[]string{tool, line("0")}, false},
		"after output":        {[]string{line("17")}, false},
		"no sentence at all":  {[]string{`{"type":"result","is_error":true}`}, false},
		"an ordinary success": {[]string{`{"type":"result","is_error":false,"result":"{\"messages\":[]}"}`}, false},
	} {
		root := t.TempDir()
		path := filepath.Join(root, "claude")
		program := "#!/bin/sh\n"
		for _, event := range c.events {
			program += "printf '%s\\n' '" + event + "'\n"
		}
		program += "exit 1\n"
		if err := testenv.WriteExecutable(path, []byte(program), 0700); err != nil {
			t.Fatal(err)
		}
		turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{
			TurnID: "definite", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeCorrection,
		})
		if err != nil {
			t.Fatal(err)
		}
		for range turn.Events() {
		}
		exit, err := turn.Wait()
		if err != nil || exit.DefiniteFailure != c.definite {
			t.Errorf("%s: definite=%t, want %t (exit=%+v err=%v)", name, exit.DefiniteFailure, c.definite, exit, err)
		}
		if strings.Contains(exit.ErrorSummary, "aak_secret") || strings.Contains(exit.ErrorEvent, "aak_secret") {
			t.Errorf("%s: a secret survived: %+v", name, exit)
		}
		if c.definite && exit.ErrorSummary != "Failed to authenticate: OAuth session expired · Please run /login [REDACTED]" && !strings.HasPrefix(exit.ErrorSummary, "Failed to authenticate: OAuth session expired") {
			t.Errorf("%s: summary %q", name, exit.ErrorSummary)
		}
		if name == "an ordinary success" && (exit.ErrorSummary != "" || exit.ErrorEvent != "") {
			t.Errorf("%s: evidence of an error that was none: %+v", name, exit)
		}
	}
}

// A native turn hands the bridge's entry inline, with the key as a reference Claude expands from its own environment: the key is in the harness's
// environment, in no argument and in no file under the checkout.
func TestNativeTurnKeyIsInTheEnvironmentOnly(t *testing.T) {
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	root := t.TempDir()
	captures := t.TempDir() // outside the tree that is searched for the key
	path := filepath.Join(root, "claude")
	argsPath, envPath := filepath.Join(captures, "args"), filepath.Join(captures, "env")
	script := "#!/bin/sh\nprintf '%s' \"$AIMEBU_AGENT_KEY\" > " + envPath + "\nprintf '%s\\n' \"$@\" > " + argsPath + "\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"session-1\"}' '{\"type\":\"result\",\"result\":\"{\\\"messages\\\":[]}\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	const key = "aak_native_claude_key"
	access := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: key, Command: "/usr/bin/aimebu", Env: map[string]string{"AIMEBU_SENTRY_DSN": "off"}}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "native", Agent: adapter.AgentSpec{ID: "agt_test", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeNormal, AgentTools: access, NativeSessionHandle: "4c014434-9712-45a7-9d2e-a7866ef8f8dc", NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	if exit, err := turn.Wait(); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	args, _ := os.ReadFile(argsPath)
	if strings.Contains(string(args), key) {
		t.Fatalf("the key is in the argument list: %q", args)
	}
	if env, _ := os.ReadFile(envPath); string(env) != key {
		t.Fatalf("the key must reach Claude in its environment, got %q", env)
	}
	lines := strings.Split(string(args), "\n")
	var inline string
	for i, line := range lines {
		if line == "--mcp-config" && i+1 < len(lines) {
			inline = lines[i+1]
		}
	}
	var config struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(inline), &config); err != nil {
		t.Fatalf("the MCP entry is not inline JSON: %q: %v", inline, err)
	}
	server := config.Servers[toolboxServer]
	if server.Command != "/usr/bin/aimebu" || len(server.Args) != 1 || server.Args[0] != "agent-tools" || server.Env["AIMEBU_AGENT_KEY"] != "${AIMEBU_AGENT_KEY}" || server.Env["AIMEBU_AGENT_ID"] != "agt_test" || server.Env["AIMEBU_URL"] != "http://127.0.0.1:9897" || server.Env["AIMEBU_SENTRY_DSN"] != "off" {
		t.Fatalf("bridge entry: %+v", server)
	}
	if err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if data, _ := os.ReadFile(p); strings.Contains(string(data), key) {
			t.Errorf("%s holds the key", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
