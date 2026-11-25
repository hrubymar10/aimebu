package pi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

const testSession = "123e4567-e89b-42d3-a456-426614174000"

func TestTurnPromptSeparatesInboxAskFromAttention(t *testing.T) {
	prompt, err := turnInput(adapter.TurnSpec{TurnID: "prompt", Agent: adapter.AgentSpec{ID: "agt_prompt"}, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reply_to":null,"attachments":null`, `"handoff":false,"outcome":null,"visual_plan":null,"appendix_pages":null`, "Every message with handoff true needs outcome: done when the task is complete and verified", `"needs_attention":false,"proposed_answers":null,"open_questions":null`, "a live @slug or @agent_id mention", "after four automatic replies with no human message between them, replies stop waking anyone", "never send acknowledgement-only", "publish the home-room summary"} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("missing prompt guidance %q", want)
		}
	}
}

func TestTurnInputIncludesScopedDockerAttachmentAccess(t *testing.T) {
	input, err := turnInput(adapter.TurnSpec{TurnID: "turn_1", Agent: adapter.AgentSpec{Variant: "docker"},
		NotesJSON: json.RawMessage(`[{"attachments":[{"id":"att_1"}]}]`), ContextJSON: json.RawMessage(`{}`),
		AttachmentBaseURL: "http://host.docker.internal:9897/api/agent/turns/turn_1/attachments/", AttachmentBearer: "aat_private"})
	if err != nil || !strings.Contains(string(input), "aat_private") || !strings.Contains(string(input), "host.docker.internal:9897") {
		t.Fatalf("Docker attachment access missing: %s, %v", input, err)
	}
}

func testToolsConfig(t *testing.T, agent string) *adapter.AgentToolsAccess {
	t.Helper()
	return &adapter.AgentToolsAccess{AgentID: agent, ServerURL: "http://127.0.0.1:9897", Key: "aak_test", Command: "/usr/bin/aimebu"}
}

func TestConformance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "pi")
	marker := filepath.Join(root, "effect")
	script := `#!/bin/sh
prompt=$(cat)
case "$prompt" in
  *turn-events*)
    printf '%s\n' '{"type":"session","id":"SESSION"}' '{"type":"turn_start"}' \
      '{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"messages\":[]}"}],"usage":{"input":11,"output":7,"cost":{"total":0.01}}}}' \
      '{"type":"compaction_end","result":{"tokensBefore":100,"estimatedTokensAfter":20}}' \
      '{"type":"agent_settled","aborted":false}' ;;
  *turn-no_terminal*) printf '%s\n' '{"type":"session","id":"SESSION"}' '{"type":"turn_start"}' ;;
  *turn-cancel*) exec sleep 30 ;;
  *turn-crash_after_effect*) : > "EFFECT"; exit 17 ;;
esac
`
	script = strings.ReplaceAll(strings.ReplaceAll(script, "SESSION", testSession), "EFFECT", marker)
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
		return &Adapter{Command: path, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")},
			adapter.TurnSpec{TurnID: "turn-" + string(scenario), Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"},
				CheckoutPath: checkout, AgentTools: config, NativeSessionHandle: testSession, Mode: adapter.ModeNormal,
				NotesJSON: json.RawMessage(`[]`)}, effected
	}, adaptertest.ChildProcess())
}

