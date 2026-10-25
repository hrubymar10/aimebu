package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/adaptertest"
)

const testSession = "ses_eb9ff7679ffeTWs01Hgh3UTIS0"

func testToolsConfig(t *testing.T, agent string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tools.json")
	data := `{"mcpServers":{"aimebu":{"env":{"AIMEBU2_URL":"http://127.0.0.1:9897","AIMEBU2_AGENT_ID":"` + agent + `","AIMEBU2_AGENT_KEY":"aak_test"}}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
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
	return adapter.TurnSpec{TurnID: "turn_1", Agent: adapter.AgentSpec{ID: "agt_test", Tool: "opencode", Variant: "native", Model: "ollama-cloud/gemma4:31b"},
		CheckoutPath: checkout, AgentToolsConfigPath: testToolsConfig(t, "agt_test"), Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}
}

// script writes a fake opencode that records its arguments, selected
// environment, and stdin, then runs body.
func script(t *testing.T, root, body string) (path, record string) {
	t.Helper()
	path = filepath.Join(root, "opencode")
	record = filepath.Join(root, "record")
	source := "#!/bin/sh\n" +
		"{ printf 'ARGS:'; for a in \"$@\"; do printf '[%s]' \"$a\"; done; printf '\\n';" +
		" for v in XDG_CONFIG_HOME XDG_DATA_HOME OPENCODE_DB OPENCODE_CONFIG_CONTENT OPENCODE_DISABLE_AUTOUPDATE OPENCODE_DISABLE_PROJECT_CONFIG OPENCODE_DISABLE_CLAUDE_CODE OPENAI_API_KEY GITHUB_TOKEN CODEX_HOME CLAUDE_CONFIG_DIR AIMEBU2_AGENT_KEY; do eval \"printf '%s=%s\\n' $v \\\"\\${$v-<unset>}\\\"\"; done; } > " + record + "\n" +
		"prompt=$(cat)\nprintf 'PROMPT:%s\\n' \"$prompt\" >> " + record + "\n" + body
	if err := os.WriteFile(path, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return path, record
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
	path := filepath.Join(root, "opencode")
	marker := filepath.Join(root, "effect")
	events := fixture(t, "ok.jsonl")
	script := `#!/bin/sh
prompt=$(cat)
case "$prompt" in
  *turn-events*) cat ` + events + ` ;;
  *turn-no_terminal*) printf '%s\n' '{"type":"step_start","sessionID":"` + testSession + `","part":{"type":"step-start"}}' ;;
  *turn-cancel*) exec sleep 30 ;;
  *turn-crash_after_effect*) : > "` + marker + `"; exit 17 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
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
		return &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")},
			adapter.TurnSpec{TurnID: "turn-" + string(scenario), Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"},
				CheckoutPath: checkout, AgentToolsConfigPath: config, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}, effected
	}, adaptertest.ChildProcess())
}

