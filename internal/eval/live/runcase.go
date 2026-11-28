package live

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
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
	if err := prepareCheckout(checkout, scenario.Input.Checkout); err != nil {
		c.Attempts = []eval.Attempt{{Class: eval.ClassHarness, Err: "preparing the checkout: " + err.Error()}}
		return c
	}
	session := ""
	if target.Tool == eval.KindClaude || target.Tool == eval.KindPi {
		session = uuid() // the run's own session: these harnesses take the id from the caller, and it is never an agent's
	}
	started, completed := false, false
	var previous adapter.Usage
	for attempt := 0; attempt < scenario.Expect.MaxTurnsToValid; attempt++ {
		o.turnCounter++
		turnID := fmt.Sprintf("turn_eval_%s_%04d", o.RunID, o.turnCounter)
		a, handle, usage := o.runTurn(ctx, impl, target, row, checkout, scenario, turnID, notes, turnContext, session, started)
		settleUsage(&a, usage, previous)
		previous = usage.raw
		o.spentUSD += a.CostUSD
		if handle != "" {
			session = handle
		}
		// A turn that started a command and ended without an answer that passes is waiting for it, as the prompt asks. The real server wakes it
		// with the command's completion note; so does the run, as a second turn of the same session, and that turn's answer is the one graded.
		if done := scenario.Expect.CommandDone; done != nil && !completed && a.Valid && !a.Correct && a.Class != eval.ClassHarness && session != "" && contains(a.Tools, "run_command") && waitingForCommand(scenario, usage) {
			completed = true
			o.turnCounter++
			second, handle2, usage2 := o.runTurn(ctx, impl, target, row, checkout, scenario, fmt.Sprintf("turn_eval_%s_%04d", o.RunID, o.turnCounter), completionNote(8800+attempt, usage.commands, *done), turnContext, session, true)
			settleUsage(&second, usage2, previous)
			previous = usage2.raw
			o.spentUSD += second.CostUSD
			if handle2 != "" {
				session = handle2
			}
			a = mergeTurns(a, second)
			started = true
		}
		c.Attempts = append(c.Attempts, a)
		if a.Valid || a.Class == eval.ClassHarness || session == "" {
			break
		}
		// The next turn resumes this session with the held-reply note.
		started = true
		notes = heldNote(9000+attempt, heldDetail(a.Err))
	}
	return c
}

// waitingForCommand says a turn that started a command ended waiting for it, as the prompt asks, so the completion note is its next turn: the
// command was still running at the end of the turn (the turn did not look at it with command_log or list_commands and was not told it had ended),
// the answer has no handoff, and it is empty or the scenario checks a handoff outcome. Message text is not inspected here: a wrong first
// answer can receive the completion note, but the graded outcome still depends on the second answer.
func waitingForCommand(scenario eval.Scenario, usage turnUsage) bool {
	switch {
	case usage.finished || usage.handoff:
		return false
	case usage.messages == 0:
		return true
	}
	for _, check := range scenario.Expect.Checks {
		if check.Op == "handoff_outcome" {
			return true
		}
	}
	return false
}

// settleUsage puts a turn's time on its attempt and, for a harness that reports session running totals, takes the previous turn's total off its
// tokens and cost, so an attempt counts only what its own turn used.
func settleUsage(a *eval.Attempt, usage turnUsage, previous adapter.Usage) {
	a.Millis = usage.millis
	if usage.tokensCumulative {
		a.TokFresh = nonNegative(a.TokFresh - tokenValue(previous.TokensIn))
		a.TokRead = nonNegative(a.TokRead - tokenValue(previous.TokensCacheRead))
		a.TokWrite = nonNegative(a.TokWrite - tokenValue(previous.TokensCacheWrite))
		a.TokIn = a.TokFresh + a.TokRead + a.TokWrite
		a.TokOut = nonNegative(a.TokOut - usageOut(previous))
	}
	if usage.costCumulative {
		a.CostUSD = nonNegativeF(a.CostUSD - usageCost(previous))
	}
}

// mergeTurns is the attempt of a turn that started a command and the turn that took its completion note: the second one's answer and scores,
// with the time, tokens and cost of both and the tools of both.
func mergeTurns(first, second eval.Attempt) eval.Attempt {
	second.Millis += first.Millis
	second.TokIn += first.TokIn
	second.TokOut += first.TokOut
	second.TokFresh += first.TokFresh
	second.TokRead += first.TokRead
	second.TokWrite += first.TokWrite
	second.HasCacheWrite = second.HasCacheWrite || first.HasCacheWrite
	second.CostUSD += first.CostUSD
	second.Tools = append(append([]string(nil), first.Tools...), second.Tools...)
	second.Turns = 2
	return second
}

// completionNote is the system note a turn gets when the command it started ends, in the words of internal/store's command-finished note: the
// command, its exit code and the last lines of its output, as the scenario defines how its commands end.
func completionNote(noteNo int, commands []string, done eval.CommandDone) json.RawMessage {
	command := "the command"
	if len(commands) > 0 {
		command = "`" + commands[len(commands)-1] + "`"
	}
	tail := "no output"
	if done.Tail != "" {
		tail = "'" + done.Tail + "'"
	}
	note := map[string]any{"note_id": fmt.Sprintf("note_01EVALDONE%04d", noteNo), "room_id": eval.TaskRoom, "from": "system", "message_id": fmt.Sprintf("msg_01EVALDONE%04d", noteNo), "no": noteNo,
		"body": fmt.Sprintf("Your command %s finished with exit code %d; output tail: %s", command, done.ExitCode, tail), "why": "your command finished"}
	out, _ := json.Marshal([]any{note})
	return out
}

