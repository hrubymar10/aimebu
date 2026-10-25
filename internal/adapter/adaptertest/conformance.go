// Package adaptertest runs the shared behavioral contract against adapter
// fixtures. Future CLI adapters provide the same four scenarios via their
// test launchers; the fake uses in-process scripts.
package adaptertest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

type Scenario string

const (
	ScenarioEvents           Scenario = "events"
	ScenarioNoTerminal       Scenario = "no_terminal"
	ScenarioCancel           Scenario = "cancel"
	ScenarioCrashAfterEffect Scenario = "crash_after_effect"
)

// Fixture creates a fresh adapter and turn input for each scenario. The
// effect channel is only used by ScenarioCrashAfterEffect.
type Fixture func(t *testing.T, scenario Scenario) (adapter.Adapter, adapter.TurnSpec, <-chan struct{})

// Option adds checks that only make sense for some adapters.
type Option func(*config)

type config struct{ process bool }

// ChildProcess marks an adapter that runs a harness as a child process and tracks its reap with adapter.Reaper: Run then also cancels in
// the gap between the process being reaped and the turn reporting its exit.
func ChildProcess() Option { return func(c *config) { c.process = true } }

func Run(t *testing.T, fixture Fixture, options ...Option) {
	var settings config
	for _, option := range options {
		option(&settings)
	}
	runScenarios(t, fixture)
	if settings.process {
		t.Run("cancel_between_reap_and_exit", func(t *testing.T) { cancelInTheReapWindow(t, fixture) })
	}
}

func runScenarios(t *testing.T, fixture Fixture) {
	for _, scenario := range []Scenario{ScenarioEvents, ScenarioNoTerminal, ScenarioCancel, ScenarioCrashAfterEffect} {
		t.Run(string(scenario), func(t *testing.T) {
			implementation, spec, effected := fixture(t, scenario)
			// Collect and Wait observe actual process completion. A subprocess can
			// take several seconds to start under the race detector and concurrent
			// suites, so keep a generous bound for a genuinely stuck adapter.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			turn, err := implementation.Start(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == ScenarioCancel {
				if err := turn.Cancel(); err != nil {
					t.Fatal(err)
				}
				if err := turn.Cancel(); err != nil {
					t.Fatalf("repeated cancel: %v", err)
				}
			}
			if scenario == ScenarioCrashAfterEffect {
				select {
				case <-effected:
				case <-ctx.Done():
					t.Fatal("effect did not run")
				}
			}
			events := collect(t, ctx, turn)
			behavior := make([]adapter.Event, 0, len(events))
			for _, event := range events {
				if event.Kind != adapter.EventNativeSessionStarted {
					behavior = append(behavior, event)
				}
			}
			exit, err := turn.Wait()
			if err != nil {
				t.Fatal(err)
			}
			again, err := turn.Wait()
			if err != nil || again != exit {
				t.Fatalf("Wait not repeatable: %+v / %+v, %v", exit, again, err)
			}
			assertUniqueAndExit(t, events, exit)
			// The process is gone and reaped: cancelling a finished turn is a no-op, however the group answers a signal (Linux says ESRCH,
			// macOS may say EPERM). The supervisor cancels on its own deadlines and may do so as a turn ends.
			if err := turn.Cancel(); err != nil {
				t.Fatalf("cancel after exit: %v", err)
			}
			if again, err := turn.Wait(); err != nil || again != exit {
				t.Fatalf("a cancel after exit changed the result: %+v / %+v, %v", exit, again, err)
			}
			switch scenario {
			case ScenarioEvents:
				if len(behavior) < 4 || len(behavior) > 5 {
					t.Fatalf("events=%+v", behavior)
				}
				if behavior[0].Kind != adapter.EventActivity || behavior[1].Kind != adapter.EventUsage {
					t.Fatalf("activity/usage order: %+v", behavior)
				}
				answerIndex := 2
				if len(behavior) == 5 {
					if behavior[2].Kind != adapter.EventCompaction {
						t.Fatalf("compaction order: %+v", behavior)
					}
					answerIndex = 3
				}
				if behavior[answerIndex].Kind != adapter.EventFinalAnswer {
					t.Fatalf("final answer order: %+v", behavior)
				}
				var candidate struct {
					Messages []json.RawMessage `json:"messages"`
				}
				if err := json.Unmarshal(behavior[answerIndex].Answer, &candidate); err != nil || candidate.Messages == nil {
					t.Fatalf("final candidate=%q, %v", behavior[answerIndex].Answer, err)
				}
				if !exit.TerminalEvent || exit.ExitCode != 0 {
					t.Fatalf("successful exit=%+v", exit)
				}
			case ScenarioNoTerminal:
				if exit.ExitCode != 0 || exit.TerminalEvent {
					t.Fatalf("exit zero incorrectly implies completion: %+v", exit)
				}
				for _, event := range events {
					if event.Kind == adapter.EventFinalAnswer {
						t.Fatal("unexpected final answer")
					}
				}
			case ScenarioCancel:
				if !exit.CancelIntent || exit.TerminalEvent {
					t.Fatalf("cancel evidence=%+v", exit)
				}
			case ScenarioCrashAfterEffect:
				if exit.ExitCode == 0 || exit.TerminalEvent || exit.CancelIntent {
					t.Fatalf("crash evidence=%+v", exit)
				}
				for _, event := range events {
					if event.Kind == adapter.EventActivity || event.Kind == adapter.EventFinalAnswer {
						t.Fatalf("delayed event survived crash: %+v", event)
					}
				}
			}
		})
	}
}

