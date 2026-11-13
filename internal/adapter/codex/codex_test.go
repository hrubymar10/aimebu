package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

func TestTurnPromptSeparatesInboxAskFromAttention(t *testing.T) {
	prompt, err := turnInput(adapter.TurnSpec{TurnID: "prompt", Agent: adapter.AgentSpec{ID: "agt_prompt"}, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reply_to":null,"attachments":null`, `"handoff":false,"visual_plan":null,"appendix_pages":null`, `"needs_attention":false,"proposed_answers":null,"open_questions":null`, "a live @slug or @agent_id mention", "after four automatic replies with no human message between them, replies stop waking anyone", "Do not send acknowledgement-only", "publish the home-room summary"} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("missing prompt guidance %q", want)
		}
	}
}

func TestDockerLauncherReceivesInlineMCPAndMirroredSchema(t *testing.T) {
	root := t.TempDir()
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	path := filepath.Join(root, "codex-docker")
	argsPath, envPath := filepath.Join(root, "args"), filepath.Join(root, "env")
	script := "#!/bin/sh\nprintf '%s\\n' \"$AIMEBU2_AGENT_KEY\" \"$HARNESS_DOCKER_FORWARD_ENV\" > " + envPath + "\nprintf '%s\\n' \"$CODEX_LAUNCH_FLAGS\" \"$@\" > " + argsPath + "\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"messages\\\":[]}\"}}' '{\"type\":\"turn.completed\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "docker"}
	spec := adapter.TurnSpec{TurnID: "docker", Agent: adapter.AgentSpec{ID: "agt_test", Tool: "codex", Variant: "docker", Model: "gpt-6-sol"}, CheckoutPath: root, VisibleCheckoutPath: "/workspace/project", Mode: adapter.ModeNormal, AgentTools: config, NotesJSON: json.RawMessage(`[]`)}
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
	for _, want := range []string{"mcp_servers.aimebu_docker=", "host.docker.internal:9897/api/agent/mcp", `bearer_token_env_var="AIMEBU2_AGENT_KEY"`, "-C\n/workspace/project\n", "exec\n--ignore-user-config\n", "--output-schema\n/workspace/project/.aimebu/codex-schemas/"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %q in %q", want, args)
		}
	}
	env, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "aak_secret\nAIMEBU2_AGENT_KEY\n" {
		t.Fatalf("the key must reach the harness through the forwarded variable, got %q", env)
	}
	if strings.Contains(string(args), "aak_secret") {
		t.Fatalf("the agent key is in the argument list, which ps shows to every local user: %q", args)
	}
}

func TestEnsureSchemaRestoresDeletedCheckoutFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".aimebu", "codex-schemas", "agent", "schema.json")
	if err := ensureSchema(path, responseSchema); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := ensureSchema(path, diagnosticSchema); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(diagnosticSchema) {
		t.Fatalf("restored schema = %q, %v", data, err)
	}
}

func TestOutputSchemasUseStrictObjectRules(t *testing.T) {
	for name, schema := range map[string][]byte{"normal": responseSchema, "diagnostic": diagnosticSchema} {
		t.Run(name, func(t *testing.T) { adaptertest.AssertStrictOutputSchema(t, schema) })
	}
}

func TestFailedLauncherCapturesRedactedStderr(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "codex")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho 'key=aak_secret: checkout is not mounted' >&2\nexit 19\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.Command, a.AllowNative, a.AuthPath = path, true, ""
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "failed-stderr", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: adapter.ModeCorrection})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	exit, err := turn.Wait()
	if err != nil || exit.ExitCode != 19 || exit.TerminalEvent || !strings.Contains(exit.StderrTail, "checkout is not mounted") || strings.Contains(exit.StderrTail, "aak_secret") {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
}

func TestLastCodexErrorEventAppearsInExitEvidence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "codex")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"error","message":"first error"}' '{"type":"turn.failed","error":{"message":"Bearer aak_secret: MCP OAuth invalid_grant"}}'
exit 1
`
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.Command, a.AllowNative, a.AuthPath = path, true, ""
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "failed-event", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: adapter.ModeCorrection})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	exit, err := turn.Wait()
	if err != nil || exit.ExitCode != 1 || !strings.Contains(exit.ErrorEvent, "invalid_grant") || !strings.Contains(exit.ErrorEvent, "turn.failed") || strings.Contains(exit.ErrorEvent, "first error") || strings.Contains(exit.ErrorEvent, "aak_secret") {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
}

func TestConformance(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	adaptertest.Run(t, func(t *testing.T, scenario adaptertest.Scenario) (adapter.Adapter, adapter.TurnSpec, <-chan struct{}) {
		root := t.TempDir()
		marker := filepath.Join(root, "effect")
		script := filepath.Join(root, "codex")
		body := `#!/bin/sh
