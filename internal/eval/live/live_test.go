package live

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func rpc(t *testing.T, port int, key, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/agent/mcp", port), strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp.StatusCode, out.String()
}

func TestMCPStandInOffersTheScenariosToolsAndRecordsCalls(t *testing.T) {
	m, err := StartMCP()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Register("aak_a", []string{"react", "run_command"})
	if code, _ := rpc(t, m.Port(), "", `{"id":1,"method":"tools/list"}`); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := rpc(t, m.Port(), "aak_unknown", `{"id":1,"method":"tools/list"}`); code != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d", code)
	}
	_, list := rpc(t, m.Port(), "aak_a", `{"id":2,"method":"tools/list"}`)
	if !strings.Contains(list, `"react"`) || !strings.Contains(list, `"run_command"`) || strings.Contains(list, "dm_agent") || !strings.Contains(list, `"id":2`) {
		t.Fatalf("tools/list: %s", list)
	}
	rpc(t, m.Port(), "aak_a", `{"id":3,"method":"tools/call","params":{"name":"react","arguments":{}}}`)
	if calls := m.Calls("aak_a"); len(calls) != 1 || calls[0] != "react" {
		t.Fatalf("calls %v", calls)
	}
	if len(m.Calls("aak_b")) != 0 {
		t.Fatal("another turn's calls are not shared")
	}
}

func TestUsageHottestAndLimit(t *testing.T) {
	u := Usage{"claude/main/session": 99, "claude/main/weekly": 42.5, "claude/spare/seven_day_opus": 91, "codex/x/weekly": 10}
	if name, used := u.Hottest(); name != "claude/spare/seven_day_opus" || used != 91 {
		t.Fatalf("hottest %s %v: a session window never counts", name, used)
	}
	if got := u.OverLimit(90); !strings.Contains(got, "claude/spare/seven_day_opus") {
		t.Fatalf("over limit: %q", got)
	}
	if got := (Usage{"claude/main/weekly": 89}).OverLimit(90); got != "" {
		t.Fatalf("under limit: %q", got)
	}
	table := Table(Usage{"a/b/weekly": 10, "a/b/session": 5}, Usage{"a/b/weekly": 12.5})
	if !strings.Contains(table, "| a/b/weekly | 10.0% | 12.5% | +2.5 points |") || !strings.Contains(table, "| a/b/session | 5.0% | n/a | n/a |") {
		t.Fatalf("table:\n%s", table)
	}
}

