package claude

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

// The fixtures in testdata/stream are Claude Code 2.1.329's own output, recorded against a local stand-in for the Messages API (an isolated
// config folder and a placeholder key) and trimmed to the fields the adapter reads; session ids are "s1" and turn ids are named for the case.
//
//   - warm_resume_after_stopped_task: a live process resumed a session whose background task outlived the previous process. Before it reads
//     the turn's input, the CLI reports the task as stopped and ends that query with an empty result (origin "task-notification"); then it
//     repeats the turn's input and answers it.
//   - warm_task_query_before_input: a background task ended while the process was idle; the CLI's own query about it (assistant events and
//     a non-empty result "ok") is still running when the turn's input arrives, and the input is read after it.
//   - warm_normal, warm_error: an ordinary turn, and one the API refused (an error result, after which the CLI exits 1).
//   - cold_resume_after_stopped_task, cold_task_ends_after_answer: one-turn processes (input on stdin, not stream-json). The first starts
//     with the same empty result; in the second, a background task ends after the answer and the CLI's query about it ends with "ok".
func fixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "stream", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// resultDetails are the journal details of the turn's result events, in order: an answering result's ride on its usage event, an ignored
// result's on an activity event of its own.
func resultDetails(events []adapter.Event) []string {
	var out []string
	for _, event := range events {
		if event.Kind == adapter.EventActivity && event.Summary == "result" || event.Kind == adapter.EventUsage && strings.HasPrefix(event.Detail, "result ") {
			out = append(out, event.Detail)
		}
	}
	return out
}

func answerOf(events []adapter.Event) (string, int) {
	answer, count := "", 0
	for _, event := range events {
		if event.Kind == adapter.EventFinalAnswer {
			answer, count = string(event.Answer), count+1
		}
	}
	return answer, count
}

func usageCount(events []adapter.Event) int {
	n := 0
	for _, event := range events {
		if event.Kind == adapter.EventUsage {
			n++
		}
	}
	return n
}

func runWarmFixture(t *testing.T, script, turnID string) (adapter.WarmSession, []adapter.Event, adapter.ExitInfo) {
	t.Helper()
	a := &Adapter{Command: fakeClaudeScript(t, script), Variant: "native", AllowNative: true}
	spec := warmSpec(t, turnID)
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Kill() })
	turn, err := session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	events, exit := drain(t, turn)
	return session, events, exit
}

// The cause of the `invalid_answer: EOF` turns: a live process resumed after a background task outlived the previous one answers the CLI's
// own query about that task (an empty result) before it reads the turn's input. That result comes before the repeat of the input, so it is
// journaled and ignored, and the turn ends at the result that follows the repeat, with the real answer, in a process that stays up.
func TestWarmTurnIgnoresTheResultBeforeItsInputIsRead(t *testing.T) {
	session, events, exit := runWarmFixture(t, "cat '"+fixture(t, "warm_resume_after_stopped_task")+"'\n", "turn_resumed")
	answer, answers := answerOf(events)
	if answers != 1 || answer != `{"messages":[]}` || !exit.TerminalEvent || exit.ExitCode != 0 {
		t.Fatalf("answer %q (%d), exit %+v", answer, answers, exit)
	}
	details := resultDetails(events)
	if len(details) != 2 {
		t.Fatalf("result details %q", details)
	}
	for _, want := range []string{"subtype=success", "num_turns=0", "result_len=0", "structured_output=false", "session_id=s1", "origin=task-notification", "assistant_events=0", "ignored: it came before the CLI read this turn's input"} {
		if !strings.Contains(details[0], want) {
			t.Errorf("phantom detail %q lacks %q", details[0], want)
		}
	}
	for _, want := range []string{"num_turns=2", "result_len=15", "structured_output=true", "assistant_events=1"} {
		if !strings.Contains(details[1], want) {
			t.Errorf("answer detail %q lacks %q", details[1], want)
		}
	}
	if strings.Contains(details[1], "ignored") {
		t.Errorf("the real result was ignored: %q", details[1])
	}
	if usageCount(events) != 1 {
		t.Errorf("an empty result that spent nothing reports no usage: %d usage events", usageCount(events))
	}
	if !session.Alive() {
		t.Fatal("an ignored result does not end the process")
	}
}

