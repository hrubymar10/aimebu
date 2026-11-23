package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// fakeClaude is a harness that reads one stream-json user message per line and answers each the way claude does: it repeats the line (as
// --replay-user-messages makes the CLI do), then an init, an assistant step and a result. `body` is what it does with the n-th message after
// the repeat (shell, with $n set).
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	return fakeClaudeScript(t, "printf '%s\\n' \"$line\"\n"+body)
}

// fakeClaudeScript is fakeClaude without the repeat of the input: body alone is what it does with the n-th message.
func fakeClaudeScript(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	script := "#!/bin/sh\nn=0\nwhile IFS= read -r line; do\nn=$((n+1))\n" + body + "done\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

const okTurn = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1"}' '{"type":"assistant","message":{"id":"m'$n'","usage":{"input_tokens":5}}}' '{"type":"result","subtype":"success","is_error":false,"result":"{\"messages\":[]}","session_id":"s1","usage":{"input_tokens":5,"output_tokens":3},"modelUsage":{"claude-haiku":{"costUSD":0.001,"contextWindow":200000}}}'
`

func warmSpec(t *testing.T, id string) adapter.TurnSpec {
	t.Helper()
	root := t.TempDir()
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	return adapter.TurnSpec{TurnID: id, Agent: adapter.AgentSpec{ID: "agt_test", Tool: "claude", Variant: "native", Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeNormal, AgentTools: config, NativeSessionHandle: "s1", NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
}

func drain(t *testing.T, turn adapter.Turn) (events []adapter.Event, exit adapter.ExitInfo) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for event := range turn.Events() {
			events = append(events, event)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the turn did not end")
	}
	exit, err := turn.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return events, exit
}

func kinds(events []adapter.Event) []adapter.EventKind {
	var out []adapter.EventKind
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}

// Two turns run in one process: each ends at its own result with a terminal exit of code 0, the process stays up between them and after the
// second, and closing it ends it.
func TestWarmSessionTakesTurnsInOneProcess(t *testing.T) {
	a := &Adapter{Command: fakeClaude(t, okTurn), Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Kill()
	pid := session.Pid()
	for i, id := range []string{"t1", "t2"} {
		spec.TurnID = id
		spec.NativeSessionStarted = i > 0
		turn, err := session.Run(spec)
		if err != nil {
			t.Fatal(err)
		}
		events, exit := drain(t, turn)
		if !exit.TerminalEvent || exit.ExitCode != 0 || exit.CancelIntent {
			t.Fatalf("turn %s exit = %+v", id, exit)
		}
		got := kinds(events)
		if got[len(got)-1] != adapter.EventProcessExit || got[len(got)-2] != adapter.EventFinalAnswer {
			t.Fatalf("turn %s events %v: the answer, then the exit", id, got)
		}
		hasUsage := false
		for _, event := range events {
			hasUsage = hasUsage || (event.Kind == adapter.EventUsage && event.Usage != nil && event.Usage.CostCumulative)
			if event.Kind == adapter.EventProcessExit && !strings.HasPrefix(event.SourceEventID, id+":") {
				t.Fatalf("event ids are per turn: %q", event.SourceEventID)
			}
		}
		if !hasUsage {
			t.Fatalf("turn %s reported no usage: %v", id, got)
		}
		if !session.Alive() || session.Pid() != pid {
			t.Fatalf("the process must stay up after turn %s", id)
		}
	}
	if err := session.Close(5 * time.Second); err != nil || session.Alive() {
		t.Fatalf("close: %v alive=%v", err, session.Alive())
	}
	if _, err := session.Run(spec); err != adapter.ErrWarmGone {
		t.Fatalf("a closed session takes no turn: %v", err)
	}
}

// A turn cannot start while another is running in the session.
func TestWarmSessionTakesOneTurnAtATime(t *testing.T) {
	a := &Adapter{Command: fakeClaude(t, "sleep 1\n"+okTurn), Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Kill()
	first, err := session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.TurnID = "t2"
	if _, err := session.Run(spec); err != adapter.ErrWarmBusy {
		t.Fatalf("second turn: %v", err)
	}
	drain(t, first)
}

// An error result ends the process with the turn, and the turn's exit is not terminal and carries the error, as a one-process turn's would.
func TestWarmSessionErrorResultEndsTheProcess(t *testing.T) {
	body := `printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"it broke","session_id":"s1","usage":{"input_tokens":0,"output_tokens":0}}'
