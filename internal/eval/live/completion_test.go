package live

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
)

func TestStandinServerURLAcceptsIPv6VisibleHost(t *testing.T) {
	for _, host := range []string{"::1", "[::1]"} {
		got := standinServerURL(host, 9897)
		parsed, err := url.Parse(got)
		if err != nil || parsed.Hostname() != "::1" || parsed.Port() != "9897" {
			t.Fatalf("visible host %q: URL %q parsed as %+v, %v", host, got, parsed, err)
		}
	}
}

// callTool makes one tools/call to the stand-in toolbox with the key of the turn, the way a harness does.
func callTool(t *testing.T, spec adapter.TurnSpec, name, arguments string) {
	t.Helper()
	if spec.AgentTools == nil {
		t.Fatal("the turn has no toolbox")
	}
	url := spec.AgentTools.ServerURL + "/api/agent/mcp"
	body := fmt.Sprintf(`{"id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, arguments)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+spec.AgentTools.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestATurnThatStartedACommandIsGradedOnTheTurnTheCompletionNoteWakes(t *testing.T) {
	good := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"It scores answers.","reply_to":"7001"}]}`
	st := &stub{}
	st.reply = func(spec adapter.TurnSpec, n int) []adapter.Event {
		if n == 1 {
			// The model starts a command and ends the turn with nothing to say, as the prompt tells it to.
			callTool(t, spec, "run_command", `{"cmd":"ls internal/eval","request_key":"k"}`)
			return []adapter.Event{usageEvent(100, 10, 0.25, false, false), answerEvent(`{"messages":[]}`)}
		}
		return []adapter.Event{usageEvent(40, 5, 0.5, false, false), answerEvent(good)}
	}
	o := newOptions(t, st, "s01-plain-reply")
	o.Scenarios[0].Expect.CommandDone = &eval.CommandDone{ExitCode: 0, Tail: "ok"}
	cases, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || len(cases[0].Attempts) != 1 {
		t.Fatalf("the pair of turns is one attempt: %+v", cases)
	}
	a := cases[0].Attempts[0]
	if !a.Correct || a.Turns != 2 || a.TokIn != 140 || a.TokOut != 15 || a.CostUSD < 0.749 || a.CostUSD > 0.751 || o.Spent() < 0.749 || o.Spent() > 0.751 {
		t.Fatalf("graded on the second turn, with both turns' tokens and cost: %+v spent %v", a, o.Spent())
	}
	if len(a.Tools) != 1 || a.Tools[0] != "run_command" {
		t.Fatalf("tools of the turns: %v", a.Tools)
	}
	if len(st.specs) != 2 || !st.specs[0].IsolateUserMCP || !st.specs[1].IsolateUserMCP {
		t.Fatalf("every turn of the eval is isolated from the person's MCP servers: %+v", st.specs)
	}
	second := st.specs[1]
	if !second.NativeSessionStarted || second.NativeSessionHandle == "" || second.NativeSessionHandle != st.specs[0].NativeSessionHandle {
		t.Fatalf("the completion note resumes the session of the first turn: %+v", second)
	}
	for _, want := range []string{"your command finished", "`ls internal/eval` finished with exit code 0; output tail: 'ok'"} {
		if !strings.Contains(string(second.NotesJSON), want) {
			t.Fatalf("the note lacks %q: %s", want, second.NotesJSON)
		}
	}
	report := eval.Render("t", cases, o.Scenarios)
	if !strings.Contains(report, "## Cases that needed the second turn") || !strings.Contains(report, "| claude-x | 0 | 1 | 1 | 1 |") {
		t.Fatalf("the report counts the cases that needed the second turn:\n%s", report)
	}
}

func TestNoCompletionTurnWithoutACommandOrWithAPassingAnswerOrWithoutTheScenarioAsking(t *testing.T) {
	good := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"It scores answers.","reply_to":"7001"}]}`
	cases := []struct {
		name     string
		command  bool
		answer   string
		scenario bool
	}{
		{"no command started", false, `{"messages":[]}`, true},
		{"a passing answer after a command", true, good, true},
		{"the scenario defines no ending", true, `{"messages":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &stub{reply: func(spec adapter.TurnSpec, n int) []adapter.Event {
				if tc.command {
					callTool(t, spec, "run_command", `{"cmd":"true","request_key":"k"}`)
				}
				return []adapter.Event{answerEvent(tc.answer)}
			}}
			o := newOptions(t, st, "s01-plain-reply")
			o.Scenarios[0].Expect.CommandDone = nil // s01 defines one in its file
			if tc.scenario {
				o.Scenarios[0].Expect.CommandDone = &eval.CommandDone{ExitCode: 0}
			}
			out, err := o.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range out[0].Attempts {
				if a.Turns != 0 {
					t.Fatalf("no second turn expected: %+v", a)
				}
			}
			if len(st.specs) != 1 {
				t.Fatalf("turns started: %d", len(st.specs))
			}
		})
	}
}

