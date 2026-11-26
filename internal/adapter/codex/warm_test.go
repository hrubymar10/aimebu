package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func warmHelperScript(t *testing.T, mode string) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "codex")
	body := "#!/bin/sh\nexport AIMEBU_CODEX_WARM_FAKE_MODE=" + strconv.Quote(mode) + "\nexec " + strconv.Quote(binary) + " -test.run=TestCodexWarmHelper -- \"$@\"\n"
	if err := testenv.WriteExecutable(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWarmContainerEndEvidenceAfterTerm(t *testing.T) {
	w := &codexWarmSession{containerized: true, termSent: true, exitCode: 0}
	if err := w.confirmed(); err != nil {
		t.Fatalf("launcher completed EOF cleanup after TERM: %v", err)
	}
	w.exitCode = 125
	if err := w.confirmed(); err == nil {
		t.Fatal("unknown launcher outcome released the session")
	}
	w.exitCode = 143
	if err := w.confirmed(); err != nil {
		t.Fatalf("verified TERM path: %v", err)
	}
}

func TestWarmContainerEndReportOverridesLauncherStatus(t *testing.T) {
	identity := &adapter.ContainerIdentity{}
	identity.EnableEndReport()
	filter := identity.Filter(io.Discard)
	token := strings.Repeat("a", 64) + ":42-7-99"
	_, _ = filter.Write([]byte(adapter.DockerExecIDPrefix + token + "\n"))
	_ = filter.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { identity.ReadEndReport(read); close(done) }()
	_, _ = write.WriteString(adapter.DockerSessionEndPrefix + token + " killed\n")
	_ = write.Close()
	<-done
	w := &codexWarmSession{containerized: true, container: identity, termSent: true, exitCode: 125}
	if err := w.confirmed(); err != nil {
		t.Fatalf("matching end report: %v", err)
	}
}

func TestWarmSessionEndFDLauncherFailureIsNotWarmFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-docker")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\n[ \"$HARNESS_DOCKER_SESSION_END_FD\" = 3 ] || exit 9\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a, spec := warmSpec(t, path)
	spec.LauncherContainerized = true
	spec.LauncherReportsContainer = true
	spec.LauncherReportsSessionEnd = true
	_, err := a.StartWarm(context.Background(), spec)
	if !errors.Is(err, adapter.ErrWarmLaunch) {
		t.Fatalf("launcher exit 2 must be a start failure: %v", err)
	}
}