func TestNativeMCPAndDockerIsolation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi")
	argsPath := filepath.Join(root, "args")
	envPath := filepath.Join(root, "env")
	forwardPath := filepath.Join(root, "forward")
	fixture, err := filepath.Abs(filepath.Join("testdata", "print.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "key")
	// The container's own agent directory: what the launcher's command mode sees as PI_CODING_AGENT_DIR, and what the person has natively.
	realDir := filepath.Join(root, "real-agent")
	for name, content := range map[string]string{"models.json": `{"providers":{"mine":{}}}`, "auth.json": `{"mine":{"key":"x"}}`,
		"settings.json": `{"packages":["git:example/pkg"]}`, "mcp.json": `{"mcpServers":{}}`, "git/pkg/index.ts": "export {}", "sessions/old.jsonl": "{}", ".hidden": "h"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(realDir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realDir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PI_CODING_AGENT_DIR", realDir)
	script := "#!/bin/sh\n" +
		`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then echo forward_env; echo run_command; exit 0; fi` + "\n" +
		// Command mode: the launcher runs the line in the container, whose own variable names its agent directory; nothing is forwarded.
		`if [ -n "${HARNESS_DOCKER_RUN_COMMAND+x}" ]; then printf '%s' "${AIMEBU_AGENT_KEY:-}${HARNESS_DOCKER_FORWARD_ENV:-}" > ` + filepath.Join(root, "command-env") + "; PI_CODING_AGENT_DIR=" + realDir + ` exec sh -c "$HARNESS_DOCKER_RUN_COMMAND"; fi` + "\n" +
		"printf '%s\\n' \"$@\" > " + argsPath + "\nprintf '%s' \"${PI_CODING_AGENT_DIR:-}\" > " + envPath + "\n" +
		"printf '%s' \"${AIMEBU_AGENT_KEY:-}\" > " + keyPath + "\nprintf '%s' \"${HARNESS_DOCKER_FORWARD_ENV:-}\" > " + forwardPath + "\n" +
		"cat " + fixture + "\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := testToolsConfig(t, "agt_test")
	for _, variant := range []string{"native", "docker"} {
		t.Run(variant, func(t *testing.T) {
			checkout := t.TempDir()
			a := &Adapter{Command: path, Variant: variant, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}
			turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: variant, Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"},
				CheckoutPath: checkout, AgentTools: config, NativeSessionHandle: testSession, Mode: adapter.ModeNormal})
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
			envDir, err := os.ReadFile(envPath)
			if err != nil {
				t.Fatal(err)
			}
			forward, err := os.ReadFile(forwardPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), "--session-id\n"+testSession) || !strings.Contains(string(args), "--session-dir\n"+filepath.Join(checkout, ".aimebu", "pi-sessions")) {
				t.Fatalf("session args=%q", args)
			}
			// The allowlist names every MCP tool: Pi keeps an unmatched MCP tool registered but inactive,
			// so a direct tool is declared to the model only when the allowlist matches it.
			if !strings.Contains(string(args), "mcp__*") || strings.Contains(string(args), "--no-mcp") || strings.Contains(string(args), "--mcp-config") {
				t.Fatalf("%s toolbox args=%q", variant, args)
			}
			// Pi Docker may run with the pi-mcp-adapter package, which names aimebu's tools aimebu_<tool>.
			if hasAdapterNames := strings.Contains(string(args), "aimebu_*"); hasAdapterNames != (variant == "docker") {
				t.Fatalf("%s allowlist must name the adapter's tool spelling only for Docker: %q", variant, args)
			}
			sum := sha256.Sum256([]byte("agt_test"))
			privateDir := filepath.Join(root, "runtime", hex.EncodeToString(sum[:]))
			if variant == "docker" {
				privateDir = filepath.Join(checkout, ".aimebu", "pi", hex.EncodeToString(sum[:]))
			}
			// The private directory replaces Pi's agent directory, so what Pi needs to find its models and login must be linked into it:
			// everything for Docker (the container's own directory, but mcp.json and sessions), and the person's models and login natively.
			wantLinked := map[string]bool{"models.json": true, "auth.json": true}
			if variant == "docker" {
				for _, name := range []string{"settings.json", "git", ".hidden"} {
					wantLinked[name] = true
				}
			}
			entries, err := os.ReadDir(privateDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() == "mcp.json" {
					if entry.Type()&os.ModeSymlink != 0 {
						t.Fatal("the private mcp.json must be the adapter's own file, not a link to the person's")
					}
					continue
				}
				target, err := os.Readlink(filepath.Join(privateDir, entry.Name()))
				if err != nil || target != filepath.Join(realDir, entry.Name()) || !wantLinked[entry.Name()] {
					t.Fatalf("unexpected entry %q in the private agent directory: target=%q err=%v", entry.Name(), target, err)
				}
				delete(wantLinked, entry.Name())
			}
			if len(wantLinked) != 0 {
				t.Fatalf("missing links in the private agent directory: %v", wantLinked)
			}
			if variant == "docker" {
				// The layering run forwards nothing: no key and no variable list.
				if env, err := os.ReadFile(filepath.Join(root, "command-env")); err != nil || len(env) != 0 {
					t.Fatalf("the layering command must carry no key and forward nothing: %q err=%v", env, err)
				}
			}
			var configPath string
			if variant == "native" {
				if string(envDir) == "" {
					t.Fatal("native Pi config directory was not isolated")
				}
				configPath = filepath.Join(root, "runtime", hex.EncodeToString(sum[:]), "mcp.json")
			} else {
				// The container's config directory is under the mirrored checkout, named by the variable the launcher forwards.
				if !strings.Contains(string(args), "--session-dir\n"+filepath.Join(checkout, ".aimebu", "pi-sessions")) || string(envDir) != filepath.Join(checkout, ".aimebu", "pi", hex.EncodeToString(sum[:])) {
					t.Fatalf("docker Pi config directory=%q args=%q", envDir, args)
				}
				if string(forward) != "PI_CODING_AGENT_DIR,AIMEBU_AGENT_KEY" {
					t.Fatalf("the launcher is told which variables to forward: %q", forward)
				}
				configPath = filepath.Join(checkout, ".aimebu", "pi", hex.EncodeToString(sum[:]), "mcp.json")
			}
			info, err := os.Stat(configPath)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("private Pi config mode=%v err=%v", info, err)
			}
			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			wantHost := "127.0.0.1"
			if variant == "docker" {
				wantHost = "host.docker.internal"
			}
			if !strings.Contains(string(data), wantHost+":9897/api/agent/mcp") {
				t.Fatalf("private Pi endpoint=%q err=%v", data, err)
			}
			// The key is not in the file, nor in the arguments: the header names the variable Pi expands, and the key travels in the
			// environment (to the harness directly when native, through the launcher by name when Docker).
			if strings.Contains(string(data), config.Key) || !strings.Contains(string(data), "Bearer ${AIMEBU_AGENT_KEY}") || strings.Contains(string(args), config.Key) {
				t.Fatalf("the key must not be in the private Pi config or the arguments: %s / %q", data, args)
			}
			if key, err := os.ReadFile(keyPath); err != nil || string(key) != config.Key {
				t.Fatalf("the key reaches the launcher only in its environment: %q err=%v", key, err)
			}
		})
	}
}