func collect(t *testing.T, ctx context.Context, turn adapter.Turn) []adapter.Event {
	t.Helper()
	done := make(chan []adapter.Event, 1)
	go func() {
		events := []adapter.Event{}
		for event := range turn.Events() {
			events = append(events, event)
		}
		done <- events
	}()
	select {
	case events := <-done:
		return events
	case <-ctx.Done():
		_ = turn.Cancel()
		t.Fatalf("event channel did not close: %v", ctx.Err())
		return nil
	}
}

func assertUniqueAndExit(t *testing.T, events []adapter.Event, exit adapter.ExitInfo) {
	t.Helper()
	if len(events) == 0 || events[len(events)-1].Kind != adapter.EventProcessExit {
		t.Fatalf("missing process exit: %+v", events)
	}
	seen := map[string]bool{}
	for _, event := range events {
		if event.SourceEventID == "" || seen[event.SourceEventID] {
			t.Fatalf("bad source_event_id %q", event.SourceEventID)
		}
		seen[event.SourceEventID] = true
	}
	if got := events[len(events)-1].Exit; got == nil || *got != exit {
		t.Fatalf("exit event=%+v Wait=%+v", got, exit)
	}
}

// cancelInTheReapWindow holds a finished turn right after its process is reaped, before it reports the exit, and cancels it there. The
// process is gone: nothing may be signalled (its group id could already be someone else's) and no cancel intent is recorded, so the turn
// ends as the successful one it was.
func cancelInTheReapWindow(t *testing.T, fixture Fixture) {
	implementation, spec, _ := fixture(t, ScenarioEvents)
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	restore := adapter.HoldAfterReap(func() {
		once.Do(func() { close(reached) })
		<-release
	})
	defer restore()
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	defer letGo()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	turn, err := implementation.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("the process was not reaped")
	}
	if err := turn.Cancel(); err != nil {
		t.Fatalf("cancel between reap and exit: %v", err)
	}
	letGo()
	events := collect(t, ctx, turn)
	exit, err := turn.Wait()
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueAndExit(t, events, exit)
	if exit.CancelIntent || !exit.TerminalEvent || exit.ExitCode != 0 {
		t.Fatalf("a cancel after the process ended must not change how the turn ended: %+v", exit)
	}
}