func TestArgsEnvironmentAndIsolation(t *testing.T) {
	root := t.TempDir()
	path, record := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n")
	t.Setenv("OPENAI_API_KEY", "sk-test-provider-key")
	t.Setenv("GITHUB_TOKEN", "ghp_must_not_leak")
	t.Setenv("CODEX_HOME", "/must/not/leak")
	t.Setenv("CLAUDE_CONFIG_DIR", "/must/not/leak")
	t.Setenv("AIMEBU2_AGENT_KEY", "aak_must_not_leak")
	t.Setenv("XDG_DATA_HOME", "/real/xdg/data")
	runtimeRoot := filepath.Join(root, "runtime")
	sum := sha256.Sum256([]byte("agt_test"))
	runtimeDir := filepath.Join(runtimeRoot, hex.EncodeToString(sum[:]))
	checkout := t.TempDir()

	for _, tc := range []struct {
		name    string
		mutate  func(*adapter.TurnSpec)
		wantArg []string
		auto    bool
		mcp     bool
	}{
		{"first turn", func(*adapter.TurnSpec) {}, []string{"[run][--pure][--format][json][-m][ollama-cloud/gemma4:31b][--dir][" + checkout + "]"}, true, true},
		{"resumed turn", func(s *adapter.TurnSpec) { s.NativeSessionStarted, s.NativeSessionHandle = true, testSession }, []string{"[-s][" + testSession + "]"}, true, true},
		{"diagnostic turn", func(s *adapter.TurnSpec) { s.Mode = adapter.ModeDiagnostic }, []string{"[run][--pure]"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turnSpec := spec(t, checkout)
			tc.mutate(&turnSpec)
			if turnSpec.NativeSessionStarted {
				// A resumed turn must see its own session in the output.
				path, record = script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n")
			}
			a := &Adapter{Command: path, RuntimeRoot: runtimeRoot}
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
			for _, want := range tc.wantArg {
				if !strings.Contains(got, want) {
					t.Fatalf("args missing %s:\n%s", want, got)
				}
			}
			if hasAuto := strings.Contains(got, "[--auto]"); hasAuto != tc.auto {
				t.Fatalf("--auto = %t, want %t:\n%s", hasAuto, tc.auto, got)
			}
			for _, want := range []string{"XDG_CONFIG_HOME=" + filepath.Join(runtimeDir, "config"), "OPENCODE_DB=" + filepath.Join(runtimeDir, "opencode.db"),
				"OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_CLAUDE_CODE=1",
				"XDG_DATA_HOME=/real/xdg/data", "OPENAI_API_KEY=sk-test-provider-key", `"turn_id":"turn_1"`} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			for _, leaked := range []string{"ghp_must_not_leak", "/must/not/leak", "aak_must_not_leak"} {
				if strings.Contains(strings.Split(got, "PROMPT:")[0], leaked) {
					t.Fatalf("%q reached the child environment:\n%s", leaked, got)
				}
			}
			var config struct {
				MCP map[string]struct {
					Type, URL string
					Headers   map[string]string
				} `json:"mcp"`
				Permission map[string]string                 `json:"permission"`
				AutoUpdate bool                              `json:"autoupdate"`
				Share      string                            `json:"share"`
				Agent      map[string]struct{ Disable bool } `json:"agent"`
			}
			line := ""
			for _, l := range strings.Split(got, "\n") {
				if strings.HasPrefix(l, "OPENCODE_CONFIG_CONTENT=") {
					line = strings.TrimPrefix(l, "OPENCODE_CONFIG_CONTENT=")
				}
			}
			if err := json.Unmarshal([]byte(line), &config); err != nil {
				t.Fatalf("inline config %q: %v", line, err)
			}
			if config.AutoUpdate || config.Share != "disabled" || !config.Agent["title"].Disable {
				t.Fatalf("config must disable autoupdate, sharing, and title calls: %+v", config)
			}
			if tc.mcp {
				server := config.MCP["aimebu"]
				if server.Type != "remote" || !strings.HasPrefix(server.URL, "http://127.0.0.1:9897") || !strings.HasPrefix(server.Headers["Authorization"], "Bearer aak_") || len(config.Permission) != 0 {
					t.Fatalf("normal turn needs the aimebu MCP and no deny rules: %+v", config)
				}
			} else {
				if len(config.MCP) != 0 || config.Permission["bash"] != "deny" || config.Permission["edit"] != "deny" || config.Permission["webfetch"] != "deny" || config.Permission["external_directory"] != "deny" {
					t.Fatalf("diagnostic turn must deny acting tools and carry no MCP: %+v", config)
				}
			}
		})
	}
}

