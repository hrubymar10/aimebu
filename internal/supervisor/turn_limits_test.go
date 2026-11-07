package supervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

func durationPtr(d time.Duration) *time.Duration { return &d }

func TestTurnLimitsFromEnvAndPerHarnessDefaults(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	none, err := TurnLimitsFromEnv(env(nil))
	if err != nil || none.Idle != nil || none.Max != nil {
		t.Fatalf("unset: %+v %v", none, err)
	}
	// Pi keeps v1's watchdog; claude/codex/fake are idle-only by default.
	if got := none.For("pi"); got != (TurnLimits{Idle: 600 * time.Second, Max: 30 * time.Minute}) {
		t.Fatalf("pi defaults: %+v", got)
	}
	for _, tool := range []string{"claude", "codex", "fake", ""} {
		if got := none.For(tool); got != (TurnLimits{Idle: 600 * time.Second}) {
			t.Fatalf("%s defaults: %+v", tool, got)
		}
	}
	got, err := TurnLimitsFromEnv(env(map[string]string{TurnIdleEnv: "90s", TurnMaxEnv: "2h"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"pi", "codex"} {
		if limits := got.For(tool); limits != (TurnLimits{Idle: 90 * time.Second, Max: 2 * time.Hour}) {
			t.Fatalf("%s override: %+v", tool, limits)
		}
	}
	// "0" disables a deadline everywhere, including pi's default cap.
	got, err = TurnLimitsFromEnv(env(map[string]string{TurnMaxEnv: "0"}))
	if err != nil || got.For("pi") != (TurnLimits{Idle: 600 * time.Second}) {
		t.Fatalf("max=0: %+v %v", got, err)
	}
	for _, bad := range []string{"soon", "-5s"} {
		if _, err := TurnLimitsFromEnv(env(map[string]string{TurnIdleEnv: bad})); err == nil || !strings.Contains(err.Error(), TurnIdleEnv) {
			t.Fatalf("%q: err=%v", bad, err)
		}
	}
}

func TestTurnWatchPolicy(t *testing.T) {
	base := time.Unix(1000, 0)
	w := newTurnWatch(TurnLimits{Idle: 10 * time.Second, Max: time.Minute}, base)
	if d, r, ok := w.next(); !ok || r != turnStallIdle || !d.Equal(base.Add(10*time.Second)) {
		t.Fatalf("initial %v %v %v", d, r, ok)
	}
	w.observe(nil, base.Add(8*time.Second))
	if d, _, _ := w.next(); !d.Equal(base.Add(18 * time.Second)) {
		t.Fatalf("output must push the idle deadline, got %v", d)
	}
	w.observe([]adapter.ToolEvent{{ID: "a"}, {ID: "b"}}, base.Add(9*time.Second))
	if d, r, _ := w.next(); r != turnStallMax || !d.Equal(base.Add(time.Minute)) {
		t.Fatalf("running tools suspend idle: %v %v", d, r)
	}
	w.observe([]adapter.ToolEvent{{ID: "a", Done: true}}, base.Add(30*time.Second))
	if _, r, _ := w.next(); r != turnStallMax {
		t.Fatalf("one tool still running: %v", r)
	}
	w.observe([]adapter.ToolEvent{{ID: "b", Done: true}, {ID: "stray", Done: true}}, base.Add(40*time.Second))
	if d, r, _ := w.next(); r != turnStallIdle || !d.Equal(base.Add(50*time.Second)) {
		t.Fatalf("idle resumes after the last tool (a stray end must not go negative): %v %v", d, r)
	}
	w.observe(nil, base.Add(55*time.Second))
	if _, r, _ := w.next(); r != turnStallMax {
		t.Fatalf("wall-clock cap wins when nearer: %v", r)
	}
	if _, _, ok := newTurnWatch(TurnLimits{}, base).next(); ok {
		t.Fatal("zero limits disable all deadlines")
	}
	// An ID-less start (harness gave no ID) pairs with an ID-less end.
	w = newTurnWatch(TurnLimits{Idle: time.Second}, base)
	w.observe([]adapter.ToolEvent{{}}, base)
	if _, _, ok := w.next(); ok {
		t.Fatal("ID-less tool start must suspend idle")
	}
	w.observe([]adapter.ToolEvent{{Done: true}}, base)
	if _, r, ok := w.next(); !ok || r != turnStallIdle {
		t.Fatal("ID-less tool end must resume idle")
	}
}

// stallWindDownBudget bounds how long a killed turn may take to finish winding down.
const stallWindDownBudget = 60 * time.Second

func stalledTurn(t *testing.T, tool string, limits TurnLimitOverrides, steps ...adapter.Step) (*Journal, *Client, string) {
	t.Helper()
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	fake := adapter.NewFake()
	fake.Script("turn-stall", steps...)
	turn, err := fake.Start(ctx, adapter.TurnSpec{TurnID: "turn-stall", Mode: adapter.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "turn-stall", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, fake)
	client.TurnLimits = limits
	reported := make(chan error, 4)
	client.OnError = func(err error) {
		if strings.Contains(err.Error(), "stalled") {
			select {
			case reported <- err:
			default:
			}
		}
	}
	client.active["agent"] = "turn-stall"
	client.running["agent"] = turn
	client.turns.Add(1)
	go client.runTurn(ctx, protocol.Delivery{AgentID: "agent", TurnID: "turn-stall", Agent: adapter.AgentSpec{Tool: tool}}, turn)
	done := make(chan struct{})
	go func() { client.turns.Wait(); close(done) }()
	// The watchdog fires at the limit (tens or hundreds of milliseconds here), but winding the killed turn down
	// (draining its events, journalling the exit) is disk and CPU work, which took seconds on a machine running
	// other suites. This wait only bounds a turn that never ends, so it is generous; what the tests prove (that
	// a stall is recorded, and as which limit) is asserted on the journal afterwards, not on how fast it happened.
	began := time.Now()
	select {
	case <-done:
	case <-time.After(stallWindDownBudget):
		// A turn that does not end in this long is either a machine starved of CPU and disk or a real hang: the goroutines say which (the stack of the
		// wind-down, or the journal write it waits on), so a failure under load leaves its own evidence.
		stacks := make([]byte, 1<<20)
		stacks = stacks[:runtime.Stack(stacks, true)]
		t.Fatalf("hung turn was never killed (waited %s); goroutines:\n%s", time.Since(began).Round(time.Millisecond), stacks)
	}
	select {
	case err := <-reported:
		return journal, client, err.Error()
	default:
		return journal, client, ""
	}
}

func assertStallRecorded(t *testing.T, journal *Journal, want string) {
	t.Helper()
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 {
		t.Fatalf("exits=%+v %v", exits, err)
	}
	exit := exits[0].Exit
	if exit.CancelIntent {
		t.Fatal("a watchdog kill must not look like an owner cancel")
	}
	if exit.TerminalEvent || exit.ExitCode == 0 {
		t.Fatalf("exit=%+v", exit)
	}
	if !strings.Contains(exit.ErrorEvent, want) || !strings.Contains(exit.StderrTail, want) {
		t.Fatalf("diagnostic missing %q: %+v", want, exit)
	}
}

func TestRunTurnKillsSilentHarness(t *testing.T) {
	journal, _, report := stalledTurn(t, "claude", TurnLimitOverrides{Idle: durationPtr(80 * time.Millisecond)}, adapter.Step{Kind: adapter.StepHang})
	assertStallRecorded(t, journal, "no output")
	if !strings.Contains(report, "stalled") {
		t.Fatalf("report=%q", report)
	}
}

func TestRunTurnWallClockCapKillsChattyHarness(t *testing.T) {
	// Output every 30ms keeps the idle deadline fresh; only the cap can fire.
	var steps []adapter.Step
	for i := 0; i < 400; i++ {
		steps = append(steps,
			adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventActivity, SourceEventID: fmt.Sprintf("tick-%d", i), Summary: "turn_start"}},
			adapter.Step{Kind: adapter.StepEffect, Effect: func(context.Context) error { time.Sleep(30 * time.Millisecond); return nil }})
	}
	journal, _, _ := stalledTurn(t, "pi", TurnLimitOverrides{Idle: durationPtr(time.Second), Max: durationPtr(150 * time.Millisecond)}, steps...)
	assertStallRecorded(t, journal, "wall-clock")
}

func TestRunTurnWithoutLimitsLeavesCleanExitAlone(t *testing.T) {
	journal, _, report := stalledTurn(t, "codex", TurnLimitOverrides{Idle: durationPtr(time.Second), Max: durationPtr(time.Minute)},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{ExitCode: 0, TerminalEvent: true}})
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 || exits[0].Exit.ErrorEvent != "" || report != "" {
		t.Fatalf("exits=%+v report=%q %v", exits, report, err)
	}
}

