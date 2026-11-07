package vibe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/adaptertest"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

// The fixtures in testdata are entries shaped like the ones `vibe -p --output streaming` writes (the PublicHistoryEntry models of Vibe
// 2.25.0: message, reasoning and effect entries, camelCase, one per line); the tests run a scripted `vibe` and never the real one.
const testSession = "0c3e1f4a-8b2d-4a6e-9f10-5d7c2b1a9e33"

func testToolsConfig(t *testing.T, agent string) *adapter.AgentToolsAccess {
	t.Helper()
	return &adapter.AgentToolsAccess{AgentID: agent, ServerURL: "http://127.0.0.1:9897", Key: "aak_test", Command: "/usr/bin/aimebu"}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func spec(t *testing.T, checkout string) adapter.TurnSpec {
	return adapter.TurnSpec{TurnID: "turn_1", Agent: adapter.AgentSpec{ID: "agt_test", Tool: "vibe", Variant: "native", Model: "mistral-medium-3.5"},
		CheckoutPath: checkout, AgentTools: testToolsConfig(t, "agt_test"), Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}
}

// adapterFor makes an adapter that runs the script at path, with a user home of its own.
func adapterFor(t *testing.T, root, path string) (*Adapter, string) {
	t.Helper()
	userHome := filepath.Join(root, "user-vibe")
	if err := os.MkdirAll(userHome, 0700); err != nil {
		t.Fatal(err)
	}
	return &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime"), UserHome: userHome}, userHome
}

func runtimeDirOf(root, agent string) string {
	sum := sha256.Sum256([]byte(agent))
	return filepath.Join(root, "runtime", hex.EncodeToString(sum[:]))
}

// script writes a fake vibe that records its arguments, selected environment, and stdin, then runs body.
func script(t *testing.T, root, body string) (path, record string) {
	t.Helper()
	path = filepath.Join(root, "vibe")
	record = filepath.Join(root, "record")
	source := "#!/bin/sh\n" +
		"{ printf 'ARGS:'; for a in \"$@\"; do printf '[%s]' \"$a\"; done; printf '\\n';" +
		" for v in VIBE_HOME VIBE_ACTIVE_MODEL VIBE_SESSION_LOGGING__SAVE_DIR VIBE_MCP_SERVERS AIMEBU2_AGENT_KEY MISTRAL_API_KEY ACME_API_KEY OPENAI_API_KEY GITHUB_TOKEN CODEX_HOME CLAUDE_CONFIG_DIR PYTHONUNBUFFERED; do eval \"printf '%s=%s\\n' $v \\\"\\${$v-<unset>}\\\"\"; done; } > " + record + "\n" +
		"prompt=$(cat)\nprintf 'PROMPT:%s\\n' \"$prompt\" >> " + record + "\n" + body
	if err := testenv.WriteExecutable(path, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return path, record
}

// persist is the part of a script that saves the session the way Vibe does after a turn: a directory named for the session with its stats.
func persist(sessionID string, prompt, completion, cached int, cost, inPrice, outPrice float64) string {
	return fmt.Sprintf(`d="$VIBE_SESSION_LOGGING__SAVE_DIR/session_20261024_000000_%s"
mkdir -p "$d"
printf '{"session_id":"%s","stats":{"session_prompt_tokens":%d,"session_completion_tokens":%d,"session_cached_tokens":%d,"context_tokens":%d,"session_cost":%g,"input_price_per_million":%g,"output_price_per_million":%g}}' > "$d/meta.json"
`, sessionID[:8], sessionID, prompt, completion, cached, prompt+completion, cost, inPrice, outPrice)
}

func drain(t *testing.T, turn adapter.Turn) ([]adapter.Event, adapter.ExitInfo, error) {
	t.Helper()
	var events []adapter.Event
	for event := range turn.Events() {
		events = append(events, event)
	}
	exit, err := turn.Wait()
	return events, exit, err
}

func answerOf(events []adapter.Event) string {
	for _, event := range events {
		if event.Kind == adapter.EventFinalAnswer {
			return string(event.Answer)
		}
	}
	return ""
}

func usageOf(events []adapter.Event) *adapter.Usage {
	for _, event := range events {
		if event.Kind == adapter.EventUsage {
			return event.Usage
		}
	}
	return nil
}

func TestTurnPromptSeparatesInboxAskFromAttention(t *testing.T) {
	prompt, err := turnInput(adapter.TurnSpec{TurnID: "prompt", Agent: adapter.AgentSpec{ID: "agt_prompt"}, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reply_to":null,"thread_root":null,"attachments":null`, `"needs_attention":false,"proposed_answers":null,"open_questions":null`, "Never combine ask with human attention controls.", "call the aimebu attachment_get tool", `"turn_id":"prompt"`} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("missing prompt guidance %q", want)
		}
	}
}

func TestConformance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "vibe")
	marker := filepath.Join(root, "effect")
	script := `#!/bin/sh
prompt=$(cat)
case "$prompt" in
  *turn-events*) cat ` + fixture(t, "minimal.jsonl") + `
` + persist(testSession, 120, 30, 20, 0.0004, 1.5, 7.5) + ` ;;
  *turn-no_terminal*) : ;;
  *turn-cancel*) exec sleep 30 ;;
  *turn-crash_after_effect*) : > "` + marker + `"; exit 17 ;;
esac
`
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	config := testToolsConfig(t, "agt_test")
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
		a, _ := adapterFor(t, t.TempDir(), path)
		return a, adapter.TurnSpec{TurnID: "turn-" + string(scenario), Agent: adapter.AgentSpec{ID: "agt_test", Model: "mistral-medium-3.5"},
			CheckoutPath: checkout, AgentTools: config, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}, effected
	}, adaptertest.ChildProcess())
}