input=$(cat)
case "$input" in
  *turn-events*)
    printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}' '{"type":"turn.started"}' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"messages\":[]}"}}' '{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":7}}' ;;
  *turn-no_terminal*)
    printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}' '{"type":"turn.started"}' ;;
  *turn-cancel*) exec sleep 30 ;;
  *turn-crash_after_effect*) : > "` + marker + `"; exit 17 ;;
esac
`
		if err := testenv.WriteExecutable(script, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
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
		implementation := New(filepath.Join(root, "runtime"))
		implementation.Command = script
		implementation.AuthPath = ""
		spec := adapter.TurnSpec{TurnID: "turn-" + string(scenario), Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-luna"}, Mode: adapter.ModeNormal, CheckoutPath: root, NotesJSON: json.RawMessage(`[]`)}
		return implementation, spec, effected
	}, adaptertest.ChildProcess())
}

func TestResumeRawCandidateAndPrivateConfig(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	root := t.TempDir()
	a := New(filepath.Join(root, "runtime"))
	a.AuthPath = filepath.Join(root, "auth.json")
	if err := os.WriteFile(a.AuthPath, []byte(`{"auth_mode":"chatgpt"}`), 0600); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "args")
	script := filepath.Join(root, "codex")
	body := "#!/bin/sh\nprintf '%s' \"$*\" > " + capture + "\nprintf '%s' \"$AIMEBU2_AGENT_KEY\" > " + capture + ".key\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}' '{\"type\":\"turn.started\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"messages\\\":[]}" + "\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}'\n"
	if err := testenv.WriteExecutable(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	a.Command = script
	toolConfig := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu", Env: map[string]string{"AIMEBU2_INSECURE_SKIP_VERIFY": "1"}}
	spec := adapter.TurnSpec{TurnID: "resume", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-luna"}, Mode: adapter.ModeNormal, CheckoutPath: root, NativeSessionStarted: true, NativeSessionHandle: "thread-1", AgentTools: toolConfig, NotesJSON: json.RawMessage(`[]`)}
	turn, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var candidate string
	for event := range turn.Events() {
		if event.Kind == adapter.EventFinalAnswer {
			candidate = string(event.Answer)
		}
	}
	exit, err := turn.Wait()
	if err != nil || !exit.TerminalEvent || candidate != `{"messages":[]}` {
		t.Fatalf("exit=%+v err=%v candidate=%q", exit, err, candidate)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "exec resume --json -m gpt-6-luna") || !strings.Contains(string(args), "thread-1 -") || strings.Contains(string(args), "aak_secret") {
		t.Fatalf("bad args %q", args)
	}
	runtimeDir := filepath.Dir(findConfig(t, a.RuntimeRoot))
	stored, err := os.ReadFile(filepath.Join(runtimeDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "aak_secret") || !strings.Contains(string(stored), `env_vars = ["AIMEBU2_AGENT_KEY"]`) {
		t.Fatalf("the private MCP config must name the key variable and never hold the key:\n%s", stored)
	}
	if key, err := os.ReadFile(capture + ".key"); err != nil || string(key) != "aak_secret" {
		t.Fatalf("the key reaches Codex only in its environment: %q err=%v", key, err)
	}
	if !strings.Contains(string(stored), "AIMEBU2_INSECURE_SKIP_VERIFY = \"1\"") {
		t.Fatal("private MCP config did not propagate TLS verification opt-in")
	}
	info, err := os.Stat(filepath.Join(runtimeDir, "config.toml"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config mode=%v err=%v", info.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(runtimeDir, "auth.json")); err != nil || target != a.AuthPath {
		t.Fatalf("auth link=%q err=%v", target, err)
	}
	// A diagnostic rewrites the private config without any MCP server.
	spec.Mode = adapter.ModeDiagnostic
	spec.AgentTools = nil
	if _, err := a.prepareRuntime(spec, userMCP{}); err != nil {
		t.Fatal(err)
	}
	stored, err = os.ReadFile(filepath.Join(runtimeDir, "config.toml"))
	if err != nil || strings.Contains(string(stored), "mcp_servers") {
		t.Fatalf("diagnostic MCP config=%q err=%v", stored, err)
	}
}

func TestRejectsChangingThreadID(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	root := t.TempDir()
	script := filepath.Join(root, "codex")
	body := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"first"}' '{"type":"thread.started","thread_id":"second"}' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"messages\":[]}"}}' '{"type":"turn.completed"}'
`
	if err := testenv.WriteExecutable(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.AuthPath = ""
	a.Command = script
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "changed-thread", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-luna"},
		Mode: adapter.ModeNormal, CheckoutPath: root, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	exit, err := turn.Wait()
	if err == nil || exit.TerminalEvent {
		t.Fatalf("changing thread ID accepted: exit=%+v err=%v", exit, err)
	}
}