// A Docker launcher that cannot forward environment variables would start Pi without its configuration and toolbox: the turn is refused
// before anything starts, naming the way out.
func TestDockerTurnRefusesALauncherWithoutForwardEnv(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi-docker")
	script := "#!/bin/sh\necho 1.1.0\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "docker"}
	_, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "old-launcher", Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"},
		CheckoutPath: t.TempDir(), AgentTools: testToolsConfig(t, "agt_test"), NativeSessionHandle: testSession, Mode: adapter.ModeNormal})
	if err == nil || !strings.Contains(err.Error(), "forward environment variables") {
		t.Fatalf("an old launcher must be refused with the way out: %v", err)
	}
}

func TestLargeTurnPromptUsesStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	command := filepath.Join(root, "pi")
	argsPath := filepath.Join(root, "args")
	inputPath := filepath.Join(root, "input")
	fixture, err := filepath.Abs(filepath.Join("testdata", "print.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\ncat > " + inputPath + "\ncat " + fixture + "\n"
	if err := testenv.WriteExecutable(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	longNote := strings.Repeat("x", 160*1024)
	notes, err := json.Marshal([]map[string]string{{"body": longNote}})
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: command, Variant: "native", AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "pi-large", Agent: adapter.AgentSpec{ID: "agt_large_pi", Model: "ollama-cloud/gemma4:31b"},
		CheckoutPath: t.TempDir(), AgentTools: testToolsConfig(t, "agt_large_pi"), NativeSessionHandle: testSession,
		Mode: adapter.ModeNormal, NotesJSON: notes})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	if exit, err := turn.Wait(); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v error=%v", exit, err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil || strings.Contains(string(args), longNote[:100]) {
		t.Fatalf("large prompt appeared in argv: error=%v", err)
	}
	input, err := os.ReadFile(inputPath)
	if err != nil || !strings.Contains(string(input), longNote) {
		t.Fatalf("large prompt absent from stdin: bytes=%d error=%v", len(input), err)
	}
}

