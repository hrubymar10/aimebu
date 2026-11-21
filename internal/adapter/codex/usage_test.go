package codex

import (
	"context"
	"math"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// usageOf runs a stand-in codex that prints the given turn.completed line and returns the usage event the adapter produced.
func usageOf(t *testing.T, completed string) *adapter.Usage {
	t.Helper()
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	script := filepath.Join(root, "codex")
	body := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}' '{\"type\":\"turn.started\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"messages\\\":[]}\"}}' '" + completed + "'\n"
	if err := testenv.WriteExecutable(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(filepath.Join(root, "runtime"))
	a.Command, a.AuthPath = script, ""
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "usage", Agent: adapter.AgentSpec{ID: "agt_usage", Model: "gpt-6-luna"}, Mode: adapter.ModeNormal, CheckoutPath: root, NotesJSON: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	var usage *adapter.Usage
	for event := range turn.Events() {
		if event.Kind == adapter.EventUsage {
			usage = event.Usage
		}
	}
	turn.Wait()
	if usage == nil {
		t.Fatal("no usage event")
	}
	return usage
}

func pick(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// Codex's turn.completed counts the cached share inside input_tokens (the Responses API's input_tokens includes it), so In is what was not
// cached and Cache read is the cached share, as the other adapters report them: the two never overlap.
func TestUsageSeparatesCachedInputFromInput(t *testing.T) {
	for name, test := range map[string]struct {
		line           string
		in, cache, out any
	}{
		// What codex 0.162.1 printed for a turn whose model reported 100 input tokens, 80 of them cached, and 7 output tokens.
		"reported":     {`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":80,"cache_write_input_tokens":5,"output_tokens":7,"reasoning_output_tokens":2}}`, int64(15), int64(80), int64(7)},
		"not cached":   {`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":7}}`, int64(100), int64(0), int64(7)},
		"not told":     {`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":7}}`, int64(100), nil, int64(7)},
		"all cached":   {`{"type":"turn.completed","usage":{"input_tokens":80,"cached_input_tokens":80,"output_tokens":1}}`, int64(0), int64(80), int64(1)},
		"inconsistent": {`{"type":"turn.completed","usage":{"input_tokens":5,"cached_input_tokens":9,"output_tokens":1}}`, int64(0), int64(9), int64(1)},
		"no input":     {`{"type":"turn.completed","usage":{"cached_input_tokens":9,"output_tokens":1}}`, nil, int64(9), int64(1)},
	} {
		t.Run(name, func(t *testing.T) {
			usage := usageOf(t, test.line)
			if got := [3]any{pick(usage.TokensIn), pick(usage.TokensCacheRead), pick(usage.TokensOut)}; got != [3]any{test.in, test.cache, test.out} {
				t.Errorf("in/cache/out = %v, want %v", got, [3]any{test.in, test.cache, test.out})
			}
			if name == "reported" && (usage.TokensCacheWrite == nil || *usage.TokensCacheWrite != 5) {
				t.Errorf("cache write = %v, want 5", usage.TokensCacheWrite)
			}
			if name == "inconsistent" && usage.CostEstimate != nil {
				t.Errorf("inconsistent token shares were priced: %+v", usage)
			}
			if name == "reported" {
				want := (15*0.1 + 80*0.01 + 5*0.125 + 7*0.5) / 1_000_000
				if usage.CostEstimate == nil || math.Abs(*usage.CostEstimate-want) > 1e-12 || usage.CostSource != "estimated" {
					t.Errorf("price estimate = %+v, want %g", usage, want)
				}
			}
		})
	}
}

// The cold Codex path reports the thread's running token totals (exec's turn.completed carries token_usage.total, earlier turns
// included), so the usage event says so: the store turns the totals into the turn's own usage by subtracting the session's previous
// totals, and the cost priced over the totals is a running total the same way.
func TestColdUsageReportsSessionTotals(t *testing.T) {
	usage := usageOf(t, `{"type":"turn.completed","usage":{"input_tokens":13960000,"cached_input_tokens":13960000,"output_tokens":7}}`)
	if !usage.TokensCumulative {
		t.Fatalf("the cold usage is not marked as session totals: %+v", usage)
	}
	if usage.CostEstimate == nil && !usage.CostCumulative {
		t.Fatalf("a priced cold usage is not marked cumulative: %+v", usage)
	}
	if usage.CostEstimate != nil && usage.CostSource != "estimated" {
		t.Errorf("cost source = %q, want estimated", usage.CostSource)
	}
}