// turnUsage is what a turn reported beyond the attempt: its time, the raw report, and which of its figures are the session's running totals (a
// harness may report either one so, as the store reads them: internal/store/usage.go).
type turnUsage struct {
	millis           int64
	commands         []string // the commands the turn started with run_command
	finished         bool     // the turn was told that a command it started had ended (MCPServer.CommandFinished)
	messages         int      // the messages of its answer, and whether one is a handoff
	handoff          bool
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
		NativeSessionHandle: session, NativeSessionStarted: started, CheckoutPath: checkout, Mode: adapter.ModeNormal, NotesJSON: notes, ContextJSON: turnContext,
		// Only the aimebu toolbox and the stand-in's tools: the machine the eval runs on has MCP servers of its own, which would show in the answers.
		IsolateUserMCP: true}
	if _, err := supervisor.ApplyLauncher(&spec, row, checkout); err != nil {
		return fail("launcher: %v", err)
	}
	// The row's agent server address is the real server's. The turn's toolbox is the stand-in, which a container reaches by the host gateway.
	spec.AgentServerURL = ""
	if o.VisibleHost != "" {
		spec.AgentServerURL = standinServerURL(o.VisibleHost, o.MCP.Port())
	}
	// Every normal turn has agent tools access (the Docker adapters refuse one without it), so a scenario with no tools gets a toolbox that offers
	// none. The key is random: the stand-in listens on every interface, and a key derived from the turn id could be guessed by a neighbour.
	key := toolboxKey()
	o.MCP.Register(key, scenario.Input.AllowedTools)
	messages, err := scenario.Input.Messages()
	if err != nil {
		return fail("history: %v", err)
	}
	o.MCP.SetHistory(key, messages)
	o.MCP.SetCommandDone(key, scenario.Expect.CommandDone)
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
	used.commands = o.MCP.Commands(key)
	used.finished = o.MCP.CommandFinished(key)
	a := eval.Attempt{Tools: tools}
	if usage != nil {
		a.TokIn = usageIn(*usage)
		a.TokFresh = tokenValue(usage.TokensIn)
		a.TokRead = tokenValue(usage.TokensCacheRead)
		a.TokWrite = tokenValue(usage.TokensCacheWrite)
		a.HasCacheWrite = usage.TokensCacheWrite != nil
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
		a.Class, a.Err = eval.ClassHarness, harnessFailure(exit, err)
		return a, newSession, used
	}
	r := eval.Score(answer, scenario, tools)
	used.messages = len(r.Candidate.Messages)
	for _, m := range r.Candidate.Messages {
		used.handoff = used.handoff || m.Handoff
	}
	a.Candidate, a.Valid, a.Strict, a.Correct, a.Class = truncate(answer, 4000), r.Valid, r.Strict, r.Correct, r.Class
	a.Via = answerVia(answer, o.MCP.Submitted(key))
	switch {
	case r.Err != "":
		a.Err = r.Err
	case len(r.Failed) > 0:
		a.Err = strings.Join(r.Failed, "; ")
	}
	return a, newSession, used
}

func standinServerURL(host string, port int) string {
	return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
}

// harnessFailure says why a turn ended with no answer. A harness may report its failure on stdout (OpenCode's "error" event, Claude's error
// result) and leave stderr empty, so the adapter's error event and summary are part of the message with the stderr tail; a failure that said
// nothing anywhere says so.
func harnessFailure(exit adapter.ExitInfo, err error) string {
	out := fmt.Sprintf("no final answer: exit %d, terminal=%t, err=%v", exit.ExitCode, exit.TerminalEvent, err)
	said := false
	for _, part := range []struct{ name, text string }{{"error_summary", exit.ErrorSummary}, {"error_event", exit.ErrorEvent}, {"stderr", exit.StderrTail}} {
		if text := strings.TrimSpace(part.text); text != "" {
			out += ", " + part.name + "=" + truncate(text, 300)
			said = true
		}
	}
	if !said {
		out += ", no output on stdout or stderr"
	}
	if strings.Contains(exit.ErrorEvent, "UnknownError: Unexpected server error") {
		out += ", hint: OpenCode prints this for a built-in provider it cannot use (no key in the environment of this process, or an unknown model), not only for a fault of its own"
	}
	return out
}

// answerVia says how the answer the adapter reported arrived: through the tool when it is, byte for byte once compacted, what the stand-in
// toolbox accepted from submit_answer, otherwise as the final text. The adapter takes the tool's input as the answer when it is valid (for
// OpenCode internal/adapter/opencode/opencode.go:691-694, for Pi internal/adapter/pi/pi.go:555-562), so a tool answer reaches the same scorer as a text one.
func answerVia(answer string, submitted []byte) string {
	var compact bytes.Buffer
	if len(submitted) > 0 && json.Compact(&compact, []byte(answer)) == nil && bytes.Equal(compact.Bytes(), submitted) {
		return eval.ViaTool
	}
	return eval.ViaText
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
	return tokenValue(u.TokensIn) + tokenValue(u.TokensCacheRead) + tokenValue(u.TokensCacheWrite)
}

func tokenValue(part *int64) int64 {
	if part == nil {
		return 0
	}
	return *part
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