func TestSessionHandle(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "ok.jsonl")+"\n")
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	checkout := t.TempDir()

	first := spec(t, checkout)
	turn, err := a.Start(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err := drain(t, turn)
	if err != nil || !exit.TerminalEvent {
		t.Fatalf("first turn: exit=%+v err=%v", exit, err)
	}
	started := 0
	for _, event := range events {
		if event.Kind == adapter.EventNativeSessionStarted {
			started++
			if event.NativeSessionHandle != testSession {
				t.Fatalf("handle=%q", event.NativeSessionHandle)
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
	mismatched.NativeSessionStarted, mismatched.NativeSessionHandle = true, "ses_someone_else"
	turn, err = a.Start(context.Background(), mismatched)
	if err != nil {
		t.Fatal(err)
	}
	_, exit, err = drain(t, turn)
	if err == nil || !strings.Contains(err.Error(), "session ID changed") || exit.TerminalEvent {
		t.Fatalf("a resumed turn answering from another session must fail: exit=%+v err=%v", exit, err)
	}
}

func TestMultiStepTurnAnswersWithTheFinalStepAndSumsUsage(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "tool.jsonl")+"\n")
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	events, exit, err := drain(t, turn)
	if err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	var answer string
	var usage *adapter.Usage
	var starts, ends int
	for _, event := range events {
		switch event.Kind {
		case adapter.EventFinalAnswer:
			answer = string(event.Answer)
		case adapter.EventUsage:
			usage = event.Usage
		case adapter.EventActivity:
			for _, tool := range event.Tools {
				if tool.Done {
					ends++
				} else {
					starts++
				}
			}
		}
	}
	if !strings.Contains(answer, `"tool ran"`) || strings.Contains(answer, "write the file first") {
		t.Fatalf("the answer is the last step's text, not the narration before the tool: %q", answer)
	}
	if usage == nil || *usage.TokensIn != 31 || *usage.TokensOut != 11 || usage.CostEstimate == nil || *usage.CostEstimate < 0.0029 || *usage.CostEstimate > 0.0031 {
		t.Fatalf("usage must sum both steps: %+v", usage)
	}
	if usage.ContextUsed == nil || *usage.ContextUsed != 13 {
		t.Fatalf("context used is the last step's prompt, cache reads included: %+v", usage.ContextUsed)
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("the finished tool is reported as one start and one end: %d/%d", starts, ends)
	}
}

func TestProviderErrorIsEvidenceAndNeverTerminal(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "cat "+fixture(t, "limit.jsonl")+"\nexit 1\n")
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, exit, err := drain(t, turn)
	if err != nil {
		t.Fatal(err)
	}
	if exit.TerminalEvent || exit.ExitCode != 1 {
		t.Fatalf("exit=%+v", exit)
	}
	for _, want := range []string{"OpenCode error: APIError (HTTP 429)", "weekly usage limit", "resets in 3 hours"} {
		if !strings.Contains(exit.ErrorEvent, want) {
			t.Fatalf("error evidence %q missing %q", exit.ErrorEvent, want)
		}
	}
	if strings.Contains(exit.ErrorEvent, "aak_secret") {
		t.Fatalf("a bearer key reached the evidence: %q", exit.ErrorEvent)
	}
}

func TestIncompleteTurnsAreNotTerminal(t *testing.T) {
	step := func(reason, text string) string {
		out := `{"type":"step_start","sessionID":"` + testSession + `","part":{"type":"step-start"}}` + "\n"
		if text != "" {
			out += `{"type":"text","sessionID":"` + testSession + `","part":{"type":"text","text":` + quote(text) + `}}` + "\n"
		}
		return out + `{"type":"step_finish","sessionID":"` + testSession + `","part":{"type":"step-finish","reason":"` + reason + `","tokens":{"input":1,"output":1,"cache":{}}}}` + "\n"
	}
	for name, output := range map[string]string{
		"stop without text":              step("stop", ""),
		"text but the step was cut off":  step("length", `{"messages":[]}`),
		"a step that ends in tool calls": step("tool-calls", `{"messages":[]}`),
		"error after a stopped step":     step("stop", `{"messages":[]}`) + `{"type":"error","sessionID":"` + testSession + `","error":{"name":"UnknownError","data":{"message":"boom"}}}` + "\n",
		"no events at all":               "",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path, _ := script(t, root, "cat <<'EVENTS'\n"+output+"EVENTS\n")
			a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
			turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			events, exit, err := drain(t, turn)
			if err != nil || exit.TerminalEvent {
				t.Fatalf("must not be terminal: exit=%+v err=%v", exit, err)
			}
			for _, event := range events {
				if event.Kind == adapter.EventFinalAnswer {
					t.Fatal("unexpected final answer")
				}
			}
		})
	}
}