// A harness that has announced a running tool and then goes silent (Codex
// and Claude emit nothing while a long command runs) must survive the idle
// deadline; only the wall-clock cap, when one applies, ends it.
func TestRunTurnSilentToolSuspendsIdleKill(t *testing.T) {
	for _, tool := range []string{"codex", "claude", "pi"} {
		t.Run(tool, func(t *testing.T) {
			started := adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventActivity, SourceEventID: "tool-start", Summary: "tool", Tools: []adapter.ToolEvent{{ID: "cmd"}}}}
			journal, _, _ := stalledTurn(t, tool, TurnLimitOverrides{Idle: durationPtr(60 * time.Millisecond), Max: durationPtr(400 * time.Millisecond)}, started, adapter.Step{Kind: adapter.StepHang})
			exits, err := journal.PendingExits(context.Background())
			if err != nil || len(exits) != 1 {
				t.Fatalf("exits=%+v %v", exits, err)
			}
			if !strings.Contains(exits[0].Exit.ErrorEvent, "wall-clock") {
				t.Fatalf("a running tool must defer the idle kill to the wall-clock cap, got %q", exits[0].Exit.ErrorEvent)
			}
		})
	}
}

func TestRunTurnClaudeAndCodexHaveNoDefaultWallClockCap(t *testing.T) {
	// Defaults only (no override): a chatty claude turn is never capped.
	var steps []adapter.Step
	for i := 0; i < 10; i++ {
		steps = append(steps,
			adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventActivity, SourceEventID: fmt.Sprintf("t-%d", i), Summary: "assistant"}},
			adapter.Step{Kind: adapter.StepEffect, Effect: func(context.Context) error { time.Sleep(10 * time.Millisecond); return nil }})
	}
	steps = append(steps, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	journal, _, report := stalledTurn(t, "claude", TurnLimitOverrides{}, steps...)
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 || exits[0].Exit.ErrorEvent != "" || report != "" {
		t.Fatalf("exits=%+v report=%q %v", exits, report, err)
	}
	if got := (TurnLimitOverrides{}).For("claude").Max; got != 0 {
		t.Fatalf("claude max default = %s", got)
	}
}