exit 1
`
	a := &Adapter{Command: fakeClaude(t, body), Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	_, exit := drain(t, turn)
	if exit.TerminalEvent || exit.ExitCode != 1 || exit.ErrorSummary != "it broke" || !exit.DefiniteFailure {
		t.Fatalf("exit = %+v", exit)
	}
	select {
	case <-session.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the process must be gone after an error result")
	}
	if _, err := session.Run(spec); err != adapter.ErrWarmGone {
		t.Fatalf("run after the end: %v", err)
	}
}

// A process that does not end by itself after an error result is killed.
func TestWarmSessionErrorResultThatLingersIsKilled(t *testing.T) {
	body := `printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"x","session_id":"s1"}'
sleep 60
`
	a := &Adapter{Command: fakeClaude(t, body), Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, exit := drain(t, turn)
	if exit.TerminalEvent || time.Since(started) > 10*time.Second {
		t.Fatalf("exit = %+v after %s", exit, time.Since(started))
	}
}

// A process that dies inside a turn (a crash) ends the turn without a terminal event, and what it spent is reported as partial.
func TestWarmSessionCrashInATurn(t *testing.T) {
	body := `printf '%s\n' '{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":7}}}'
exit 3
`
	a := &Adapter{Command: fakeClaude(t, body), Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	events, exit := drain(t, turn)
	partial, background := false, false
	for _, event := range events {
		if event.Kind == adapter.EventUsage && event.Usage != nil && event.Usage.Partial {
			partial, background = true, event.Usage.Background
		}
	}
	// The CLI had repeated the turn's input, so what it spent is the turn's own, not background.
	if exit.TerminalEvent || exit.ExitCode != 3 || !partial || background {
		t.Fatalf("exit = %+v partial=%v background=%v events=%v", exit, partial, background, kinds(events))
	}
}

// A live process that dies, or a turn cancelled, before the CLI repeats the turn's input: what was spent belonged to a query the CLI ran on
// its own (a background task's), so the partial usage is reported as background and confirms no context delivery.
func TestWarmTurnEndedBeforeItsInputReportsBackgroundUsage(t *testing.T) {
	body := `printf '%s\n' '{"type":"assistant","message":{"id":"bg1","usage":{"input_tokens":9}}}'