// A background task that ended while the process was idle makes the CLI run a query of its own, with assistant events and a non-empty
// result. The turn's input is read after it, so that result is not the turn's answer however real it looks.
func TestWarmTurnIgnoresTheCLIsOwnQueryBeforeItsInput(t *testing.T) {
	_, events, exit := runWarmFixture(t, "cat '"+fixture(t, "warm_task_query_before_input")+"'\n", "turn_after_task")
	answer, answers := answerOf(events)
	if answers != 1 || answer != `{"messages":[]}` || !exit.TerminalEvent {
		t.Fatalf("answer %q (%d), exit %+v", answer, answers, exit)
	}
	details := resultDetails(events)
	if len(details) != 2 || !strings.Contains(details[0], "result_len=2") || !strings.Contains(details[0], "assistant_events=2") ||
		!strings.Contains(details[0], "ignored") || !strings.Contains(details[1], "assistant_events=1") || strings.Contains(details[1], "ignored") {
		t.Fatalf("result details %q", details)
	}
	// What the CLI's own query spent is reported too, as a usage event of its own before the answer's.
	var spent []*adapter.Usage
	for _, event := range events {
		if event.Kind == adapter.EventUsage {
			spent = append(spent, event.Usage)
		}
	}
	if len(spent) != 2 || spent[0].TokensOut == nil || *spent[0].TokensOut == 0 || spent[0].CostEstimate == nil || !spent[0].CostCumulative ||
		spent[1].CostEstimate == nil || *spent[1].CostEstimate <= *spent[0].CostEstimate {
		t.Fatalf("usage %+v", spent)
	}
}

// Only the origin the CLI is known to give its own queries is ignored: a result after the repeat with any other origin is the turn's answer
// (a later CLI may tag ordinary answers), and its origin is journaled.
func TestWarmTurnTakesAResultWithAnUnknownOrigin(t *testing.T) {
	result := `printf '%s\n' '{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":5}}}' '{"type":"result","subtype":"success","is_error":false,"result":"{\"messages\":[]}","origin":{"kind":"user-prompt"},"session_id":"s1","usage":{"input_tokens":5,"output_tokens":3}}'
`
	session, events, exit := runWarmFixture(t, "printf '%s\\n' \"$line\"\n"+result, "t1")
	if answer, answers := answerOf(events); answers != 1 || answer != `{"messages":[]}` || !exit.TerminalEvent || !session.Alive() {
		t.Fatalf("answer %q (%d), exit %+v", answer, answers, exit)
	}
	if details := resultDetails(events); len(details) != 1 || !strings.Contains(details[0], "origin=user-prompt") || strings.Contains(details[0], "ignored") {
		t.Fatalf("result details %q", details)
	}
}

// An ordinary turn: its result follows the repeat of its input and is its answer.
func TestWarmTurnTakesTheResultAfterItsInput(t *testing.T) {
	session, events, exit := runWarmFixture(t, "cat '"+fixture(t, "warm_normal")+"'\n", "turn_normal")
	answer, answers := answerOf(events)
	if answers != 1 || answer != `{"messages":[]}` || !exit.TerminalEvent || exit.ExitCode != 0 || !session.Alive() {
		t.Fatalf("answer %q (%d), exit %+v, alive %v", answer, answers, exit, session.Alive())
	}
	if details := resultDetails(events); len(details) != 1 || strings.Contains(details[0], "ignored") || !strings.Contains(details[0], "structured_output=true") {
		t.Fatalf("result details %q", details)
	}
}

// A genuine error result still ends the turn and the process, with the CLI's words as the error.
func TestWarmTurnErrorResultStillEndsIt(t *testing.T) {
	session, events, exit := runWarmFixture(t, "cat '"+fixture(t, "warm_error")+"'\nexit 1\n", "turn_error")
	if _, answers := answerOf(events); answers != 0 || exit.TerminalEvent || exit.ExitCode != 1 ||
		!strings.HasPrefix(exit.ErrorSummary, "Prompt is too long") || !exit.DefiniteFailure {
		t.Fatalf("exit %+v", exit)
	}
	if details := resultDetails(events); len(details) != 1 || !strings.Contains(details[0], "is_error=true") || strings.Contains(details[0], "ignored") {
		t.Fatalf("result details %q", details)
	}
	select {
	case <-session.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the process must be gone after an error result")
	}
}