func TestParserRequiresMatchingSessionAndSettledRun(t *testing.T) {
	for _, scenario := range []struct{ name, lines string }{
		{"wrong session", `'{"type":"session","id":"wrong"}' '{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"messages\":[]}"}]}}' '{"type":"agent_settled","aborted":false}'`},
		{"missing turn end", `'{"type":"session","id":"` + testSession + `"}' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"messages\":[]}"}]}}' '{"type":"agent_settled","aborted":false}'`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "pi")
			script := "#!/bin/sh\nprintf '%s\\n' " + scenario.lines + "\n"
			if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "incomplete", Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"}, CheckoutPath: root, NativeSessionHandle: testSession, Mode: adapter.ModeCorrection})
			if err != nil {
				t.Fatal(err)
			}
			for range turn.Events() {
			}
			exit, err := turn.Wait()
			if err != nil || exit.TerminalEvent {
				t.Fatalf("incomplete Pi turn exit=%+v err=%v", exit, err)
			}
		})
	}
}

func TestCorrectionAndDiagnosticDisableActionTools(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi")
	argsPath := filepath.Join(root, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nprintf '%s\\n' '{\"type\":\"session\",\"id\":\"" + testSession + "\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []adapter.Mode{adapter.ModeCorrection, adapter.ModeDiagnostic} {
		turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: string(mode),
			Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"}, CheckoutPath: root,
			NativeSessionHandle: testSession, Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		for range turn.Events() {
		}
		if _, err := turn.Wait(); err != nil {
			t.Fatal(err)
		}
		args, err := os.ReadFile(argsPath)
		if err != nil || !strings.Contains(string(args), "--tools\nread,grep,find,ls\n") || !strings.Contains(string(args), "--no-mcp\n") || strings.Contains(string(args), "--mcp-config") {
			t.Fatalf("%s args=%q err=%v", mode, args, err)
		}
	}
}

