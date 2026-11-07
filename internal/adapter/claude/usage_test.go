package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// usageEvents runs a stand-in claude that prints the given stream-json lines and returns the usage events the adapter produced.
func usageEvents(t *testing.T, lines ...string) []*adapter.Usage {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	script := "#!/bin/sh\ncat >/dev/null\n"
	for _, line := range lines {
		script += "printf '%s\\n' '" + line + "'\n"
	}
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := &adapter.AgentToolsAccess{AgentID: "agt_usage", ServerURL: "http://127.0.0.1:9897", Key: "aak_canary", Command: "/usr/bin/aimebu"}
	turn, err := (&Adapter{Command: path, AllowNative: true}).Start(context.Background(), adapter.TurnSpec{TurnID: "usage", Agent: adapter.AgentSpec{ID: "agt_usage", Model: "haiku"}, CheckoutPath: root, AgentTools: config, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	var out []*adapter.Usage
	for event := range turn.Events() {
		if event.Kind == adapter.EventUsage {
			out = append(out, event.Usage)
		}
	}
	turn.Wait()
	return out
}

// The result of a resumed session carries the session's total cost, which the store prices as a difference, so the adapter marks it so; it also
// reports the cache tokens and, when one model did the work, which.
func TestResultReportsCacheTokensModelAndASessionTotalCost(t *testing.T) {
	got := usageEvents(t, `{"type":"result","result":"{\"messages\":[]}","usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":110,"cache_creation_input_tokens":40},"modelUsage":{"claude-haiku-4-5":{"contextWindow":200,"costUSD":0.002172}}}`)
	if len(got) != 1 {
		t.Fatalf("usage events: %+v", got)
	}
	u := got[0]
	if u.CostEstimate == nil || *u.CostEstimate != 0.002172 || !u.CostCumulative || u.Partial || u.Model != "claude-haiku-4-5" {
		t.Fatalf("cost: %+v", u)
	}
	if u.TokensCacheRead == nil || *u.TokensCacheRead != 110 || u.TokensCacheWrite == nil || *u.TokensCacheWrite != 40 || u.TokensIn == nil || *u.TokensIn != 5 {
		t.Fatalf("tokens: %+v", u)
	}
	// Several models: the result does not say which one the turn used.
	got = usageEvents(t, `{"type":"result","result":"{\"messages\":[]}","usage":{"input_tokens":5,"output_tokens":7},"modelUsage":{"a":{"costUSD":0.1},"b":{"costUSD":0.2}}}`)
	if len(got) != 1 || got[0].Model != "" || got[0].CostEstimate == nil || *got[0].CostEstimate < 0.3-1e-9 {
		t.Fatalf("two models: %+v", got)
	}
}

// A turn that dies before its result still spent what its API responses used. Each response counts once however many events repeat its usage,
// a sub-agent's responses are not the turn's, and what only the result carries (output tokens, cost) stays unknown.
func TestATurnWithNoResultReportsItsPartialSpend(t *testing.T) {
	got := usageEvents(t,
		`{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":20,"output_tokens":1}}}`,
		`{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":20,"output_tokens":1}}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_child","message":{"id":"msg_sub","usage":{"input_tokens":900}}}`,
		`{"type":"assistant","message":{"id":"msg_2","usage":{"input_tokens":3,"cache_read_input_tokens":130}}}`)
	if len(got) != 1 {
		t.Fatalf("usage events: %+v", got)
	}
	u := got[0]
	if !u.Partial || u.TokensIn == nil || *u.TokensIn != 13 || u.TokensCacheRead == nil || *u.TokensCacheRead != 230 || u.TokensCacheWrite == nil || *u.TokensCacheWrite != 20 {
		t.Fatalf("partial usage: %+v", u)
	}
	if u.TokensOut != nil || u.CostEstimate != nil || u.CostCumulative {
		t.Fatalf("what only a result knows must stay unknown: %+v", u)
	}
	// With a result there is no partial record: the result is the whole account.
	got = usageEvents(t,
		`{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":10}}}`,
		`{"type":"result","result":"{\"messages\":[]}","usage":{"input_tokens":10,"output_tokens":2},"modelUsage":{"x":{"costUSD":0.1}}}`)
	if len(got) != 1 || got[0].Partial {
		t.Fatalf("a finished turn: %+v", got)
	}
	// Nothing seen, nothing recorded.
	if got = usageEvents(t, `{"type":"system","subtype":"init"}`); len(got) != 0 {
		t.Fatalf("a turn that spent nothing we saw: %+v", got)
	}
}