exit 3
`
	_, events, exit := runWarmFixture(t, body, "t1")
	var spent []*adapter.Usage
	for _, event := range events {
		if event.Kind == adapter.EventUsage {
			spent = append(spent, event.Usage)
		}
	}
	if exit.TerminalEvent || len(spent) != 1 || !spent[0].Partial || !spent[0].Background || spent[0].TokensIn == nil || *spent[0].TokensIn != 9 {
		t.Fatalf("exit = %+v usage %+v events=%v", exit, spent, kinds(events))
	}
}

// Cancelling a turn ends the process (the turn is cancelled by killing the group) and records the intent; the session is then gone.
func TestWarmSessionCancel(t *testing.T) {
	a := &Adapter{Command: fakeClaude(t, "sleep 60\n"), Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := turn.Cancel(); err != nil {
		t.Fatal(err)
	}
	_, exit := drain(t, turn)
	if !exit.CancelIntent || exit.TerminalEvent {
		t.Fatalf("exit = %+v", exit)
	}
	if session.Alive() {
		t.Fatal("a cancelled turn ends the process")
	}
}

// The fingerprint is stable for the same start, ignores which session is continued and the turn, changes with anything the process is
// started with, and never contains the agent's key.
func TestWarmFingerprint(t *testing.T) {
	a := &Adapter{Command: "claude", Variant: "native", AllowNative: true}
	base := warmSpec(t, "t1")
	fp := func(spec adapter.TurnSpec) string {
		t.Helper()
		got, err := a.WarmFingerprint(spec)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := fp(base)
	same := base
	same.TurnID, same.NativeSessionStarted, same.NativeSessionHandle = "t9", true, "another-session"
	same.NotesJSON = json.RawMessage(`[{"note":1}]`)
	if got := fp(same); got != want {
		t.Fatal("the turn and the session to continue are not part of what the process is started with")
	}
	for name, change := range map[string]func(*adapter.TurnSpec){
		"model":    func(s *adapter.TurnSpec) { s.Agent.Model = "opus" },
		"key":      func(s *adapter.TurnSpec) { c := *s.AgentTools; c.Key = "aak_rotated"; s.AgentTools = &c },
		"role":     func(s *adapter.TurnSpec) { s.ContextJSON = json.RawMessage(`{"role_body":"you are different now"}`) },
		"tools":    func(s *adapter.TurnSpec) { s.AllowedTools = []string{"Read"} },
		"launcher": func(s *adapter.TurnSpec) { s.LauncherPath = "/opt/other-claude" },
		"folder":   func(s *adapter.TurnSpec) { s.CheckoutPath = t.TempDir() },
	} {
		changed := base
		change(&changed)
		if fp(changed) == want {
			t.Errorf("a different %s must be a different fingerprint", name)
		}
	}
	for _, mode := range []adapter.Mode{adapter.ModeCorrection, adapter.ModeDiagnostic} {
		spec := base
		spec.Mode = mode
		if _, err := a.WarmFingerprint(spec); err != adapter.ErrNoWarm {
			t.Errorf("mode %s: %v", mode, err)
		}
	}
	fork := base
	fork.ForkSession, fork.NativeSessionStarted = true, true
	if _, err := a.WarmFingerprint(fork); err != adapter.ErrNoWarm {
		t.Errorf("fork: %v", err)
	}
	if strings.Contains(want, "aak_secret") || strings.Contains(want, "secret") {
		t.Fatal("the key is in the fingerprint")
	}
}

// A warm process is started with --input-format stream-json and the same arguments a one-turn process gets otherwise; the agent's key is in
// its environment and in no argument.
func TestWarmProcessIsStartedForStreamInput(t *testing.T) {
	root := t.TempDir()
	argsPath, envPath, ready := filepath.Join(root, "args"), filepath.Join(root, "env"), filepath.Join(root, "ready")
	path := filepath.Join(root, "claude")
	// The script says it has written both files by creating a third after them; the test reads them only then (a fixed wait raced a slow start).
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nprintf '%s\\n' \"$AIMEBU_AGENT_KEY\" > " + envPath + "\n: > " + ready + "\ncat >/dev/null\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, ready)
	_ = session.Close(2 * time.Second)
	args, _ := os.ReadFile(argsPath)
	env, _ := os.ReadFile(envPath)
	for _, want := range []string{"--input-format\nstream-json\n--replay-user-messages\n", "--output-format\nstream-json", "--session-id\ns1", "--mcp-config\n", "--permission-mode\nbypassPermissions"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("missing %q in:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "aak_secret") || strings.TrimSpace(string(env)) != "aak_secret" {
		t.Fatalf("the key must be in the environment only: args=%q env=%q", args, env)
	}
}

// waitReady waits for the file a test launcher script creates once its trap is in place (the script puts ": > @READY@" after the trap), so a signal is
// never sent before the trap and the process never dies of it instead of exiting the way it traps. A fixed wait raced the shell's start: on a slow start
// the signal arrived first.
func waitReady(t *testing.T, ready string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the launcher never said its trap is in place")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A containerized wrapper's end is confirmed only by a clean exit after its input closed (0), or by exactly 143 after a terminate (a launcher with
// stop_on_signal), never by time and never by a kill or another status; a kill stays unconfirmed however long ago it was. A native session is
// confirmed by being reaped.
func TestWarmContainerEndIsConfirmedByTheWrapperNotByTime(t *testing.T) {
	oldGrace := containerTermGrace
	defer func() { containerTermGrace = oldGrace }()
	cases := []struct {
		name, script, variant string
		confirmed             bool
	}{
		{"docker: exits 0 when its input closes", "#!/bin/sh\ncat >/dev/null\nexit 0\n", "docker", true},
		{"docker: exits 143 on terminate (stop_on_signal)", "#!/bin/sh\ntrap 'exit 143' TERM\n: > '@READY@'\nwhile true; do sleep 0.1; done\n", "docker", true},
		{"docker: exits 75 on terminate", "#!/bin/sh\ntrap 'exit 75' TERM\n: > '@READY@'\nwhile true; do sleep 0.1; done\n", "docker", false},
		{"docker: dies of an error when its input closes", "#!/bin/sh\ncat >/dev/null\nexit 1\n", "docker", false},
		{"docker: ignores everything and is killed", "#!/bin/sh\ntrap '' TERM\n: > '@READY@'\nwhile true; do sleep 1; done\n", "docker", false},
		{"native: ignores everything and is killed", "#!/bin/sh\ntrap '' TERM\n: > '@READY@'\nwhile true; do sleep 1; done\n", "native", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "claude")
			ready := filepath.Join(root, "ready")
			if err := testenv.WriteExecutable(path, []byte(strings.ReplaceAll(tc.script, "@READY@", ready)), 0700); err != nil {
				t.Fatal(err)
			}
			a := &Adapter{Command: path, Variant: tc.variant, AllowNative: true}
			spec := warmSpec(t, "t1")
			spec.Agent.Variant = tc.variant
			session, err := a.StartWarm(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(tc.script, "@READY@") {
				waitReady(t, ready) // the traps are set
			}
			// A wrapper whose trap exits on the terminate gets a generous grace (it ends at once; the bound only covers a slow start); one that
			// ignores it is killed after a short one.
			grace := 200 * time.Millisecond
			if strings.Contains(tc.script, "trap 'exit") {
				grace = 10 * time.Second
			}
			containerTermGrace = grace
			err = session.Close(grace)
			if tc.confirmed && err != nil {
				t.Fatalf("expected a confirmed end, got %v", err)
			}
			if !tc.confirmed && !errors.Is(err, ErrContainerSideUnconfirmed) {
				t.Fatalf("expected an unconfirmed end, got %v", err)
			}
			if session.Alive() {
				t.Fatal("the host side must be gone either way")
			}
			if !tc.confirmed {
				time.Sleep(300 * time.Millisecond)
				if err := session.Kill(); !errors.Is(err, ErrContainerSideUnconfirmed) {
					t.Fatalf("time does not confirm it: %v", err)
				}
			}
		})
	}
}

// A keep-alive turn differs from a real one only in its user turn: the instruction is there (and not in the system prompt, which is the cached prefix),
// the process arguments are the same, and so is the warm fingerprint: it must read the cache the real turns wrote, not write another.
func TestKeepAliveTurnChangesOnlyTheUserTurn(t *testing.T) {
	a := &Adapter{Command: "claude", Variant: "native", AllowNative: true}
	real := warmSpec(t, "t1")
	ping := real
	ping.KeepAlive = true
	ping.NotesJSON = json.RawMessage(`[]`)
	realInput, _ := turnInput(real)
	pingInput, _ := turnInput(ping)
	if strings.Contains(string(realInput), "keep-alive") || !strings.Contains(string(pingInput), "cache keep-alive, not work") {
		t.Fatalf("real=%s ping=%s", realInput, pingInput)
	}
	if systemPromptFor(real) != systemPromptFor(ping) {
		t.Fatal("the instruction must not be in the system prompt: it is part of the cached prefix")
	}
	lReal, err := a.prepare(real, false)
	if err != nil {
		t.Fatal(err)
	}
	lPing, err := a.prepare(ping, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lReal.args, "\x00") != strings.Join(lPing.args, "\x00") || strings.Join(lReal.env, "\x00") != strings.Join(lPing.env, "\x00") {
		t.Fatal("a ping must run with exactly the arguments and environment of a real turn")
	}
	fpReal, _ := a.WarmFingerprint(real)
	fpPing, _ := a.WarmFingerprint(ping)
	if fpReal == "" || fpReal != fpPing {
		t.Fatal("a ping must have the real turn's fingerprint")
	}
	if a.KeepAliveTool() != "claude" {
		t.Fatal("the claude adapter runs keep-alive turns for claude")
	}
	var _ adapter.KeepAliver = a
}

// Terminate asks a one-process turn to end with SIGTERM and kills it only if it does not: a launcher that traps the signal exits 143 on its own, one
// that ignores it is killed after the grace.
func TestTurnTerminateEndsOnTheSignalOrIsKilledAfterTheGrace(t *testing.T) {
	for name, tc := range map[string]struct {
		script   string
		byItself bool
		code     int
	}{
		"traps TERM": {"#!/bin/sh\ntrap 'exit 143' TERM\n: > '@READY@'\nwhile true; do sleep 0.1; done\n", true, 143},
		"ignores it": {"#!/bin/sh\ntrap '' TERM\n: > '@READY@'\nwhile true; do sleep 0.2; done\n", false, -1},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "claude")
			ready := filepath.Join(root, "ready")
			if err := testenv.WriteExecutable(path, []byte(strings.ReplaceAll(tc.script, "@READY@", ready)), 0700); err != nil {
				t.Fatal(err)
			}
			a := &Adapter{Command: path, Variant: "native", AllowNative: true}
			spec := warmSpec(t, "t1")
			turn, err := a.Start(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			waitReady(t, ready)
			terminator, ok := turn.(interface{ Terminate(time.Duration) bool })
			if !ok {
				t.Fatal("a turn can be terminated")
			}
			grace := 300 * time.Millisecond
			if tc.byItself {
				grace = 10 * time.Second // the trap ends it at once; the bound only covers a slow start
			}
			if got := terminator.Terminate(grace); got != tc.byItself {
				t.Fatalf("ended on the terminate = %v, want %v", got, tc.byItself)
			}
			_, exit := drain(t, turn)
			if !exit.CancelIntent || exit.TerminalEvent {
				t.Fatalf("exit = %+v", exit)
			}
			if tc.byItself && exit.ExitCode != tc.code {
				t.Fatalf("exit code = %d, want %d", exit.ExitCode, tc.code)
			}
		})
	}
}

// A marker turn (the context's role_body shrunk to the unchanged marker) has the same warm fingerprint as the full turn before it: the
// role body rides in the system prompt argument from role_body_system on every turn, so the warm process is not retired and the cached
// prefix is not rewritten.
func TestMarkerTurnKeepsTheWarmFingerprint(t *testing.T) {
	a := &Adapter{Command: "claude", Variant: "native", AllowNative: true}
	full := warmSpec(t, "t1")
	full.ContextJSON = json.RawMessage(`{"role_body":"You are the worker."}`)
	marker := full
	marker.ContextJSON = json.RawMessage(`{"role_body":{"unchanged":true,"rev":"ab12cd34"},"role_body_system":"You are the worker."}`)
	fpFull, err := a.WarmFingerprint(full)
	if err != nil {
		t.Fatal(err)
	}
	fpMarker, err := a.WarmFingerprint(marker)
	if err != nil {
		t.Fatal(err)
	}
	if fpFull != fpMarker {
		t.Fatal("a marker turn changed the warm fingerprint; the warm process would be retired and the cache rewritten")
	}
	// A changed role body is still a different fingerprint: the system prompt changes and the cache is rewritten once.
	changed := marker
	changed.ContextJSON = json.RawMessage(`{"role_body":"You are the worker.","role_body_system":"Different role now."}`)
	if fp, err := a.WarmFingerprint(changed); err != nil || fp == fpMarker {
		t.Fatalf("a changed role body must change the fingerprint: %v %v", fp, err)
	}
}

// A warm process whose launcher can say so is asked which container it starts the harness in, and its session knows the ID from then on. The
// request is the process's own: it is not part of the fingerprint, and the launcher's line stays out of the harness's stderr.
func TestWarmSessionKnowsItsContainer(t *testing.T) {
	const container = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	root := t.TempDir()
	path := filepath.Join(root, "claude-docker")
	script := "#!/bin/sh\n" +
		"if [ \"$HARNESS_DOCKER_REPORT_EXEC_ID\" = 1 ]; then echo 'HARNESS_DOCKER_EXEC_ID=" + container + ":7-8-9' >&2; fi\n" +
		"echo 'harness stderr' >&2\ncat >/dev/null\nexit 0\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "docker"}
	spec := warmSpec(t, "t1")
	spec.Agent.Variant = "docker"
	plain, err := a.WarmFingerprint(spec)
	if err != nil {
		t.Fatal(err)
	}
	// Not asked: the session knows nothing, and says nothing about a launcher that never started.
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if id, never := session.(adapter.ContainerSession).Container(); id != "" || never {
		t.Fatalf("a launcher that was not asked: id %q never started %t", id, never)
	}
	spec.LauncherReportsContainer = true
	if asked, err := a.WarmFingerprint(spec); err != nil || asked != plain {
		t.Fatalf("asking the launcher for its container changed the fingerprint: %v", err)
	}
	var stderr lockedBuffer
	spec.StderrSink = &stderr
	session, err = a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if id, _ := session.(adapter.ContainerSession).Container(); id == container {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never learned its container")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := session.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if id, never := session.(adapter.ContainerSession).Container(); id != container || never {
		t.Fatalf("after its end: id %q never started %t", id, never)
	}
	if got := stderr.String(); strings.Contains(got, "HARNESS_DOCKER_EXEC_ID") || !strings.Contains(got, "harness stderr") {
		t.Fatalf("stderr kept %q", got)
	}
}

// A launcher that was asked for its container and ended without naming one never reached its `docker exec`: the session says so once the
// process is gone, which is how a launcher that found its container being removed is told from one that left a process behind.
func TestWarmSessionKnowsALauncherThatStartedNothing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude-docker")
	script := "#!/bin/sh\necho 'Error: Harness container is removing. Run: bin/harness-docker-ctrl start' >&2\nexit 1\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "docker"}
	spec := warmSpec(t, "t1")
	spec.Agent.Variant = "docker"
	spec.LauncherReportsContainer = true
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("the launcher did not end")
	}
	if id, never := session.(adapter.ContainerSession).Container(); id != "" || !never {
		t.Fatalf("id %q never started %t", id, never)
	}
	// Its launcher confirmed nothing, as before: it is the supervisor that reads the identity.
	if err := session.Kill(); !errors.Is(err, ErrContainerSideUnconfirmed) {
		t.Fatalf("the end of a launcher that exited 1: %v", err)
	}
}