func TestArgsEnvironmentAndIsolation(t *testing.T) {
	root := t.TempDir()
	path, record := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 10, 5, 0, 0.001, 1, 1))
	t.Setenv("MISTRAL_API_KEY", "mk-test-provider-key")
	t.Setenv("ACME_API_KEY", "acme-key-from-the-providers-table")
	t.Setenv("OPENAI_API_KEY", "sk-must-not-leak")
	t.Setenv("GITHUB_TOKEN", "ghp_must_not_leak")
	t.Setenv("CODEX_HOME", "/must/not/leak")
	t.Setenv("CLAUDE_CONFIG_DIR", "/must/not/leak")
	t.Setenv("AIMEBU2_AGENT_KEY", "aak_must_not_leak")
	a, userHome := adapterFor(t, root, path)
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte("[[providers]]\nname = \"acme\"\napi_key_env_var = \"ACME_API_KEY\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runtimeDir := runtimeDirOf(root, "agt_test")
	checkout := t.TempDir()

	for _, tc := range []struct {
		name    string
		mutate  func(*adapter.TurnSpec)
		wantArg []string
		yolo    bool
		mcp     bool
	}{
		{"first turn", func(*adapter.TurnSpec) {}, []string{"[-p][--output][streaming][--workdir][" + checkout + "]", "[--trust]"}, true, true},
		{"resumed turn", func(s *adapter.TurnSpec) { s.NativeSessionStarted, s.NativeSessionHandle = true, testSession }, []string{"[--resume][" + testSession + "]"}, true, true},
		{"diagnostic turn", func(s *adapter.TurnSpec) { s.Mode = adapter.ModeDiagnostic }, []string{"[--enabled-tools][read_file][--enabled-tools][grep]"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turnSpec := spec(t, checkout)
			tc.mutate(&turnSpec)
			turn, err := a.Start(context.Background(), turnSpec)
			if err != nil {
				t.Fatal(err)
			}
			_, exit, err := drain(t, turn)
			if err != nil || !exit.TerminalEvent {
				t.Fatalf("exit=%+v err=%v", exit, err)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			args := strings.SplitN(got, "\n", 2)[0]
			for _, want := range tc.wantArg {
				if !strings.Contains(args, want) {
					t.Fatalf("args missing %s:\n%s", want, got)
				}
			}
			if hasYolo := strings.Contains(args, "[--yolo]"); hasYolo != tc.yolo {
				t.Fatalf("--yolo = %t, want %t:\n%s", hasYolo, tc.yolo, got)
			}
			if trusted := strings.Contains(args, "[--trust]"); trusted != tc.yolo {
				t.Fatalf("--trust = %t, want %t (only a normal turn loads the checkout's own Vibe configuration):\n%s", trusted, tc.yolo, got)
			}
			if tc.mcp == strings.Contains(args, "--enabled-tools") {
				t.Fatalf("a read-only turn, and only that, restricts the tools:\n%s", got)
			}
			if strings.Contains(args, "aak_") || strings.Contains(args, "http://") {
				t.Fatalf("the toolbox must not travel in argv:\n%s", args)
			}
			for _, want := range []string{"VIBE_HOME=" + filepath.Join(runtimeDir, "home"), "VIBE_ACTIVE_MODEL=mistral-medium-3.5",
				"VIBE_SESSION_LOGGING__SAVE_DIR=" + filepath.Join(runtimeDir, "sessions"), "MISTRAL_API_KEY=mk-test-provider-key",
				"ACME_API_KEY=acme-key-from-the-providers-table", "PYTHONUNBUFFERED=1", `"turn_id":"turn_1"`} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			child := strings.Split(got, "PROMPT:")[0]
			for _, leaked := range []string{"sk-must-not-leak", "ghp_must_not_leak", "/must/not/leak", "aak_must_not_leak"} {
				if strings.Contains(child, leaked) {
					t.Fatalf("%q reached the child environment:\n%s", leaked, got)
				}
			}
			var servers []map[string]any
			line := ""
			for _, l := range strings.Split(child, "\n") {
				if strings.HasPrefix(l, "VIBE_MCP_SERVERS=") {
					line = strings.TrimPrefix(l, "VIBE_MCP_SERVERS=")
				}
			}
			if !tc.mcp {
				if line != "<unset>" || strings.Contains(child, "AIMEBU2_AGENT_KEY=aak_") {
					t.Fatalf("a read-only turn carries no toolbox:\n%s", child)
				}
				return
			}
			if err := json.Unmarshal([]byte(line), &servers); err != nil || len(servers) != 1 {
				t.Fatalf("VIBE_MCP_SERVERS %q: %v", line, err)
			}
			server := servers[0]
			if server["name"] != "aimebu" || server["transport"] != "streamable-http" || !strings.HasPrefix(fmt.Sprint(server["url"]), "http://127.0.0.1:9897") || server["api_key_env"] != "AIMEBU2_AGENT_KEY" {
				t.Fatalf("toolbox config: %+v", server)
			}
			if strings.Contains(line, "aak_") || !strings.Contains(child, "AIMEBU2_AGENT_KEY=aak_test\n") {
				t.Fatalf("the bearer is in its own variable, bare, and not in the config: %s", child)
			}
		})
	}
}

// The private home holds the person's models and providers (so a model they listed resolves) and their own MCP servers, but not a server of
// aimebu's name (a v1 bus; aimebu's own entry comes through the environment) and nothing else of their config; their .env is linked, never read.
func TestSeedHomeCopiesModelsProvidersAndTheirOwnMCPServers(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n")
	a, userHome := adapterFor(t, root, path)
	config := "active_model = \"custom\"\nenable_telemetry = true\n\n[[models]]\nname = \"m-custom\"\nalias = \"custom\"\nprovider = \"acme\"\n\n[[providers]]\nname = \"acme\"\napi_key_env_var = \"ACME_API_KEY\"\n\n[[mcp_servers]]\nname = \"grafana\"\ntransport = \"stdio\"\ncommand = \"uvx\"\n\n[[mcp_servers]]\nname = \"aimebu\"\ntransport = \"streamable-http\"\nurl = \"http://v1.example/mcp\"\n\n[hooks]\nsecret_hook = \"rm -rf\"\n"
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(userHome, ".env")
	if err := os.WriteFile(env, []byte("MISTRAL_API_KEY=never-read\n"), 0000); err != nil { // unreadable: any attempt to open it would fail
		t.Fatal(err)
	}
	turnSpec := spec(t, t.TempDir())
	turnSpec.Agent.Model = "custom"
	turn, err := a.Start(context.Background(), turnSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	home := filepath.Join(runtimeDirOf(root, "agt_test"), "home")
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"enable_telemetry = false", "enable_update_checks = false", "enable_connectors = false", `alias = "custom"`, `name = "acme"`, `name = "grafana"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("private config misses %q:\n%s", want, got)
		}
	}
	for _, leaked := range []string{"secret_hook", "active_model", "enable_telemetry = true", "v1.example", `name = "aimebu"`} {
		if strings.Contains(got, leaked) {
			t.Fatalf("%q from the person's config reached the private one:\n%s", leaked, got)
		}
	}
	if target, err := os.Readlink(filepath.Join(home, ".env")); err != nil || target != env {
		t.Fatalf(".env link: %q %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(home, "config.toml")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private config mode: %v %v", info, err)
	}
	// Without a user home the turn still runs: Vibe's own defaults apply.
	bare := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "bare"), UserHome: filepath.Join(root, "no-such-home")}
	turn, err = bare.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("no user config: exit=%+v err=%v", exit, err)
	}
}

func TestSessionHandleIsReportedAfterACleanExitOnly(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 10, 5, 0, 0.001, 1, 1))
	a, _ := adapterFor(t, root, path)
	checkout := t.TempDir()

	turn, err := a.Start(context.Background(), spec(t, checkout))
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err := drain(t, turn)
	if err != nil || !exit.TerminalEvent {
		t.Fatalf("first turn: exit=%+v err=%v", exit, err)
	}
	started := 0
	for i, event := range events {
		if event.Kind == adapter.EventNativeSessionStarted {
			started++
			if event.NativeSessionHandle != testSession || i != 0 && events[i-1].Kind == adapter.EventProcessExit {
				t.Fatalf("handle=%q at %d", event.NativeSessionHandle, i)
			}
		}
	}
	if started != 1 {
		t.Fatalf("a first turn reports its session once, got %d", started)
	}

	resumed := spec(t, checkout)
	resumed.NativeSessionStarted, resumed.NativeSessionHandle = true, testSession
	turn, err = a.Start(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err = drain(t, turn)
	if err != nil || !exit.TerminalEvent {
		t.Fatalf("resumed turn: exit=%+v err=%v", exit, err)
	}
	for _, event := range events {
		if event.Kind == adapter.EventNativeSessionStarted {
			t.Fatal("a resumed turn must not report a new session")
		}
	}

	mismatched := spec(t, checkout)
	mismatched.NativeSessionStarted, mismatched.NativeSessionHandle = true, "11111111-8b2d-4a6e-9f10-5d7c2b1a9e33"
	turn, err = a.Start(context.Background(), mismatched)
	if err != nil {
		t.Fatal(err)
	}
	_, exit, err = drain(t, turn)
	if err == nil || !strings.Contains(err.Error(), "session ID changed") || exit.TerminalEvent {
		t.Fatalf("a resumed turn answering from another session must fail: exit=%+v err=%v", exit, err)
	}

	// A first turn that fails persists no session, so none is reported even though its entries named one.
	failing, _ := script(t, t.TempDir(), "cat "+fixture(t, "ok.jsonl")+"\nexit 1\n")
	b, _ := adapterFor(t, t.TempDir(), failing)
	turn, err = b.Start(context.Background(), spec(t, checkout))
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err = drain(t, turn)
	if err != nil || exit.TerminalEvent || exit.ExitCode != 1 {
		t.Fatalf("failing turn: exit=%+v err=%v", exit, err)
	}
	for _, event := range events {
		if event.Kind == adapter.EventNativeSessionStarted || event.Kind == adapter.EventFinalAnswer {
			t.Fatalf("a failed turn reports no session and no answer: %+v", event)
		}
	}
}

// A resumed session replays its earlier history before this turn's own prompt: only what follows that prompt is this turn's answer.
func TestResumedTurnIgnoresReplayedHistory(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "resumed.jsonl")+"\n"+persist(testSession, 10, 5, 0, 0.001, 1, 1))
	a, _ := adapterFor(t, root, path)
	resumed := spec(t, t.TempDir())
	resumed.NativeSessionStarted, resumed.NativeSessionHandle = true, testSession
	turn, err := a.Start(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err := drain(t, turn)
	if err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	if answer := answerOf(events); !strings.Contains(answer, "NEW ANSWER") || strings.Contains(answer, "OLD ANSWER") {
		t.Fatalf("answer=%q", answer)
	}
	// Without this turn's prompt in the stream there is nothing to tell the new answer from the old: not a terminal turn.
	replayOnly := strings.Join(strings.Split(strings.TrimSpace(readFile(t, fixture(t, "resumed.jsonl"))), "\n")[:2], "\n")
	only, _ := script(t, t.TempDir(), "cat <<'EVENTS'\n"+replayOnly+"\nEVENTS\n")
	b, _ := adapterFor(t, t.TempDir(), only)
	turn, err = b.Start(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err = drain(t, turn)
	if err != nil || exit.TerminalEvent || answerOf(events) != "" {
		t.Fatalf("a replay with no turn of its own must not answer: exit=%+v err=%v", exit, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMultiStepTurnAnswersWithTheLastMessageAndReportsTools(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "tool.jsonl")+"\n"+persist(testSession, 40, 12, 10, 0.0003, 1, 1))
	a, _ := adapterFor(t, root, path)
	turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err := drain(t, turn)
	if err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	answer := answerOf(events)
	if !strings.Contains(answer, `"tool ran"`) || strings.Contains(answer, "read the file first") {
		t.Fatalf("the answer is the last message, not the narration before the tool: %q", answer)
	}
	var starts, ends int
	for _, event := range events {
		for _, tool := range event.Tools {
			if tool.Done {
				ends++
			} else {
				starts++
			}
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("a finished effect is one start and one end: %d/%d", starts, ends)
	}
}

func TestStopEventIsEvidenceAndNeverAnAnswer(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "stop.jsonl")+"\n"+persist(testSession, 10, 5, 0, 0.001, 1, 1)+"exit 1\n")
	a, _ := adapterFor(t, root, path)
	turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err := drain(t, turn)
	if err != nil || exit.TerminalEvent || exit.ExitCode != 1 || answerOf(events) != "" {
		t.Fatalf("exit=%+v err=%v answer=%q", exit, err, answerOf(events))
	}
	if !strings.Contains(exit.ErrorEvent, "Vibe stopped the turn") || !strings.Contains(exit.ErrorEvent, "Turn limit of 2 reached") {
		t.Fatalf("error evidence: %q", exit.ErrorEvent)
	}
	if usage := usageOf(events); usage == nil || !usage.Partial {
		t.Fatalf("what an unfinished turn spent still counts, as partial: %+v", usage)
	}
}

func TestProviderErrorOnStderrIsEvidence(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "echo 'Error: API error from mistral (HTTP 429): weekly usage limit reached, resets in 3 hours. Bearer aak_secret' >&2\nexit 1\n")
	a, _ := adapterFor(t, root, path)
	turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, exit, err := drain(t, turn)
	if err != nil || exit.TerminalEvent || exit.ExitCode != 1 {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	for _, want := range []string{"Vibe error: API error from mistral (HTTP 429)", "weekly usage limit", "resets in 3 hours"} {
		if !strings.Contains(exit.ErrorEvent, want) {
			t.Fatalf("error evidence %q missing %q", exit.ErrorEvent, want)
		}
	}
	if strings.Contains(exit.ErrorEvent, "aak_secret") || strings.Contains(exit.StderrTail, "aak_secret") {
		t.Fatalf("a bearer key reached the evidence: %q / %q", exit.ErrorEvent, exit.StderrTail)
	}
}

func TestIncompleteTurnsAreNotTerminal(t *testing.T) {
	prompt := `{"id":"u","sessionId":"` + testSession + `","type":"message","role":"user","source":"turn_start","generationStatus":"completed","content":[{"type":"text","text":"p"}]}` + "\n"
	assistant := func(text string) string {
		return `{"id":"a","sessionId":"` + testSession + `","type":"message","role":"assistant","generationStatus":"completed","content":[{"type":"text","text":` + quote(text) + `}]}` + "\n"
	}
	for name, c := range map[string]struct{ output, tail string }{
		"the prompt and no reply":     {prompt, ""},
		"an empty reply":              {prompt + assistant(""), ""},
		"a reply, then exit 1":        {prompt + assistant(`{"messages":[]}`), "exit 1\n"},
		"no entries at all":           {"", ""},
		"a reply under another id":    {prompt + strings.Replace(assistant(`{"messages":[]}`), testSession, "22222222-aaaa", 1), ""},
		"an entry that is not a turn": {`{"type":"notice","sessionId":"` + testSession + `","level":"info","message":"hi"}` + "\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path, _ := script(t, root, "cat <<'EVENTS'\n"+c.output+"EVENTS\n"+c.tail)
			a, _ := adapterFor(t, root, path)
			turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			events, exit, _ := drain(t, turn)
			if exit.TerminalEvent || answerOf(events) != "" {
				t.Fatalf("must not be terminal: exit=%+v", exit)
			}
		})
	}
}

func quote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

func TestMissingSessionIsReportedOnStderr(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "echo 'Error: Session not found: "+testSession+"' >&2\nexit 1\n")
	a, _ := adapterFor(t, root, path)
	resumed := spec(t, t.TempDir())
	resumed.NativeSessionStarted, resumed.NativeSessionHandle = true, testSession
	turn, err := a.Start(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	_, exit, err := drain(t, turn)
	if err != nil || exit.TerminalEvent || !strings.Contains(exit.StderrTail, "Error: Session not found") {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
}

// What the turn spent is read from the session's stats once it has exited. Vibe counts the whole session and counts cached tokens inside the
// prompt, so a turn's own share is the difference from before it, cached tokens are split out of the input, and the cost stays Vibe's
// cumulative figure (the store takes the difference).
func TestUsageIsThisTurnsShareOfTheSession(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 1000, 150, 400, 0.0042, 1.5, 7.5))
	a, _ := adapterFor(t, root, path)
	saveDir := filepath.Join(runtimeDirOf(root, "agt_test"), "sessions")

	turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	events, _, _ := drain(t, turn)
	usage := usageOf(events)
	if usage == nil || *usage.TokensIn != 600 || *usage.TokensOut != 150 || *usage.TokensCacheRead != 400 || usage.Partial {
		t.Fatalf("first turn usage=%+v: cached tokens are split out of the prompt", usage)
	}
	if usage.CostEstimate == nil || *usage.CostEstimate != 0.0042 || !usage.CostCumulative || usage.ContextUsed == nil || *usage.ContextUsed != 1150 {
		t.Fatalf("first turn cost=%+v: Vibe's own cumulative cost, and the context of its last call", usage)
	}

	// The session carried 700/100/200 into this turn: the turn's own share is the rest.
	if err := os.RemoveAll(saveDir); err != nil {
		t.Fatal(err)
	}
	seed, _ := script(t, t.TempDir(), persist(testSession, 700, 100, 200, 0.0030, 1.5, 7.5))
	if out, err := execScript(seed, saveDir); err != nil {
		t.Fatalf("seed the earlier totals: %v %s", err, out)
	}
	resumed := spec(t, t.TempDir())
	resumed.NativeSessionStarted, resumed.NativeSessionHandle = true, testSession
	turn, err = a.Start(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	events, _, _ = drain(t, turn)
	usage = usageOf(events)
	if usage == nil || *usage.TokensIn != 100 || *usage.TokensOut != 50 || *usage.TokensCacheRead != 200 {
		t.Fatalf("resumed usage=%+v, want the turn's own 300 prompt (100 + 200 cached) and 50 completion", usage)
	}
}

func execScript(path, saveDir string) (string, error) {
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(), "VIBE_SESSION_LOGGING__SAVE_DIR="+saveDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// A model with no price leaves the cost unknown rather than zero, and a resumed turn killed before Vibe saved the session spent nothing
// that is known (the totals did not move), so it reports nothing.
func TestUnpricedAndUnsavedTurnsReportNoCost(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 100, 20, 0, 0, 0, 0))
	a, _ := adapterFor(t, root, path)
	turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	events, _, _ := drain(t, turn)
	if usage := usageOf(events); usage == nil || *usage.TokensIn != 100 || usage.CostEstimate != nil {
		t.Fatalf("usage=%+v, want tokens and no cost", usage)
	}

	killed, _ := script(t, t.TempDir(), "cat "+fixture(t, "ok.jsonl")+"\nexit 143\n")
	b, _ := adapterFor(t, t.TempDir(), killed)
	turn, err = b.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	events, exit, _ := drain(t, turn)
	if usageOf(events) != nil || exit.TerminalEvent {
		t.Fatalf("a turn that persisted nothing reports no usage: %+v", usageOf(events))
	}
}

func TestStartRejectsWhatVibeCannotRun(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "")
	a, _ := adapterFor(t, root, path)
	checkout := t.TempDir()
	cases := map[string]func(*adapter.TurnSpec){
		"docker variant":        func(s *adapter.TurnSpec) { s.Agent.Variant = "docker" },
		"model with a space":    func(s *adapter.TurnSpec) { s.Agent.Model = "mistral medium" },
		"model with a newline":  func(s *adapter.TurnSpec) { s.Agent.Model = "a\nb" },
		"model starting with -": func(s *adapter.TurnSpec) { s.Agent.Model = "-x" },
		"no model":              func(s *adapter.TurnSpec) { s.Agent.Model = "" },
		"no checkout":           func(s *adapter.TurnSpec) { s.CheckoutPath = "" },
		"resume without handle": func(s *adapter.TurnSpec) { s.NativeSessionStarted = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := spec(t, checkout)
			mutate(&s)
			if _, err := a.Start(context.Background(), s); err == nil {
				t.Fatal("must be rejected before anything runs")
			}
		})
	}
	if _, err := (&Adapter{Command: path}).Start(context.Background(), spec(t, checkout)); err == nil {
		t.Fatal("a missing runtime root must be rejected")
	}
}

func TestCatalogAndCapabilities(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "vibe")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho 'vibe 2.25.0'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path}
	catalog := a.Catalog()
	if len(catalog) != 1 || catalog[0].Tool != "vibe" || catalog[0].Version != "2.25.0" || catalog[0].Environment.Variant != "native" {
		t.Fatalf("catalog=%+v", catalog)
	}
	if tool, variant := a.CatalogIdentity(); tool != "vibe" || variant != "native" {
		t.Fatalf("identity=%s/%s", tool, variant)
	}
	supported := adapter.Environment{Variant: "native", OS: "linux"}
	caps := a.Capabilities(SupportedVersion, supported)
	if !caps.Cost || !caps.CancelConfirmed || caps.NoActionMode || caps.ReadonlyVerified {
		t.Fatalf("supported capabilities=%+v: no-action mode is claimed only after the live canary", caps)
	}
	for _, c := range []struct {
		version string
		env     adapter.Environment
	}{{"2.26.0", supported}, {SupportedVersion, adapter.Environment{Variant: "docker", OS: "linux"}}, {SupportedVersion, adapter.Environment{Variant: "native", OS: "darwin"}}} {
		if caps := a.Capabilities(c.version, c.env); caps != (adapter.Capabilities{}) {
			t.Fatalf("an unverified version or environment must advertise nothing: %+v for %+v", caps, c)
		}
	}
	if (&Adapter{Command: filepath.Join(root, "missing")}).Catalog() != nil {
		t.Fatal("a missing executable must not be listed")
	}
	empty := filepath.Join(root, "silent")
	if err := testenv.WriteExecutable(empty, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if (&Adapter{Command: empty}).Catalog() != nil {
		t.Fatal("an executable that names no version must not be listed")
	}
}

// An owner's environment references cannot replace what isolates the agent, nor give a read-only turn a toolbox.
func TestOwnerEnvironmentReferencesCannotOverrideTheIsolation(t *testing.T) {
	root := t.TempDir()
	path, record := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 10, 5, 0, 0.001, 1, 1))
	t.Setenv("OWNER_HOME", "/the/persons/own/vibe")
	t.Setenv("OWNER_SERVERS", `[{"name":"theirs","transport":"stdio","command":"rm"}]`)
	t.Setenv("OWNER_KEY", "aak_owner_chosen")
	t.Setenv("OWNER_OTHER", "fine")
	a, _ := adapterFor(t, root, path)
	runtimeDir := runtimeDirOf(root, "agt_test")
	for name, mode := range map[string]adapter.Mode{"normal": adapter.ModeNormal, "diagnostic": adapter.ModeDiagnostic} {
		t.Run(name, func(t *testing.T) {
			turnSpec := spec(t, t.TempDir())
			turnSpec.Mode = mode
			turnSpec.LauncherEnvRefs = map[string]string{"VIBE_HOME": "OWNER_HOME", "VIBE_MCP_SERVERS": "OWNER_SERVERS", "AIMEBU2_AGENT_KEY": "OWNER_KEY",
				"VIBE_ACTIVE_MODEL": "OWNER_HOME", "VIBE_SESSION_LOGGING__SAVE_DIR": "OWNER_HOME", "ACME_API_KEY": "OWNER_OTHER"}
			turn, err := a.Start(context.Background(), turnSpec)
			if err != nil {
				t.Fatal(err)
			}
			if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
				t.Fatalf("exit=%+v err=%v", exit, err)
			}
			got := strings.Split(readFile(t, record), "PROMPT:")[0]
			for _, want := range []string{"VIBE_HOME=" + filepath.Join(runtimeDir, "home"), "VIBE_ACTIVE_MODEL=mistral-medium-3.5", "VIBE_SESSION_LOGGING__SAVE_DIR=" + filepath.Join(runtimeDir, "sessions"), "ACME_API_KEY=fine"} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			for _, leaked := range []string{"/the/persons/own/vibe", "aak_owner_chosen", `"theirs"`} {
				if strings.Contains(got, leaked) {
					t.Fatalf("an owner's reference replaced %q:\n%s", leaked, got)
				}
			}
			if mode == adapter.ModeNormal && !strings.Contains(got, `"name":"aimebu"`) {
				t.Fatalf("a normal turn keeps the aimebu toolbox:\n%s", got)
			}
			if mode != adapter.ModeNormal && !strings.Contains(got, "VIBE_MCP_SERVERS=<unset>") {
				t.Fatalf("a read-only turn must carry no toolbox whatever the owner forwards:\n%s", got)
			}
		})
	}
}

// A resumed session whose totals before the turn cannot be read has nothing to subtract from: the tokens stay unknown rather than being
// the whole session's, and the cumulative cost is still reported.
func TestResumedTurnWithoutABaselineReportsNoTokens(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 1000, 150, 400, 0.0042, 1.5, 7.5))
	a, _ := adapterFor(t, root, path)
	resumed := spec(t, t.TempDir())
	resumed.NativeSessionStarted, resumed.NativeSessionHandle = true, testSession // no stats were saved for this session before the turn
	turn, err := a.Start(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err := drain(t, turn)
	if err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	usage := usageOf(events)
	if usage == nil || usage.TokensIn != nil || usage.TokensOut != nil || usage.TokensCacheRead != nil {
		t.Fatalf("usage=%+v: the session's whole history must not be counted as this turn's", usage)
	}
	if usage.CostEstimate == nil || *usage.CostEstimate != 0.0042 || !usage.CostCumulative || usage.Partial {
		t.Fatalf("usage=%+v: the cumulative cost still counts", usage)
	}
}

// The fixtures named recorded_* follow the keys, types and values of real `vibe -p --output streaming` runs and meta.json files (what a
// person recorded during them, ids and prompt text trimmed): every entry has ids, millisecond times, generationStatus and relatedEntryId;
// turn_start is a source value on a user message; a resumed session replays earlier messages as source "harness"; a finished tool call is
// one effect entry; the session totals count the cached tokens inside the prompt tokens.
func TestRecordedShapes(t *testing.T) {
	meta := func(t *testing.T) string {
		return "d=\"$VIBE_SESSION_LOGGING__SAVE_DIR/session_20261024_120000_" + testSession[:8] + "\"\nmkdir -p \"$d\"\ncp " + fixture(t, "recorded_meta.json") + " \"$d/meta.json\"\n"
	}
	t.Run("a normal turn", func(t *testing.T) {
		root := t.TempDir()
		path, _ := script(t, root, "cat "+fixture(t, "recorded_normal.jsonl")+"\n"+meta(t))
		a, _ := adapterFor(t, root, path)
		turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		events, exit, err := drain(t, turn)
		if err != nil || !exit.TerminalEvent || !strings.Contains(answerOf(events), "recorded shape") {
			t.Fatalf("exit=%+v err=%v answer=%q", exit, err, answerOf(events))
		}
		usage := usageOf(events)
		if usage == nil || *usage.TokensIn != 2769 || *usage.TokensOut != 121 || *usage.TokensCacheRead != 2176 || *usage.ContextUsed != 5066 {
			t.Fatalf("usage=%+v: 4945 prompt tokens of which 2176 cached", usage)
		}
		if usage.CostEstimate == nil || *usage.CostEstimate != 0.0053874 || !usage.CostCumulative {
			t.Fatalf("cost=%+v", usage.CostEstimate)
		}
	})
	t.Run("a finished tool call", func(t *testing.T) {
		root := t.TempDir()
		path, _ := script(t, root, "cat "+fixture(t, "recorded_tool.jsonl")+"\n"+meta(t))
		a, _ := adapterFor(t, root, path)
		turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		events, exit, err := drain(t, turn)
		if err != nil || !exit.TerminalEvent {
			t.Fatalf("exit=%+v err=%v", exit, err)
		}
		var starts, ends int
		for _, event := range events {
			for _, tool := range event.Tools {
				if tool.Done {
					ends++
				} else {
					starts++
				}
			}
		}
		if starts != 1 || ends != 1 {
			t.Fatalf("one effect entry is one finished tool call: %d/%d", starts, ends)
		}
	})
	t.Run("a resumed turn", func(t *testing.T) {
		root := t.TempDir()
		path, _ := script(t, root, "cat "+fixture(t, "recorded_resume.jsonl")+"\n"+meta(t))
		a, _ := adapterFor(t, root, path)
		saveDir := filepath.Join(runtimeDirOf(root, "agt_test"), "sessions")
		earlier, _ := script(t, t.TempDir(), "d=\""+saveDir+"/session_20261024_110000_"+testSession[:8]+"\"\nmkdir -p \"$d\"\n"+
			`printf '{"session_id":"`+testSession+`","stats":{"session_prompt_tokens":3000,"session_completion_tokens":80,"session_cached_tokens":1000,"session_cost":0.003,"input_price_per_million":1.5,"output_price_per_million":7.5}}' > "$d/meta.json"`+"\n")
		if out, err := execScript(earlier, saveDir); err != nil {
			t.Fatalf("earlier totals: %v %s", err, out)
		}
		resumed := spec(t, t.TempDir())
		resumed.NativeSessionStarted, resumed.NativeSessionHandle = true, testSession
		turn, err := a.Start(context.Background(), resumed)
		if err != nil {
			t.Fatal(err)
		}
		events, exit, err := drain(t, turn)
		if err != nil || !exit.TerminalEvent {
			t.Fatalf("exit=%+v err=%v", exit, err)
		}
		if answer := answerOf(events); !strings.Contains(answer, "NEW ANSWER") || strings.Contains(answer, "OLD ANSWER") {
			t.Fatalf("answer=%q: the replayed messages are not this turn's", answer)
		}
		// The recorded totals are 4945/121/2176 after the turn and the session had 3000/80/1000 before it.
		if usage := usageOf(events); usage == nil || *usage.TokensIn != 769 || *usage.TokensOut != 41 || *usage.TokensCacheRead != 1176 {
			t.Fatalf("usage=%+v", usage)
		}
	})
	t.Run("a stop event", func(t *testing.T) {
		root := t.TempDir()
		path, _ := script(t, root, "cat "+fixture(t, "recorded_stop.jsonl")+"\n"+meta(t)+"exit 1\n")
		a, _ := adapterFor(t, root, path)
		turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		events, exit, err := drain(t, turn)
		if err != nil || exit.TerminalEvent || answerOf(events) != "" || exit.ExitCode != 1 || !strings.Contains(exit.ErrorEvent, "Turn limit of 2 reached") {
			t.Fatalf("exit=%+v err=%v", exit, err)
		}
	})
}

// A container has none of the host's commands: it gets the person's remote MCP servers (with their key variable), not the ones that run a command.
func TestSeedHomeKeepsOnlyTheRemoteMCPServersForAContainer(t *testing.T) {
	userHome, home := t.TempDir(), t.TempDir()
	config := "[[models]]\nname = \"m\"\nalias = \"custom\"\nprovider = \"acme\"\n\n[[mcp_servers]]\nname = \"grafana\"\ntransport = \"stdio\"\ncommand = \"uvx\"\napi_key_env = \"GRAFANA_KEY\"\n\n[[mcp_servers]]\nname = \"docs\"\ntransport = \"streamable-http\"\nurl = \"https://docs.example/mcp\"\napi_key_env = \"DOCS_KEY\"\n"
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := seedHome(home, userHome, mcpRemote)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if strings.Contains(string(data), "grafana") || !strings.Contains(string(data), `name = "docs"`) || !strings.Contains(string(data), `alias = "custom"`) {
		t.Fatalf("the remote server stays, the command goes:\n%s", data)
	}
	if joined := strings.Join(env, ","); strings.Contains(joined, "GRAFANA_KEY") || !strings.Contains(joined, "DOCS_KEY") {
		t.Fatalf("only the kept server's variable: %v", env)
	}
	env, err = seedHome(home, userHome, mcpAll)
	data, _ = os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || !strings.Contains(string(data), "grafana") || !strings.Contains(strings.Join(env, ","), "GRAFANA_KEY") {
		t.Fatalf("a native turn carries both: %v\n%s\n%v", err, data, env)
	}
	env, err = seedHome(home, userHome, mcpNone)
	data, _ = os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || strings.Contains(string(data), "docs.example") || strings.Contains(strings.Join(env, ","), "DOCS_KEY") {
		t.Fatalf("a read-only turn carries none: %v\n%s\n%v", err, data, env)
	}
}

// An entry that is aimebu's own bus of v1 is dropped whatever it is called; a read-only turn gets none of the person's servers.
func TestSeedHomeDropsAV1BusUnderAnotherName(t *testing.T) {
	userHome, home := t.TempDir(), t.TempDir()
	config := "[[mcp_servers]]\nname = \"bus\"\ntransport = \"stdio\"\ncommand = \"/usr/local/bin/aimebu\"\nargs = [\"mcp\"]\n\n[[mcp_servers]]\nname = \"v1port\"\ntransport = \"streamable-http\"\nurl = \"http://localhost:9997/mcp\"\n\n[[mcp_servers]]\nname = \"AIMEBU\"\nurl = \"http://upper\"\n\n[[mcp_servers]]\nname = \"docs\"\ntransport = \"streamable-http\"\nurl = \"https://docs.example/mcp\"\napi_key_env = \"VIBE_HOME\"\n"
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := seedHome(home, userHome, mcpAll)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	got := string(data)
	if strings.Contains(got, "9997") || strings.Contains(got, "usr/local/bin/aimebu") || strings.Contains(got, "http://upper") || !strings.Contains(got, `name = "docs"`) {
		t.Fatalf("only the person's own server:\n%s", got)
	}
	if strings.Contains(strings.Join(env, ","), "VIBE_HOME") {
		t.Fatalf("a name that would point Vibe at the person's home is not passed: %v", env)
	}
}

func TestAReadOnlyTurnGetsNoneOfThePersonsMCPServers(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n")
	a, userHome := adapterFor(t, root, path)
	config := "[[models]]\nname = \"m-custom\"\nalias = \"custom\"\nprovider = \"acme\"\n\n[[mcp_servers]]\nname = \"grafana\"\ntransport = \"stdio\"\ncommand = \"uvx\"\napi_key_env = \"GRAFANA_KEY\"\n"
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	turnSpec := spec(t, t.TempDir())
	turnSpec.Agent.Model = "custom"
	turnSpec.Mode = adapter.ModeDiagnostic
	turn, err := a.Start(context.Background(), turnSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	data, err := os.ReadFile(filepath.Join(runtimeDirOf(root, "agt_test"), "home", "config.toml"))
	if err != nil || strings.Contains(string(data), "grafana") || !strings.Contains(string(data), `alias = "custom"`) {
		t.Fatalf("a read-only turn keeps the models and none of the MCP servers: %v\n%s", err, data)
	}
}
