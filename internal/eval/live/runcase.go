package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// runCase runs one repetition of a scenario on a target: the first try, then up to MaxTurnsToValid-1 repairs while the parser rejects the answer.
// A repair is a turn in the same session that carries the held-reply note, which is what an agent whose reply was held gets. Only a reply the
// parser rejects can be repaired: a valid answer that does the wrong thing is invisible to the server, so it ends the case.
func (o *Options) runCase(ctx context.Context, impl adapter.Adapter, target Target, row protocol.LauncherRow, checkout string, scenario eval.Scenario, rep int) eval.Case {
	c := eval.Case{Model: target.Name, Scenario: scenario.ID, Rep: rep}
	notes, turnContext, err := scenario.Input.Build()
	if err == nil {
		turnContext, err = withIdentity(turnContext, target)
	}
	if err != nil {
		c.Attempts = []eval.Attempt{{Class: eval.ClassHarness, Err: "building the turn input: " + err.Error()}}
		return c
	}
	session := ""
	if target.Tool == eval.KindClaude || target.Tool == eval.KindPi {
		session = uuid() // the run's own session: these harnesses take the id from the caller, and it is never an agent's
	}
	started := false
	var previous adapter.Usage
	for attempt := 0; attempt < scenario.Expect.MaxTurnsToValid; attempt++ {
		o.turnCounter++
		turnID := fmt.Sprintf("turn_eval_%s_%04d", o.RunID, o.turnCounter)
		a, handle, usage := o.runTurn(ctx, impl, target, row, checkout, scenario, turnID, notes, turnContext, session, started)
		a.Millis = usage.millis
		if usage.tokensCumulative {
			a.TokIn, a.TokOut = nonNegative(a.TokIn-usageIn(previous)), nonNegative(a.TokOut-usageOut(previous))
		}
		if usage.costCumulative {
			a.CostUSD = nonNegativeF(a.CostUSD - usageCost(previous))
		}
		previous = usage.raw
		o.spentUSD += a.CostUSD
		c.Attempts = append(c.Attempts, a)
		if handle != "" {
			session = handle
		}
		if a.Valid || a.Class == eval.ClassHarness || session == "" {
			break
		}
		// The next turn resumes this session with the held-reply note.
		started = true
		notes = heldNote(9000+attempt, heldDetail(a.Err))
	}
	return c
}