func findConfig(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("runtime entries=%v err=%v", entries, err)
	}
	return filepath.Join(root, entries[0].Name(), "config.toml")
}

func TestChildEnvExcludesBusSecrets(t *testing.T) {
	t.Setenv("AIMEBU2_AGENT_KEY", "secret")
	t.Setenv("OPENAI_API_KEY", "test-key")
	items := childEnv("/tmp/private-codex")
	joined := strings.Join(items, "\n")
	if strings.Contains(joined, "AIMEBU2_") || !strings.Contains(joined, "CODEX_HOME=/tmp/private-codex") || !strings.Contains(joined, "OPENAI_API_KEY=test-key") {
		t.Fatalf("child env selection=%q", joined)
	}
}

func TestCatalogDetectsCLIVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\nprintf 'codex-cli 0.162.1\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(t.TempDir())
	a.Command = path
	items := a.Catalog()
	if len(items) != 1 || items[0].Tool != "codex" || items[0].Version != "0.162.1" || items[0].Environment.Variant != "native" || items[0].Environment.OS != runtime.GOOS {
		t.Fatalf("catalog=%+v", items)
	}
	if a.Capabilities(items[0].Version, items[0].Environment).ReadonlyVerified {
		t.Fatal("read-only capability claimed without host verification")
	}
	if got := a.Capabilities("0.163.0", items[0].Environment); got != (adapter.Capabilities{}) {
		t.Fatalf("unverified CLI version gained capabilities: %+v", got)
	}
}