func TestCodexWarmHelper(t *testing.T) {
	mode := os.Getenv("AIMEBU_CODEX_WARM_FAKE_MODE")
	if mode == "" {
		return
	}
	reader := bufio.NewScanner(os.Stdin)
	writer := json.NewEncoder(os.Stdout)
	turn := 0
	sendSuccess := func() {
		_ = writer.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]string{"id": "answer", "type": "agentMessage", "text": `{"messages":[]}`, "phase": "final_answer"}}})
		total := map[string]int{"inputTokens": turn * 100, "outputTokens": turn * 20, "cachedInputTokens": turn * 10}
		if mode == "resume-usage" || mode == "resume-usage-multi" {
			total = map[string]int{"inputTokens": 1000, "outputTokens": 200, "cachedInputTokens": 100}
		}
		_ = writer.Encode(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"total": total, "last": map[string]int{"inputTokens": 100, "outputTokens": 20, "cachedInputTokens": 10}}}})
		if mode == "resume-usage-multi" {
			_ = writer.Encode(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{
				"total": map[string]int{"inputTokens": 1120, "outputTokens": 230, "cachedInputTokens": 120},
				"last":  map[string]int{"inputTokens": 120, "outputTokens": 30, "cachedInputTokens": 20}}}})
		}
		_ = writer.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"id": fmt.Sprintf("turn-%d", turn), "status": "completed"}}})
	}
	for reader.Scan() {
		var frame struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(reader.Bytes(), &frame) != nil {
			continue
		}
		switch frame.Method {
		case "initialize":
			home := os.Getenv("CODEX_HOME")
			if mode == "bad-home" {
				home = "/wrong/home"
			}
			_ = writer.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"codexHome": home}})
		case "model/list":
			_ = writer.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"data": []map[string]string{{"id": "gpt-6-sol", "defaultReasoningEffort": "medium"}, {"id": "gpt-6.1-sol", "defaultReasoningEffort": "low"}}, "nextCursor": nil}})
		case "thread/start", "thread/resume":
			if mode == "require-defaults" {
				var params map[string]any
				_ = json.Unmarshal(frame.Params, &params)
				if _, set := params["personality"]; set {
					_ = writer.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -1, "message": "thread personality overrode the cold default"}})
					continue
				}
			}
			id := "thread-fake"
			if mode == "resume-mismatch" && frame.Method == "thread/resume" {
				id = "different"
			}
			_ = writer.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"thread": map[string]string{"id": id}}})
		case "turn/start":
			if mode == "require-defaults" {
				var params map[string]any
				_ = json.Unmarshal(frame.Params, &params)
				want := "medium"
				if params["model"] == "gpt-6.1-sol" {
					want = "low"
				}
				_, personalitySet := params["personality"]
				if params["effort"] != want || personalitySet {
					_ = writer.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -1, "message": "turn defaults were not explicit"}})
					continue
				}
			}
			turn++
			_ = writer.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"turn": map[string]any{"id": fmt.Sprintf("turn-%d", turn), "status": "inProgress"}}})
			if mode == "stale" && turn > 1 {
				_ = writer.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"turnId": "turn-1", "item": map[string]string{"type": "agentMessage", "text": `{"wrong":true}`, "phase": "final_answer"}}})
				_ = writer.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"id": "turn-1", "status": "completed"}}})
			}
			_ = writer.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"turn": map[string]string{"id": fmt.Sprintf("turn-%d", turn)}}})
			switch mode {
			case "hold":
				// The next request can observe a live turn still in progress.
			case "failed":
				_ = writer.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": fmt.Sprintf("turn-%d", turn), "status": "failed", "error": map[string]string{"message": "usage limit, try again at 20:00"}}}})
			case "missing-answer":
				_ = writer.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"id": fmt.Sprintf("turn-%d", turn), "status": "completed"}}})
			case "approval":
				_ = writer.Encode(map[string]any{"id": 900, "method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "echo hello"}})
			default:
				sendSuccess()
			}
		case "":
			if mode == "approval" && frame.ID == 900 {
				if !strings.Contains(string(frame.Result), `"decline"`) {
					return
				}
				sendSuccess()
			}
		}
	}
}

func warmSpec(t *testing.T, command string) (*Adapter, adapter.TurnSpec) {
	t.Helper()
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"test-access","refresh_token":"","account_id":"test-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: command, Variant: "native", AllowNative: true, RuntimeRoot: filepath.Join(dir, "runtime"), AuthPath: authPath}
	spec := adapter.TurnSpec{TurnID: "turn-one", Agent: adapter.AgentSpec{ID: "agent-one", Tool: "codex", Model: "gpt-6-sol"}, CheckoutPath: dir, Mode: adapter.ModeNormal}
	return a, spec
}