// streamingTurn emits no adapter events but reports raw stdout liveness, like
// a harness streaming one long generation.
type streamingTurn struct {
	events chan adapter.Event
	last   atomic.Int64
	done   chan struct{}
	once   sync.Once
}

func (t *streamingTurn) Events() <-chan adapter.Event { return t.events }
func (t *streamingTurn) LastOutput() time.Time        { return time.Unix(0, t.last.Load()) }
func (t *streamingTurn) Cancel() error {
	t.once.Do(func() { close(t.events); close(t.done) })
	return nil
}
func (t *streamingTurn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	return adapter.ExitInfo{ExitCode: 137}, nil
}

func TestRunTurnRawOutputCountsAsIdleLiveness(t *testing.T) {
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "turn-stream", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	client.TurnLimits = TurnLimitOverrides{Idle: durationPtr(120 * time.Millisecond), Max: durationPtr(0)}
	turn := &streamingTurn{events: make(chan adapter.Event), done: make(chan struct{})}
	client.active["agent"] = "turn-stream"
	client.running["agent"] = turn
	client.turns.Add(1)
	go client.runTurn(ctx, protocol.Delivery{AgentID: "agent", TurnID: "turn-stream"}, turn)
	finished := make(chan struct{})
	go func() { client.turns.Wait(); close(finished) }()
	// Keep writing for several idle periods: the turn must survive.
	for i := 0; i < 12; i++ {
		turn.last.Store(time.Now().UnixNano())
		select {
		case <-finished:
			t.Fatal("a turn that is still writing output was killed as idle")
		case <-time.After(40 * time.Millisecond):
		}
	}
	// Output stops: now it is a stall.
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("silent turn was never killed")
	}
	exits, err := journal.PendingExits(ctx)
	if err != nil || len(exits) != 1 || !strings.Contains(exits[0].Exit.ErrorEvent, "no output") {
		t.Fatalf("exits=%+v %v", exits, err)
	}
}