func TestLiveCanary(t *testing.T) {
	docker := os.Getenv("AIMEBU2_LIVE_CODEX_DOCKER") == "1"
	if !docker && os.Getenv("AIMEBU2_LIVE_CODEX") != "1" {
		t.Skip("set AIMEBU2_LIVE_CODEX=1 or AIMEBU2_LIVE_CODEX_DOCKER=1 for the two-turn Codex canary")
	}
	checkout := t.TempDir()
	if docker {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Join(cwd, "..", "..", "..", "tmp")
		if err := os.MkdirAll(base, 0700); err != nil {
			t.Fatal(err)
		}
		checkout, err = os.MkdirTemp(base, "codex-docker-canary-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(checkout) })
	}
	if output, err := exec.Command("git", "-C", checkout, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	a := New(filepath.Join(t.TempDir(), "runtime"))
	variant := "native"
	var config *adapter.AgentToolsAccess
	if docker {
		a = NewDocker()
		variant = "docker"
		config = &adapter.AgentToolsAccess{AgentID: "agt_live_codex", ServerURL: "http://127.0.0.1:9897", Key: "aak_canary", Command: "/usr/bin/aimebu"}
	} else {
		t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
		a.AllowNative = true
	}
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	a.stderrCapture = &stderr
	a.stdoutCapture = &stdout
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(random[:])
	first := adapter.TurnSpec{TurnID: "live-codex-first", Agent: adapter.AgentSpec{ID: "agt_live_codex", Variant: variant, Model: "gpt-6-luna"}, Mode: adapter.ModeNormal, CheckoutPath: checkout, AgentTools: config,
		NotesJSON: json.RawMessage(`[{"room_id":"canary","body":"Remember nonce ` + nonce + `. Reply with a JSON message to room canary saying remembered."}]`), ContextJSON: json.RawMessage(`{"room":"canary"}`)}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	turn, err := a.Start(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	var handle string
	var firstAnswer []byte
	for event := range turn.Events() {
		if event.Kind == adapter.EventNativeSessionStarted {
			handle = event.NativeSessionHandle
		}
		if event.Kind == adapter.EventFinalAnswer {
			firstAnswer = append([]byte(nil), event.Answer...)
		}
	}
	exit, err := turn.Wait()
	if err != nil || !exit.TerminalEvent || handle == "" || !json.Valid(firstAnswer) {
		t.Fatalf("first turn exit=%+v err=%v handle=%q answer=%q stderr=%q stdout=%q", exit, err, handle, firstAnswer, stderr.String(), stdout.String())
	}
	second := first
	second.TurnID = "live-codex-second"
	second.NativeSessionStarted = true
	second.NativeSessionHandle = handle
	second.NotesJSON = json.RawMessage(`[{"room_id":"canary","body":"What nonce did I ask you to remember? Reply with the exact nonce in a JSON message to room canary."}]`)
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer secondCancel()
	turn, err = a.Start(secondCtx, second)
	if err != nil {
		t.Fatal(err)
	}
	var answer []byte
	for event := range turn.Events() {
		if event.Kind == adapter.EventFinalAnswer {
			answer = append([]byte(nil), event.Answer...)
		}
	}
	exit, err = turn.Wait()
	if err != nil || !exit.TerminalEvent || !strings.Contains(string(answer), nonce) {
		t.Fatalf("resume exit=%+v err=%v nonce recalled=%t answer=%q", exit, err, strings.Contains(string(answer), nonce), answer)
	}
}

func TestCommandItemsMarkToolEvents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "codex")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"th"}' '{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"sleep 900"}}' '{"type":"item.started","item":{"id":"i2","type":"reasoning"}}' '{"type":"item.updated","item":{"id":"i1","type":"command_execution"}}' '{"type":"item.completed","item":{"id":"i1","type":"command_execution"}}' '{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{}}'
`
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.Command, a.AllowNative, a.AuthPath = path, true, ""
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "tool-items", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: adapter.ModeCorrection})
	if err != nil {
		t.Fatal(err)
	}
	var got [][]adapter.ToolEvent
	for event := range turn.Events() {
		if event.Kind == adapter.EventActivity && strings.HasPrefix(event.Summary, "item.") {
			got = append(got, event.Tools)
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if lo, ok := turn.(interface{ LastOutput() time.Time }); !ok || time.Since(lo.LastOutput()) > time.Minute {
		t.Fatalf("turn must report raw output liveness, got %v", turn)
	}
	want := [][]adapter.ToolEvent{{{ID: "i1"}}, nil, nil, {{ID: "i1", Done: true}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool events = %+v, want %+v", got, want)
	}
}

// The version probe has a 5 s deadline, and the deadline kills the launcher only. A launcher that starts helpers (an npm shim runs node)
// leaves them holding the pipe the probe reads, and the probe then waited for them for as long as they lived: a supervisor start that
// never finished. The helper here lives 20 s; the probe must give up at about 5 s plus the one second it grants for the pipe to close.
func TestVersionProbeDoesNotWaitForHelpersTheLauncherLeftBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	script := filepath.Join(t.TempDir(), "codex")
	if err := testenv.WriteExecutable(script, []byte("#!/bin/sh\nsleep 20 &\nsleep 20\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	implementation := New(t.TempDir())
	implementation.Command = script
	started := time.Now()
	entries := implementation.Catalog()
	if elapsed := time.Since(started); elapsed > 12*time.Second {
		t.Fatalf("the version probe took %v: it waited for the launcher's helpers", elapsed)
	}
	if len(entries) != 0 {
		t.Fatalf("a probe that timed out reported a version: %+v", entries)
	}
}

// The same launcher, when it exits 0: the version it printed is the version, and the probe is not delayed by the helper.
func TestVersionProbeSucceedsForALauncherThatExitedZeroAndLeftAHelperBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	script := filepath.Join(t.TempDir(), "codex")
	if err := testenv.WriteExecutable(script, []byte("#!/bin/sh\necho 'codex-cli 0.162.1'\nsleep 4 &\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	implementation := New(t.TempDir())
	implementation.Command = script
	started := time.Now()
	entries := implementation.Catalog()
	if len(entries) != 1 || entries[0].Version != "0.162.1" {
		t.Fatalf("catalog = %+v, want codex 0.162.1", entries)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the probe took %v: it waited for the helper", elapsed)
	}
}

// A keep-alive turn of a Codex agent differs from a real one only in what it says after the unchanged instruction text: the process is started with the
// same arguments (a resume of the same session) and environment, and standard input differs only by the keep_alive instruction in the turn JSON.
func TestKeepAliveTurnRunsWithTheRealTurnsArguments(t *testing.T) {
	root := t.TempDir()
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	path := filepath.Join(root, "codex-docker")
	run := func(name string, keepAlive bool) (args, stdin string) {
		argsPath, inPath := filepath.Join(root, name+"-args"), filepath.Join(root, name+"-in")
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\ncat > " + inPath + "\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"019e0000-0000-7000-8000-000000000001\"}'\n"
		if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		a := &Adapter{Command: path, Variant: "docker"}
		spec := adapter.TurnSpec{TurnID: name, Agent: adapter.AgentSpec{ID: "agt_test", Tool: "codex", Variant: "docker", Model: "gpt-6-sol"}, CheckoutPath: root, VisibleCheckoutPath: "/workspace/project", Mode: adapter.ModeNormal, AgentTools: config,
			NotesJSON: json.RawMessage(`[]`), NativeSessionHandle: "019e0000-0000-7000-8000-000000000001", NativeSessionStarted: true, KeepAlive: keepAlive}
		turn, err := a.Start(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		for range turn.Events() {
		}
		_, _ = turn.Wait()
		rawArgs, _ := os.ReadFile(argsPath)
		rawIn, _ := os.ReadFile(inPath)
		return string(rawArgs), string(rawIn)
	}
	realArgs, realIn := run("real", false)
	pingArgs, pingIn := run("ping", true)
	if realArgs != pingArgs {
		t.Fatalf("a ping must run with the real turn's arguments:\nreal: %s\nping: %s", realArgs, pingArgs)
	}
	if !strings.Contains(pingArgs, "resume\n") {
		t.Fatalf("a ping continues the session: %s", pingArgs)
	}
	if strings.Contains(realIn, "keep-alive") || !strings.Contains(pingIn, "cache keep-alive, not work") {
		t.Fatalf("real=%q ping=%q", realIn, pingIn)
	}
	prefix := func(in string) string { return in[:strings.Index(in, `{"turn_id"`)] }
	if prefix(realIn) != prefix(pingIn) {
		t.Fatal("the instruction text ahead of the turn JSON must not change")
	}
	if (&Adapter{}).KeepAliveTool() != "codex" {
		t.Fatal("the codex adapter runs keep-alive turns for codex")
	}
}

// Terminate asks a Codex turn to end with SIGTERM and kills it only if it does not: a launcher that traps the signal exits 143 on its own (a confirmed
// end), one that ignores it is killed after the grace. Without it a containerized Codex ping could only be cancelled, which confirms nothing.
func TestTurnTerminateEndsOnTheSignalOrIsKilledAfterTheGrace(t *testing.T) {
	// The script says it is ready (its trap is in place) by creating a file; the signal is sent only after that. A fixed wait instead raced the
	// shell's start: on a slow start the TERM arrived before the trap and the process died of the signal (exit code -1) instead of exiting 143.
	for name, tc := range map[string]struct {
		trap     string
		byItself bool
		code     int
	}{
		"traps TERM": {"trap 'exit 143' TERM", true, 143},
		"ignores it": {"trap '' TERM", false, -1},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "codex")
			ready := filepath.Join(root, "ready")
			script := "#!/bin/sh\ncat >/dev/null &\n" + tc.trap + "\n: > '" + ready + "'\nwhile true; do sleep 0.1; done\n"
			if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			a := New(filepath.Join(root, "runtime"))
			a.Command, a.AllowNative, a.AuthPath = path, true, ""
			turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "terminate", Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: adapter.ModeCorrection})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the launcher never said its trap is in place")
				}
				time.Sleep(5 * time.Millisecond)
			}
			terminator, ok := turn.(interface{ Terminate(time.Duration) bool })
			if !ok {
				t.Fatal("a Codex turn can be terminated")
			}
			if got := terminator.Terminate(300 * time.Millisecond); got != tc.byItself {
				t.Fatalf("ended on the terminate = %v, want %v", got, tc.byItself)
			}
			for range turn.Events() {
			}
			exit, _ := turn.Wait()
			if exit.TerminalEvent {
				t.Fatalf("exit = %+v", exit)
			}
			if tc.byItself && exit.ExitCode != tc.code {
				t.Fatalf("exit code = %d, want %d", exit.ExitCode, tc.code)
			}
		})
	}
}