func TestQuotaFetchReadsTheServersUsages(t *testing.T) {
	var cookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		if r.URL.Path != "/api/usages" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"providers":[{"provider_name":"claude","profiles":[{"profile_name":"main","windows":[{"key":"session","percent_used":12},{"key":"weekly","percent_used":40.5}]}]}]}`)
	}))
	defer server.Close()
	t.Setenv("EVAL_TEST_COOKIE", "aimebu_session=abc")
	u, err := QuotaSource{URL: server.URL + "/", CookieEnv: "EVAL_TEST_COOKIE"}.Fetch(context.Background())
	if err != nil || u["claude/main/weekly"] != 40.5 || u["claude/main/session"] != 12 || cookie != "aimebu_session=abc" {
		t.Fatalf("usage %v err %v cookie %q", u, err, cookie)
	}
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer denied.Close()
	if _, err := (QuotaSource{URL: denied.URL}).Fetch(context.Background()); err == nil {
		t.Fatal("a refused read is an error")
	}
}

// stub is an adapter whose turns answer from a function; it records every spec it was started with.
type stub struct {
	mu    sync.Mutex
	specs []adapter.TurnSpec
	reply func(spec adapter.TurnSpec, n int) []adapter.Event
}

func (s *stub) Catalog() []adapter.CatalogEntry { return nil }
func (s *stub) Capabilities(string, adapter.Environment) adapter.Capabilities {
	return adapter.Capabilities{}
}
func (s *stub) Start(_ context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	n := len(s.specs)
	s.mu.Unlock()
	ch := make(chan adapter.Event, 8)
	for _, e := range s.reply(spec, n) {
		ch <- e
	}
	close(ch)
	return stubTurn{ch}, nil
}

type stubTurn struct{ ch chan adapter.Event }

func (t stubTurn) Events() <-chan adapter.Event    { return t.ch }
func (t stubTurn) Cancel() error                   { return nil }
func (t stubTurn) Wait() (adapter.ExitInfo, error) { return adapter.ExitInfo{TerminalEvent: true}, nil }

func answerEvent(text string) adapter.Event {
	return adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(text)}
}

func usageEvent(in, out int64, cost float64, cumulative bool) adapter.Event {
	return adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{TokensIn: &in, TokensOut: &out, CostEstimate: &cost, CostCumulative: cumulative}}
}

func scenarios(t *testing.T, ids ...string) []eval.Scenario {
	t.Helper()
	all, err := eval.LoadDir("../testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	var out []eval.Scenario
	for _, id := range ids {
		for _, s := range all {
			if s.ID == id {
				out = append(out, s)
			}
		}
	}
	if len(out) != len(ids) {
		t.Fatalf("scenarios %v not all found", ids)
	}
	return out
}

func newOptions(t *testing.T, st *stub, ids ...string) *Options {
	t.Helper()
	m, err := StartMCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	root := t.TempDir()
	return &Options{RunID: "r1", ScratchDir: root, Reps: 1, MCP: m, Scenarios: scenarios(t, ids...),
		Targets:    []Target{{Name: "claude-x", Tool: "claude", LauncherID: "lch_1", Model: "claude-x"}},
		Launchers:  []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "lch_1", Tool: "claude", Containerized: true, AgentServerURL: "http://real.example:1"}, ResolvedCommand: "/bin/claude-docker", Available: true, Features: []string{"claude_home_shared"}}},
		NewAdapter: func(string) (adapter.Adapter, error) { return st, nil }}
}

func TestRunScoresAndRepairsInTheSameSession(t *testing.T) {
	good := `{"messages":[{"room":"` + eval.TaskRoom + `","body":"It scores answers.","reply_to":"7001"}]}`
	st := &stub{reply: func(spec adapter.TurnSpec, n int) []adapter.Event {
		if n == 1 {
			return []adapter.Event{usageEvent(100, 10, 0.5, true), answerEvent("Sure! " + good)}
		}
		return []adapter.Event{usageEvent(160, 30, 0.7, true), answerEvent(good)}
	}}
	o := newOptions(t, st, "s01-plain-reply")
	var log bytes.Buffer
	o.Log = &log
	cases, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || len(cases[0].Attempts) != 2 {
		t.Fatalf("cases %+v", cases)
	}
	first, second := cases[0].Attempts[0], cases[0].Attempts[1]
	if first.Valid || first.Class != eval.ClassFormat || !second.Valid || !second.Correct || !second.Strict {
		t.Fatalf("attempts %+v", cases[0].Attempts)
	}
	// The repair turn's usage is the session's running total minus the first turn's.
	if second.TokIn != 60 || second.TokOut != 20 || second.CostUSD < 0.199 || second.CostUSD > 0.201 || o.Spent() < 0.69 || o.Spent() > 0.71 {
		t.Fatalf("second attempt %+v spent %v", second, o.Spent())
	}
	if len(st.specs) != 2 {
		t.Fatalf("%d turns", len(st.specs))
	}
	a, b := st.specs[0], st.specs[1]
	if a.NativeSessionHandle == "" || a.NativeSessionHandle != b.NativeSessionHandle || a.NativeSessionStarted || !b.NativeSessionStarted {
		t.Fatalf("session: first %q started=%t, second %q started=%t", a.NativeSessionHandle, a.NativeSessionStarted, b.NativeSessionHandle, b.NativeSessionStarted)
	}
	if !strings.Contains(string(b.NotesJSON), "Your previous reply was held: ") || !strings.Contains(string(b.NotesJSON), "text around the JSON") {
		t.Fatalf("repair note: %s", b.NotesJSON)
	}
	if !strings.Contains(a.CheckoutPath, "aimebu-eval-r1-claude-x") || a.LauncherPath != "/bin/claude-docker" || !a.LauncherContainerized || a.Agent.Variant != "docker" || a.Agent.LauncherID != "lch_1" {
		t.Fatalf("spec %+v", a)
	}
	if a.AgentServerURL != "" || a.AgentTools == nil || !strings.HasPrefix(a.AgentTools.Key, "aak_eval_") || a.AgentTools.Key == b.AgentTools.Key {
		t.Fatalf("the toolbox is the stand-in with a key per turn: %q %+v", a.AgentServerURL, a.AgentTools)
	}
	if !strings.Contains(string(a.ContextJSON), `"harness":"claude"`) || !strings.Contains(string(a.ContextJSON), `"model":"claude-x"`) {
		t.Fatalf("context: %s", a.ContextJSON)
	}
	if !strings.Contains(log.String(), "invalid (format) -> correct") {
		t.Fatalf("log: %s", log.String())
	}
}

func TestRunReadsTheToolCallsFromTheStandIn(t *testing.T) {
	var o *Options
	st := &stub{reply: func(spec adapter.TurnSpec, n int) []adapter.Event {
		rpc(t, o.MCP.Port(), spec.AgentTools.Key, `{"id":1,"method":"tools/call","params":{"name":"run_command"}}`)
		return []adapter.Event{answerEvent(`{"messages":[]}`)}
	}}
	o = newOptions(t, st, "s15-waiting-on-command")
	cases, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := cases[0].Attempts[0]
	if !a.Valid || a.Correct || a.Class != eval.ClassBehaviour || !strings.Contains(a.Err, "tool_not_called") || len(a.Tools) != 1 || a.Tools[0] != "run_command" {
		t.Fatalf("a model that starts a command again after being told it runs: %+v", a)
	}
	if len(cases[0].Attempts) != 1 {
		t.Fatal("a valid answer that does the wrong thing is not repaired")
	}
}

func TestRunHarnessFailureEndsTheCaseAndIsNotRepaired(t *testing.T) {
	st := &stub{reply: func(adapter.TurnSpec, int) []adapter.Event { return nil }}
	o := newOptions(t, st, "s01-plain-reply")
	cases, err := o.Run(context.Background())
	if err != nil || len(cases[0].Attempts) != 1 || cases[0].Attempts[0].Class != eval.ClassHarness {
		t.Fatalf("cases %+v err %v", cases, err)
	}
}

func TestRunStopsOnBudgetAndOnQuota(t *testing.T) {
	st := &stub{reply: func(adapter.TurnSpec, int) []adapter.Event {
		return []adapter.Event{usageEvent(1, 1, 1.0, false), answerEvent(`{"messages":[]}`)}
	}}
	o := newOptions(t, st, "s18-fyi-silence", "s15-waiting-on-command")
	o.MaxUSD = 1.0
	cases, err := o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "run stopped") || len(cases) != 1 {
		t.Fatalf("budget: %d cases, %v", len(cases), err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"providers":[{"provider_name":"claude","profiles":[{"profile_name":"main","windows":[{"key":"weekly","percent_used":93}]}]}]}`)
	}))
	defer server.Close()
	o = newOptions(t, st, "s18-fyi-silence")
	o.Quota = &QuotaSource{URL: server.URL}
	if _, err := o.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "93%") {
		t.Fatalf("a weekly window above the limit stops the run before it starts: %v", err)
	}
	if len(st.specs) != 1 {
		t.Fatalf("no turn after the guard fired: %d", len(st.specs))
	}
}

func TestRunRefusesANativeLauncherAndSkipsOnlyKinds(t *testing.T) {
	st := &stub{reply: func(adapter.TurnSpec, int) []adapter.Event { return []adapter.Event{answerEvent(`{"messages":[]}`)} }}
	o := newOptions(t, st, "s01-plain-reply")
	o.Launchers[0].Containerized = false
	if _, err := o.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "not a Docker launcher") {
		t.Fatalf("native: %v", err)
	}
	o = newOptions(t, st, "s22-unchanged-marker")
	o.Targets[0].Tool = "codex"
	cases, err := o.Run(context.Background())
	if err != nil || len(cases) != 0 {
		t.Fatalf("a Claude-only scenario is skipped for codex: %v %v", cases, err)
	}
	if _, err := os.Stat(o.ScratchDir); err != nil {
		t.Fatal(err)
	}
}

func TestUUIDIsVersion4(t *testing.T) {
	id := uuid()
	if len(id) != 36 || id[14] != '4' || strings.Contains("01234567", string(id[19])) {
		t.Fatalf("uuid %s", id)
	}
}