func TestPrintFixtureParsesSessionAnswerUsageAndCompaction(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi")
	fixture, err := filepath.Abs(filepath.Join("testdata", "print.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat "+fixture+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "fixture", Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"}, CheckoutPath: root, NativeSessionHandle: testSession, Mode: adapter.ModeCorrection})
	if err != nil {
		t.Fatal(err)
	}
	var handle, answer string
	var usage *adapter.Usage
	var compact *adapter.Compaction
	for event := range turn.Events() {
		switch event.Kind {
		case adapter.EventNativeSessionStarted:
			handle = event.NativeSessionHandle
		case adapter.EventFinalAnswer:
			answer = string(event.Answer)
		case adapter.EventUsage:
			usage = event.Usage
		case adapter.EventCompaction:
			compact = event.Compaction
		}
	}
	exit, err := turn.Wait()
	if err != nil || !exit.TerminalEvent || handle != testSession || !strings.Contains(answer, "BLUE-314") ||
		usage == nil || usage.TokensIn == nil || *usage.TokensIn != 11 || usage.CostEstimate == nil || *usage.CostEstimate != 0.01 ||
		compact == nil || compact.PreTokens != 2125 || compact.PostTokens != 2033 {
		t.Fatalf("fixture handle=%q answer=%q usage=%+v compact=%+v exit=%+v err=%v", handle, answer, usage, compact, exit, err)
	}
}

func TestCompactUsesRPC(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi")
	requestPath := filepath.Join(root, "request")
	script := "#!/bin/sh\nread request\nprintf '%s' \"$request\" > " + requestPath + "\nprintf '%s\\n' '{\"type\":\"compaction_start\",\"reason\":\"manual\"}' '{\"type\":\"compaction_end\",\"result\":{\"tokensBefore\":2125,\"estimatedTokensAfter\":2033}}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	compact, err := (&Adapter{Command: path, AllowNative: true}).Compact(context.Background(), adapter.TurnSpec{
		Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"}, CheckoutPath: root, NativeSessionHandle: testSession})
	if err != nil || compact.PreTokens != 2125 || compact.PostTokens != 2033 {
		t.Fatalf("compact=%+v err=%v", compact, err)
	}
	request, err := os.ReadFile(requestPath)
	if err != nil || string(request) != `{"type":"compact"}` {
		t.Fatalf("RPC request=%q err=%v", request, err)
	}
}

func TestLiveCanary(t *testing.T) {
	docker := os.Getenv("AIMEBU_LIVE_PI_DOCKER") == "1"
	if !docker && os.Getenv("AIMEBU_LIVE_PI") != "1" {
		t.Skip("set AIMEBU_LIVE_PI=1 or AIMEBU_LIVE_PI_DOCKER=1 for the two-turn Pi canary")
	}
	checkout := t.TempDir()
	a := New(filepath.Join(t.TempDir(), "runtime"))
	variant := "native"
	if docker {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Join(cwd, "..", "..", "..", "tmp")
		if err := os.MkdirAll(base, 0700); err != nil {
			t.Fatal(err)
		}
		checkout, err = os.MkdirTemp(base, "pi-docker-canary-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(checkout) })
		a, variant = NewDocker(), "docker"
	} else {
		a.AllowNative = true
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(random[:])
	config := testToolsConfig(t, "agt_live_pi")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(id, note string, started bool) (string, bool) {
		notes, _ := json.Marshal([]map[string]string{{"body": note}})
		turn, err := a.Start(ctx, adapter.TurnSpec{TurnID: id, Agent: adapter.AgentSpec{ID: "agt_live_pi", Variant: variant, Model: "ollama-cloud/gemma4:31b"},
			CheckoutPath: checkout, AgentTools: config, NativeSessionHandle: testSession, NativeSessionStarted: started,
			Mode: adapter.ModeNormal, NotesJSON: notes, ContextJSON: json.RawMessage(`{"rooms":[{"room_id":"canary"}]}`)})
		if err != nil {
			t.Fatal(err)
		}
		var answer string
		confirmed := false
		for event := range turn.Events() {
			if event.Kind == adapter.EventNativeSessionStarted {
				confirmed = event.NativeSessionHandle == testSession
			}
			if event.Kind == adapter.EventFinalAnswer {
				answer = string(event.Answer)
			}
		}
		exit, err := turn.Wait()
		if err != nil || !exit.TerminalEvent || answer == "" {
			t.Fatalf("Pi canary exit=%+v err=%v answer=%q", exit, err, answer)
		}
		return answer, confirmed
	}
	_, confirmed := run("pi-first", "Remember nonce "+nonce+". Reply with valid JSON in room canary.", false)
	if !confirmed {
		t.Fatal("Pi did not confirm its server-issued session ID")
	}
	answer, _ := run("pi-second", "Recall the nonce from the previous turn and reply with it in room canary as JSON.", true)
	if !strings.Contains(answer, nonce) {
		t.Fatalf("Pi did not recall nonce: %q", answer)
	}
}

func TestToolExecutionEventsCarryToolIDs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"turn_start\"}' '{\"type\":\"tool_execution_start\",\"toolCallId\":\"call-1\"}' '{\"type\":\"tool_execution_end\",\"toolCallId\":\"call-1\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "tool-ids", Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"}, CheckoutPath: root, NativeSessionHandle: testSession, Mode: adapter.ModeCorrection})
	if err != nil {
		t.Fatal(err)
	}
	var got [][]adapter.ToolEvent
	for event := range turn.Events() {
		if event.Kind == adapter.EventActivity {
			got = append(got, event.Tools)
		}
	}
	_, _ = turn.Wait()
	if lo, ok := turn.(interface{ LastOutput() time.Time }); !ok || time.Since(lo.LastOutput()) > time.Minute {
		t.Fatalf("turn must report raw output liveness, got %v", turn)
	}
	want := [][]adapter.ToolEvent{nil, {{ID: "call-1"}}, {{ID: "call-1", Done: true}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool events = %+v, want %+v", got, want)
	}
}

func TestRetriesExhaustedExitNamesTheProviderError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi")
	failed := `{"type":"turn_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"429 weekly limit"}}`
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"printf '%s\\n' \"Warning: No project session found with id '" + testSession + "'; creating a new session with that id.\" \"real launcher noise\" >&2\n" +
		"printf '%s\\n' '{\"type\":\"session\",\"id\":\"" + testSession + "\"}' '{\"type\":\"turn_start\"}' '" + failed + "' " +
		"'{\"type\":\"auto_retry_start\",\"attempt\":1,\"maxAttempts\":3,\"delayMs\":2000,\"errorMessage\":\"429 weekly limit\"}' " +
		"'{\"type\":\"turn_start\"}' '" + failed + "' " +
		"'{\"type\":\"auto_retry_end\",\"success\":false,\"attempt\":3,\"finalError\":\"429 weekly usage limit, try again in 3 hours\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	turn, err := (&Adapter{Command: path, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), adapter.TurnSpec{TurnID: "retries", Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"}, CheckoutPath: root, AgentTools: testToolsConfig(t, "agt_test"), NativeSessionHandle: testSession, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	exit, err := turn.Wait()
	if err != nil || exit.TerminalEvent || exit.ExitCode != 0 {
		t.Fatalf("exhausted retries must be a non-terminal clean exit: exit=%+v err=%v", exit, err)
	}
	if want := "Pi provider error: 429 weekly usage limit, try again in 3 hours"; exit.ErrorEvent != want {
		t.Fatalf("error event = %q, want %q", exit.ErrorEvent, want)
	}
	if strings.Contains(exit.StderrTail, "No project session found") || !strings.Contains(exit.StderrTail, "real launcher noise") {
		t.Fatalf("stderr must drop only the session notice: %q", exit.StderrTail)
	}
}

