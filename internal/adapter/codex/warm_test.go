package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/testenv"
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
		case "thread/start", "thread/resume":
			id := "thread-fake"
			if mode == "resume-mismatch" && frame.Method == "thread/resume" {
				id = "different"
			}
			_ = writer.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"thread": map[string]string{"id": id}}})
		case "turn/start":
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
	launch.key = "AIMEBU2_AGENT_KEY=one"
	launch.env = append(launch.env, launch.key)
	keyOne := launch.fingerprint()
	launch.env[len(launch.env)-1] = "AIMEBU2_AGENT_KEY=two"
	launch.key = "AIMEBU2_AGENT_KEY=two"
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