func TestWarmAppServerReusesThreeTurns(t *testing.T) {
	a, spec := warmSpec(t, warmHelperScript(t, "stale"))
	fp, err := a.WarmFingerprint(spec)
	if err != nil || fp == "" {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
	w, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(2 * time.Second)
	for n := 0; n < 3; n++ {
		spec.TurnID = fmt.Sprintf("turn-%d", n)
		if n > 0 {
			spec.NativeSessionStarted = true
			spec.NativeSessionHandle = "thread-fake"
		}
		turn, err := w.Run(spec)
		if err != nil {
			t.Fatal(err)
		}
		var native, answer, usage bool
		for event := range turn.Events() {
			switch event.Kind {
			case adapter.EventNativeSessionStarted:
				native = event.NativeSessionHandle == "thread-fake"
			case adapter.EventFinalAnswer:
				answer = string(event.Answer) == `{"messages":[]}`
			case adapter.EventUsage:
				usage = event.Usage != nil && event.Usage.TokensIn != nil && *event.Usage.TokensIn == 90
			}
		}
		exit, err := turn.Wait()
		if err != nil || !exit.TerminalEvent || exit.ExitCode != 0 || !answer || !usage || (n == 0 && !native) || !w.Alive() {
			t.Fatalf("turn %d: exit=%+v err=%v native=%t answer=%t usage=%t alive=%t", n, exit, err, native, answer, usage, w.Alive())
		}
	}
	if err := w.Close(2 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWarmAppServerHandshakeAndResumeMismatchFallBack(t *testing.T) {
	for _, mode := range []string{"bad-home", "resume-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			a, spec := warmSpec(t, warmHelperScript(t, mode))
			if mode == "resume-mismatch" {
				spec.NativeSessionStarted = true
				spec.NativeSessionHandle = "expected"
			}
			w, err := a.StartWarm(context.Background(), spec)
			if mode == "bad-home" {
				if err == nil || !strings.Contains(err.Error(), "home") {
					t.Fatalf("handshake %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close(2 * time.Second)
			if _, err := w.Run(spec); err == nil || !strings.Contains(err.Error(), "thread ID") {
				t.Fatalf("resume mismatch %v", err)
			}
		})
	}
}

func TestWarmAppServerUsesExplicitColdTurnDefaults(t *testing.T) {
	for _, model := range []string{"gpt-6-sol", "gpt-6.1-sol"} {
		t.Run(model, func(t *testing.T) {
			a, spec := warmSpec(t, warmHelperScript(t, "require-defaults"))
			spec.Agent.Model = model
			w, err := a.StartWarm(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close(2 * time.Second)
			turn, err := w.Run(spec)
			if err != nil {
				t.Fatal(err)
			}
			for range turn.Events() {
			}
			if exit, err := turn.Wait(); err != nil || !exit.TerminalEvent {
				t.Fatalf("explicit defaults: exit=%+v err=%v", exit, err)
			}
		})
	}
}

func TestWarmAppServerFailureEndsProcessAndApprovalIsDeclined(t *testing.T) {
	for _, mode := range []string{"failed", "missing-answer", "approval"} {
		t.Run(mode, func(t *testing.T) {
			a, spec := warmSpec(t, warmHelperScript(t, mode))
			w, err := a.StartWarm(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close(2 * time.Second)
			turn, err := w.Run(spec)
			if err != nil {
				t.Fatal(err)
			}
			for range turn.Events() {
			}
			exit, err := turn.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "approval" {
				if !exit.TerminalEvent || !w.Alive() {
					t.Fatalf("declined approval did not allow controlled completion: %+v", exit)
				}
			} else if exit.TerminalEvent || w.Alive() {
				t.Fatalf("abnormal result kept process: %+v", exit)
			}
			if mode == "failed" && !strings.Contains(exit.ErrorEvent, "usage limit") {
				t.Fatalf("lost error wording: %+v", exit)
			}
		})
	}
}

func TestWarmAppServerBusyAndGone(t *testing.T) {
	a, spec := warmSpec(t, warmHelperScript(t, "hold"))
	w, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := w.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.TurnID = "second"
	if _, err := w.Run(spec); !errors.Is(err, adapter.ErrWarmBusy) {
		t.Fatalf("second turn while first is live: %v", err)
	}
	if err := w.Close(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	if _, err := w.Run(spec); !errors.Is(err, adapter.ErrWarmGone) {
		t.Fatalf("turn after process exit: %v", err)
	}
}

func TestWarmRefusesFullCredentialsBeforeLaunchAndReuse(t *testing.T) {
	a, spec := warmSpec(t, warmHelperScript(t, "success"))
	if _, err := a.WarmFingerprint(spec); err != nil {
		t.Fatal(err)
	}
	full := []byte(`{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"test-access","refresh_token":"test-refresh","account_id":"test-account"}}`)
	if err := os.WriteFile(a.AuthPath, full, 0600); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func() error{
		func() error { _, err := a.WarmFingerprint(spec); return err },
		func() error { _, err := a.StartWarm(context.Background(), spec); return err },
	} {
		err := run()
		if !errors.Is(err, adapter.ErrNoWarm) || !strings.Contains(err.Error(), "Codex warm needs token-only credentials") {
			t.Fatalf("full credentials allowed warm process: %v", err)
		}
	}
}

func TestWarmFingerprintTracksProcessInputsButNotSession(t *testing.T) {
	a, spec := warmSpec(t, warmHelperScript(t, "success"))
	first, err := a.WarmFingerprint(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.NativeSessionStarted, spec.NativeSessionHandle, spec.TurnID = true, "thread-fake", "next-turn"
	resumed, err := a.WarmFingerprint(spec)
	if err != nil || resumed != first {
		t.Fatalf("session changed fingerprint: %q %q %v", first, resumed, err)
	}
	spec.Agent.Model = "gpt-6-luna"
	changed, err := a.WarmFingerprint(spec)
	if err != nil || changed == first {
		t.Fatalf("model did not change fingerprint: %q %q %v", first, changed, err)
	}
	launch, err := a.prepareWarm(spec)
	if err != nil {
		t.Fatal(err)
	}
	launch.config = append(launch.config, '\n')
	if launch.fingerprint() == changed {
		t.Fatal("changed private MCP config did not change fingerprint")
	}
	launch, err = a.prepareWarm(spec)
	if err != nil {
		t.Fatal(err)
	}
	launch.key = "AIMEBU_AGENT_KEY=one"
	launch.env = append(launch.env, launch.key)
	keyOne := launch.fingerprint()
	launch.env[len(launch.env)-1] = "AIMEBU_AGENT_KEY=two"
	launch.key = "AIMEBU_AGENT_KEY=two"
	if launch.fingerprint() == keyOne {
		t.Fatal("agent key did not change fingerprint")
	}
	beforeAuthPath := launch.fingerprint()
	launch.authPath += ".other"
	if launch.fingerprint() == beforeAuthPath {
		t.Fatal("changed auth path did not change fingerprint")
	}
}

func TestWarmResumedThreadDoesNotChargePreviousTurns(t *testing.T) {
	a, spec := warmSpec(t, warmHelperScript(t, "resume-usage"))
	spec.NativeSessionStarted, spec.NativeSessionHandle = true, "thread-fake"
	w, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(2 * time.Second)
	turn, err := w.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	var input int64 = -1
	for event := range turn.Events() {
		if event.Kind == adapter.EventUsage && event.Usage != nil && event.Usage.TokensIn != nil {
			input = *event.Usage.TokensIn
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if input != 90 {
		t.Fatalf("resumed turn charged earlier history: fresh input %d", input)
	}
}

func TestWarmResumedThreadCountsEveryUsageUpdate(t *testing.T) {
	a, spec := warmSpec(t, warmHelperScript(t, "resume-usage-multi"))
	spec.NativeSessionStarted, spec.NativeSessionHandle = true, "thread-fake"
	w, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(2 * time.Second)
	turn, err := w.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	var input, output int64 = -1, -1
	for event := range turn.Events() {
		if event.Kind == adapter.EventUsage && event.Usage != nil {
			if event.Usage.TokensIn != nil {
				input = *event.Usage.TokensIn
			}
			if event.Usage.TokensOut != nil {
				output = *event.Usage.TokensOut
			}
		}
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	if input != 190 || output != 50 {
		t.Fatalf("resumed turn usage from two updates = input %d output %d", input, output)
	}
}

// A warm Codex process in a container whose launcher can say so is asked which container it starts the harness in, and its session knows the
// ID; the launcher's line is not part of what starts the process, so the fingerprint is the same either way.
func TestWarmAppServerKnowsItsContainer(t *testing.T) {
	const container = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	helper := warmHelperScript(t, "stale")
	launcher := filepath.Join(t.TempDir(), "codex-docker")
	body := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_REPORT_EXEC_ID\" = 1 ]; then echo 'HARNESS_DOCKER_EXEC_ID=" + container + ":7-8-9' >&2; fi\nexec " + strconv.Quote(helper) + " \"$@\"\n"
	if err := testenv.WriteExecutable(launcher, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	a, spec := warmSpec(t, launcher)
	spec.LauncherContainerized = true
	plain, err := a.WarmFingerprint(spec)
	if err != nil {
		t.Fatal(err)
	}
	w, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close(2 * time.Second)
	if id, never := w.(adapter.ContainerSession).Container(); id != "" || never {
		t.Fatalf("a launcher that was not asked: id %q never started %t", id, never)
	}
	spec.LauncherReportsContainer = true
	if asked, err := a.WarmFingerprint(spec); err != nil || asked != plain {
		t.Fatalf("asking the launcher for its container changed the fingerprint: %v", err)
	}
	w, err = a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(2 * time.Second)
	// The line comes before the handshake the start waited for.
	if id, never := w.(adapter.ContainerSession).Container(); id != container || never {
		t.Fatalf("id %q never started %t", id, never)
	}
}