func TestFinalStepJoinsAllItsTextParts(t *testing.T) {
	events := `{"type":"step_start","sessionID":"` + testSession + `","part":{"type":"step-start"}}
{"type":"text","sessionID":"` + testSession + `","part":{"type":"text","text":"{\"messages\":[{\"room\":\"canary\","}}
{"type":"text","sessionID":"` + testSession + `","part":{"type":"text","text":"\"body\":\"two parts\"}]}"}}
{"type":"step_finish","sessionID":"` + testSession + `","part":{"type":"step-finish","reason":"stop","tokens":{"input":1,"output":1,"cache":{}}}}
`
	root := t.TempDir()
	path, _ := script(t, root, "cat <<'EVENTS'\n"+events+"EVENTS\n")
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	turn, err := a.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	got, exit, err := drain(t, turn)
	if err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	var answer string
	for _, event := range got {
		if event.Kind == adapter.EventFinalAnswer {
			answer = string(event.Answer)
		}
	}
	var parsed struct {
		Messages []struct{ Room, Body string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(answer), &parsed); err != nil || len(parsed.Messages) != 1 || parsed.Messages[0].Body != "two parts" {
		t.Fatalf("both parts of the final step form the answer: %q (%v)", answer, err)
	}
}

func quote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

func TestMissingSessionIsReportedOnStderr(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "echo 'Error: Session not found' >&2\nexit 1\n")
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	resumed := spec(t, t.TempDir())
	resumed.NativeSessionStarted, resumed.NativeSessionHandle = true, "ses_gone"
	turn, err := a.Start(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	_, exit, err := drain(t, turn)
	if err != nil || exit.TerminalEvent || !strings.Contains(exit.StderrTail, "Session not found") {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
}

func TestStartRejectsWhatOpenCodeCannotRun(t *testing.T) {
	root := t.TempDir()
	path, _ := script(t, root, "")
	a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}
	checkout := t.TempDir()
	cases := map[string]func(*adapter.TurnSpec){
		"docker variant":        func(s *adapter.TurnSpec) { s.Agent.Variant = "docker" },
		"model without a slash": func(s *adapter.TurnSpec) { s.Agent.Model = "gemma4" },
		"model ending in slash": func(s *adapter.TurnSpec) { s.Agent.Model = "ollama-cloud/" },
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
	path := filepath.Join(root, "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 1.19.0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path}
	catalog := a.Catalog()
	if len(catalog) != 1 || catalog[0].Tool != "opencode" || catalog[0].Version != "1.19.0" || catalog[0].Environment.Variant != "native" {
		t.Fatalf("catalog=%+v", catalog)
	}
	if tool, variant := a.CatalogIdentity(); tool != "opencode" || variant != "native" {
		t.Fatalf("identity=%s/%s", tool, variant)
	}
	supported := adapter.Environment{Variant: "native", OS: "linux"}
	if caps := a.Capabilities(SupportedVersion, supported); !caps.NoActionMode || !caps.Cost || !caps.CancelConfirmed {
		t.Fatalf("supported capabilities=%+v", caps)
	}
	for _, c := range []struct {
		version string
		env     adapter.Environment
	}{{"1.20.0", supported}, {SupportedVersion, adapter.Environment{Variant: "docker", OS: "linux"}}, {SupportedVersion, adapter.Environment{Variant: "native", OS: "darwin"}}} {
		if caps := a.Capabilities(c.version, c.env); caps != (adapter.Capabilities{}) {
			t.Fatalf("an unverified version or environment must advertise nothing: %+v for %+v", caps, c)
		}
	}
	if (&Adapter{Command: filepath.Join(root, "missing")}).Catalog() != nil {
		t.Fatal("a missing executable must not be listed")
	}
}

// What a turn's steps spent is known whether or not the turn finished: one that was cut off or failed still reports it, marked partial, so the
// limits count it; a finished one is not partial. Cache reads and writes are summed over the steps.
func TestUsageIsReportedForAnUnfinishedTurnAsPartial(t *testing.T) {
	finish := func(reason string) string {
		return `{"type":"step_start","sessionID":"` + testSession + `","part":{"type":"step-start"}}` + "\n" +
			`{"type":"step_finish","sessionID":"` + testSession + `","part":{"type":"step-finish","reason":"` + reason + `","cost":0.002,"tokens":{"input":4,"output":2,"cache":{"read":30,"write":5}}}}` + "\n"
	}
	for name, c := range map[string]struct {
		output  string
		partial bool
	}{
		"cut off in tool calls": {finish("tool-calls") + finish("tool-calls"), true},
		"failed after a step":   {finish("stop") + `{"type":"error","sessionID":"` + testSession + `","error":{"name":"UnknownError","data":{"message":"boom"}}}` + "\n", true},
		"finished": {finish("tool-calls") + `{"type":"step_start","sessionID":"` + testSession + `","part":{"type":"step-start"}}` + "\n" +
			`{"type":"text","sessionID":"` + testSession + `","part":{"type":"text","text":"{\"messages\":[]}"}}` + "\n" +
			`{"type":"step_finish","sessionID":"` + testSession + `","part":{"type":"step-finish","reason":"stop","cost":0.002,"tokens":{"input":4,"output":2,"cache":{"read":30,"write":5}}}}` + "\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path, _ := script(t, root, "cat <<'EVENTS'\n"+c.output+"EVENTS\n")
			turn, err := (&Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), spec(t, t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			events, _, _ := drain(t, turn)
			var usage *adapter.Usage
			for _, event := range events {
				if event.Kind == adapter.EventUsage {
					usage = event.Usage
				}
			}
			if usage == nil || usage.Partial != c.partial || usage.CostEstimate == nil || usage.TokensCacheRead == nil || usage.TokensCacheWrite == nil || *usage.TokensCacheWrite < 5 {
				t.Fatalf("usage=%+v, want partial=%v with cost and cache", usage, c.partial)
			}
			if usage.CostCumulative {
				t.Fatalf("opencode reports each step's own cost, not a running total: %+v", usage)
			}
		})
	}
}

// A step that reports no cost leaves the turn's cost unknown, not zero: a free-looking turn would be counted as free by every limit.
func TestAnUnpricedStepLeavesTheCostUnknown(t *testing.T) {
	output := `{"type":"step_start","sessionID":"` + testSession + `","part":{"type":"step-start"}}` + "\n" +
		`{"type":"text","sessionID":"` + testSession + `","part":{"type":"text","text":"{\"messages\":[]}"}}` + "\n" +
		`{"type":"step_finish","sessionID":"` + testSession + `","part":{"type":"step-finish","reason":"stop","tokens":{"input":4,"output":2,"cache":{}}}}` + "\n"
	root := t.TempDir()
	path, _ := script(t, root, "cat <<'EVENTS'\n"+output+"EVENTS\n")
	turn, err := (&Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	events, _, _ := drain(t, turn)
	var usage *adapter.Usage
	for _, event := range events {
		if event.Kind == adapter.EventUsage {
			usage = event.Usage
		}
	}
	if usage == nil || usage.TokensIn == nil || *usage.TokensIn != 4 || usage.CostEstimate != nil {
		t.Fatalf("usage=%+v, want tokens and no cost", usage)
	}
}