// After the repeat, a result that is empty, spent nothing and followed no assistant event is not an answer either (a CLI that runs its
// own query in that place): it neither ends the turn nor starts the kill that follows a result without an answer.
func TestWarmTurnIgnoresAnEmptyResultAfterItsInput(t *testing.T) {
	empty := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":0,"result":"","session_id":"s1","usage":{"input_tokens":0,"output_tokens":0}}'
sleep 3
`
	session, events, exit := runWarmFixture(t, "printf '%s\\n' \"$line\"\n"+empty+okTurn, "t1")
	answer, answers := answerOf(events)
	if answers != 1 || answer != `{"messages":[]}` || !exit.TerminalEvent || !session.Alive() {
		t.Fatalf("answer %q (%d), exit %+v: the process outlived the 2 s kill and answered", answer, answers, exit)
	}
	if details := resultDetails(events); len(details) != 2 || !strings.Contains(details[0], "ignored: it is empty") {
		t.Fatalf("result details %q", details)
	}
}

// A result that comes without the input ever being repeated keeps the turn open: no answer, no kill.
func TestWarmTurnWithoutTheRepeatGetsNoAnswer(t *testing.T) {
	a := &Adapter{Command: fakeClaudeScript(t, `printf '%s\n' '{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":5}}}' '{"type":"result","subtype":"success","is_error":false,"result":"{\"messages\":[]}","session_id":"s1","usage":{"input_tokens":5,"output_tokens":3}}'
`), Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Kill()
	turn, err := session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	var seen []adapter.Event
	deadline := time.After(2500 * time.Millisecond)
	for waiting := true; waiting; {
		select {
		case event, ok := <-turn.Events():
			if !ok {
				t.Fatalf("the turn ended: %v", kinds(seen))
			}
			seen = append(seen, event)
		case <-deadline:
			waiting = false
		}
	}
	if _, answers := answerOf(seen); answers != 0 || !session.Alive() {
		t.Fatalf("events %v, alive %v", kinds(seen), session.Alive())
	}
	if details := resultDetails(seen); len(details) != 1 || !strings.Contains(details[0], "ignored: it came before") {
		t.Fatalf("result details %q", details)
	}
	_ = turn.Cancel()
	drain(t, turn)
}

// What the process writes between turns is not dropped silently: the next turn journals its event types and the start of the first lines.
func TestWarmOutputBetweenTurnsIsJournaledWithTheNextTurn(t *testing.T) {
	stray := `if [ "$n" = 1 ]; then printf '%s\n' '{"type":"assistant","message":{"id":"late","content":[{"type":"text","text":"a late answer"}]}}' '{"type":"result","subtype":"success","is_error":false,"result":"late"}' 'not json'; fi
`
	a := &Adapter{Command: fakeClaude(t, okTurn+stray), Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	session, err := a.StartWarm(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Kill()
	turn, err := session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	w := session.(*warmSession)
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		count := w.stray.count
		w.mu.Unlock()
		if count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stray lines seen: %d", count)
		}
		time.Sleep(10 * time.Millisecond)
	}
	spec.TurnID, spec.NativeSessionStarted = "t2", true
	turn, err = session.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	events, exit := drain(t, turn)
	if !exit.TerminalEvent || len(events) == 0 || events[0].Summary != "between_turns" {
		t.Fatalf("exit %+v, events %v", exit, events)
	}
	detail := events[0].Detail
	for _, want := range []string{"3 events arrived between turns: assistant,result/success,unparsed", "a late answer", "not json"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
	turn3 := spec
	turn3.TurnID = "t3"
	turn, err = session.Run(turn3)
	if err != nil {
		t.Fatal(err)
	}
	if events, _ := drain(t, turn); events[0].Summary == "between_turns" {
		t.Fatal("stray output is reported once")
	}
}

func runColdFixture(t *testing.T, name string) ([]adapter.Event, adapter.ExitInfo) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+fixture(t, name)+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "native", AllowNative: true}
	turn, err := a.Start(context.Background(), warmSpec(t, "t1"))
	if err != nil {
		t.Fatal(err)
	}
	return drain(t, turn)
}

// The one-turn path keeps the last result it reads. The empty result of a resumed session's stopped task comes first and is ignored; a
// background task that ends after the answer makes the CLI run its own query, and that later result ("ok") must not replace the answer.
func TestOneTurnProcessIgnoresTheCLIsOwnQueries(t *testing.T) {
	// The empty result spent nothing and reports no usage; the later query spent something and reports it.
	for name, usages := range map[string]int{"cold_resume_after_stopped_task": 1, "cold_task_ends_after_answer": 2} {
		t.Run(name, func(t *testing.T) {
			events, exit := runColdFixture(t, name)
			answer, answers := answerOf(events)
			if answers != 1 || answer != `{"messages":[]}` || !exit.TerminalEvent {
				t.Fatalf("answer %q (%d), exit %+v", answer, answers, exit)
			}
			details := resultDetails(events)
			ignored := 0
			for _, detail := range details {
				if strings.Contains(detail, "ignored") {
					ignored++
					if !strings.Contains(detail, "origin=task-notification") {
						t.Errorf("ignored result %q", detail)
					}
				}
			}
			if len(details) != 2 || ignored != 1 || usageCount(events) != usages {
				t.Fatalf("result details %q, %d usage events", details, usageCount(events))
			}
		})
	}
}

// The repeat of the input needs stream-json input, so only a live process asks for it.
func TestOnlyALiveProcessAsksForItsInputRepeated(t *testing.T) {
	a := &Adapter{Command: "claude", Variant: "native", AllowNative: true}
	spec := warmSpec(t, "t1")
	cold, err := a.prepare(spec, false)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := a.prepare(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cold.args, " "), "--replay-user-messages") || !strings.Contains(strings.Join(warm.args, " "), "--input-format stream-json --replay-user-messages") {
		t.Fatalf("cold %q\nwarm %q", cold.args, warm.args)
	}
}