func TestSuccessfulRetryLeavesNoProviderError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' " +
		"'{\"type\":\"turn_end\",\"message\":{\"role\":\"assistant\",\"content\":[],\"stopReason\":\"error\",\"errorMessage\":\"503 overloaded\"}}' " +
		"'{\"type\":\"auto_retry_end\",\"success\":true,\"attempt\":1}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	turn, err := (&Adapter{Command: path, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), adapter.TurnSpec{TurnID: "recovered", Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"}, CheckoutPath: root, AgentTools: testToolsConfig(t, "agt_test"), NativeSessionHandle: testSession, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	exit, _ := turn.Wait()
	if exit.ErrorEvent != "" {
		t.Fatalf("a retry that succeeded left an error behind: %q", exit.ErrorEvent)
	}
}

func TestPiUsageCarriesCacheTokensWhenReported(t *testing.T) {
	var usage piUsage
	if err := json.Unmarshal([]byte(`{"input":11,"output":3,"cacheRead":40,"cacheWrite":6,"cost":{"total":0.01}}`), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.CacheRead == nil || *usage.CacheRead != 40 || usage.CacheWrite == nil || *usage.CacheWrite != 6 {
		t.Fatalf("cache: %+v", usage)
	}
	if err := json.Unmarshal([]byte(`{"input":11,"output":3}`), &usage); err != nil {
		t.Fatal(err)
	}
}

// Without command mode the launcher cannot build the container's agent directory: Pi would start with no model, so the turn is refused first.
func TestDockerTurnRefusesALauncherWithoutRunCommand(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pi-docker")
	script := "#!/bin/sh\necho forward_env\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "docker"}
	_, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "no-run-command", Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"},
		CheckoutPath: t.TempDir(), AgentTools: testToolsConfig(t, "agt_test"), NativeSessionHandle: testSession, Mode: adapter.ModeNormal})
	if err == nil || !strings.Contains(err.Error(), "support run_command") {
		t.Fatalf("a launcher without command mode must be refused with the way out: %v", err)
	}
}

// A stale link from an earlier turn is followed to the container's current entries: one that vanished is dropped, one that appeared is added.
func TestLinkUserFilesReplacesStaleLinks(t *testing.T) {
	root := t.TempDir()
	source, dir := filepath.Join(root, "user"), filepath.Join(root, "private")
	for _, d := range []string{source, dir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "models.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(dir, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := linkUserFiles(dir, source, "models.json", "auth.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "auth.json")); err == nil {
		t.Fatal("a stale link to a file the person no longer has must be dropped")
	}
	if got, err := os.Readlink(filepath.Join(dir, "models.json")); err != nil || got != filepath.Join(source, "models.json") {
		t.Fatalf("models.json link=%q err=%v", got, err)
	}
}