func TestCommandDoneNeedsRunCommand(t *testing.T) {
	all, err := eval.LoadDir("../testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.ID != "s20-no-tools" {
			continue
		}
		s.Expect.CommandDone = &eval.CommandDone{}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "run_command") {
			t.Fatalf("a scenario with no run_command cannot define how a command ends: %v", err)
		}
	}
}

// A still-running command earns a completion turn after an empty answer, or
// any non-handoff answer in a scenario that checks a handoff outcome.
func TestSecondTurnOnlyForAStillRunningCommandAndNoRealAnswer(t *testing.T) {
	wrong := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"Nope.","reply_to":"9999"}]}`
	willReport := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"I started the build and will report when it ends.","reply_to":"msg_01EVALMSG7203","handoff":false}]}`
	substantive := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"The build succeeded.","reply_to":"msg_01EVALMSG7203","handoff":false}]}`
	handoffFailed := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"@` + eval.DispatcherID + ` it failed.","reply_to":"msg_01EVALMSG7203","handoff":true,"outcome":"failed"}]}`
	run := func(t *testing.T, scenario string, first string, observeFinish bool) (turns int, specs int) {
		t.Helper()
		st := &stub{}
		st.reply = func(spec adapter.TurnSpec, n int) []adapter.Event {
			if n == 1 {
				callTool(t, spec, "run_command", `{"cmd":"go build ./...","request_key":"k"}`)
				if observeFinish {
					callTool(t, spec, "command_log", `{"id":"c_1"}`)
				}
			}
			return []adapter.Event{answerEvent(first)}
		}
		o := newOptions(t, st, scenario)
		o.Scenarios[0].Expect.CommandDone = &eval.CommandDone{ExitCode: 0, Tail: "ok"}
		out, err := o.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return out[0].Attempts[0].Turns, len(st.specs)
	}
	for _, tc := range []struct {
		name, scenario, first string
		observeFinish         bool
		second                bool
	}{
		{"an empty answer, command running", "s01-plain-reply", `{"messages":[]}`, false, true},
		{"a will-report message in a handoff scenario, command running", "s03-handoff-done", willReport, false, true},
		{"a substantive non-handoff message in a handoff scenario, command running", "s03-handoff-done", substantive, false, true},
		{"a wrong answer after the command finished", "s01-plain-reply", wrong, true, false},
		{"an empty answer after the command finished", "s01-plain-reply", `{"messages":[]}`, true, false},
		{"a wrong reply in a scenario with no handoff, command running", "s01-plain-reply", wrong, false, false},
		{"a wrong handoff, command running", "s03-handoff-done", handoffFailed, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns, specs := run(t, tc.scenario, tc.first, tc.observeFinish)
			if tc.second && (turns != 2 || specs != 2) || !tc.second && (turns != 0 || specs != 1) {
				t.Fatalf("second turn wanted %v: Turns=%d, turns started=%d", tc.second, turns, specs)
			}
		})
	}
}