// turnUsage is what a turn reported beyond the attempt: its time, the raw report, and which of its figures are the session's running totals (a
// harness may report either one so, as the store reads them: internal/store/usage.go).
type turnUsage struct {
	millis           int64
	costCumulative   bool
	tokensCumulative bool
	raw              adapter.Usage
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func nonNegativeF(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// runTurn runs one turn through the launcher and scores its answer.
func (o *Options) runTurn(ctx context.Context, impl adapter.Adapter, target Target, row protocol.LauncherRow, checkout string, scenario eval.Scenario, turnID string, notes, turnContext json.RawMessage, session string, started bool) (eval.Attempt, string, turnUsage) {
	var used turnUsage
	fail := func(format string, args ...any) (eval.Attempt, string, turnUsage) {
		return eval.Attempt{Class: eval.ClassHarness, Err: fmt.Sprintf(format, args...)}, "", used
	}
	spec := adapter.TurnSpec{TurnID: turnID, Agent: adapter.AgentSpec{ID: eval.WorkerID, Tool: target.Tool, Variant: "docker", LauncherID: row.ID, Model: target.Model, Provider: target.Provider},
		NativeSessionHandle: session, NativeSessionStarted: started, CheckoutPath: checkout, Mode: adapter.ModeNormal, NotesJSON: notes, ContextJSON: turnContext}
	if _, err := supervisor.ApplyLauncher(&spec, row, checkout); err != nil {
		return fail("launcher: %v", err)
	}
	// The row's agent server address is the real server's. The turn's toolbox is the stand-in, which a container reaches by the host gateway.
	spec.AgentServerURL = ""
	if o.VisibleHost != "" {
		spec.AgentServerURL = fmt.Sprintf("http://%s:%d", o.VisibleHost, o.MCP.Port())
	}
	// Every normal turn has agent tools access (the Docker adapters refuse one without it), so a scenario with no tools gets a toolbox that offers
	// none. The key is random: the stand-in listens on every interface, and a key derived from the turn id could be guessed by a neighbour.
	key := toolboxKey()
	o.MCP.Register(key, scenario.Input.AllowedTools)
	spec.AgentTools = &adapter.AgentToolsAccess{AgentID: eval.WorkerID, ServerURL: fmt.Sprintf("http://127.0.0.1:%d", o.MCP.Port()), Key: key, Command: "/usr/bin/aimebu"}
	spec.AllowedTools = scenario.Input.AllowedTools
	spec.Providers = providersFor(target)
	turnCtx, cancel := context.WithTimeout(ctx, o.TurnTimeout)
	defer cancel()
	begin := time.Now()
	turn, err := impl.Start(turnCtx, spec)
	if err != nil {
		return fail("start: %v", err)
	}
	var (
		answer, newSession string
		hasAnswer          bool
		usage              *adapter.Usage
	)
	for event := range turn.Events() {
		switch event.Kind {
		case adapter.EventFinalAnswer:
			hasAnswer, answer = true, string(event.Answer)
		case adapter.EventNativeSessionStarted:
			newSession = event.NativeSessionHandle
		case adapter.EventUsage:
			if event.Usage != nil && !event.Usage.Partial && !event.Usage.Background {
				usage = event.Usage
			}
		}
	}
	exit, err := turn.Wait()
	used.millis = time.Since(begin).Milliseconds()
	tools := o.MCP.Calls(key)
	a := eval.Attempt{Tools: tools}
	if usage != nil {
		if usage.TokensIn != nil {
			a.TokIn = *usage.TokensIn
		}
		if usage.TokensOut != nil {
			a.TokOut = *usage.TokensOut
		}
		if usage.CostEstimate != nil {
			a.CostUSD = *usage.CostEstimate
		}
		used.costCumulative, used.tokensCumulative = usage.CostCumulative, usage.TokensCumulative
		used.raw = *usage
	}
	if err != nil || !hasAnswer {
		a.Class, a.Err = eval.ClassHarness, fmt.Sprintf("no final answer: exit %d, terminal=%t, err=%v, stderr=%s", exit.ExitCode, exit.TerminalEvent, err, truncate(exit.StderrTail, 300))
		return a, newSession, used
	}
	r := eval.Score(answer, scenario, tools)
	a.Candidate, a.Valid, a.Strict, a.Correct, a.Class = truncate(answer, 4000), r.Valid, r.Strict, r.Correct, r.Class
	switch {
	case r.Err != "":
		a.Err = r.Err
	case len(r.Failed) > 0:
		a.Err = strings.Join(r.Failed, "; ")
	}
	return a, newSession, used
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// heldDetail is the reason a held reply's note gives: the parser's error, except that one which quotes what the agent wrote is described instead,
// as internal/store/answers.go (heldDetail) does for a note a room can read.
func heldDetail(detail string) string {
	if strings.ContainsAny(detail, `"'`) || strings.Contains(detail, "json:") {
		return "it was not one valid JSON answer object (malformed JSON, an unknown or misplaced field, or text around the object)"
	}
	return strings.TrimPrefix(detail, "invalid_answer: ")
}

func usageIn(u adapter.Usage) int64 {
	if u.TokensIn == nil {
		return 0
	}
	return *u.TokensIn
}

func usageOut(u adapter.Usage) int64 {
	if u.TokensOut == nil {
		return 0
	}
	return *u.TokensOut
}

func usageCost(u adapter.Usage) float64 {
	if u.CostEstimate == nil {
		return 0
	}
	return *u.CostEstimate
}

// toolboxKey is a fresh random bearer key for a turn's stand-in toolbox: "aak_" (the prefix the adapters require) and 128 random bits.
func toolboxKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system's random source failing leaves nothing safe to do
	}
	return "aak_" + hex.EncodeToString(b[:])
}

// providersFor is the provider document an OpenCode turn carries, chosen as the store does (internal/store/harness_provider.go,
// providersForTurn): the one provider of the target whose "<id>/" prefix the model id has, and none for any other tool or a model with no prefix.
func providersFor(target Target) []adapter.CustomProvider {
	if target.Tool != eval.KindOpenCode {
		return nil
	}
	prefix, _, found := strings.Cut(target.Model, "/")
	if !found {
		return nil
	}
	for _, p := range target.Providers {
		if p.ID == prefix {
			return []adapter.CustomProvider{p}
		}
	}
	return nil
}
