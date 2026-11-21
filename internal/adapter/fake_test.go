package adapter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/adaptertest"
)

func TestFakeConformance(t *testing.T) {
	adaptertest.Run(t, func(t *testing.T, scenario adaptertest.Scenario) (adapter.Adapter, adapter.TurnSpec, <-chan struct{}) {
		fake := adapter.NewFake()
		spec := adapter.TurnSpec{TurnID: "turn-" + string(scenario), Mode: adapter.ModeNormal, Agent: adapter.AgentSpec{ID: "agent-1", Tool: "fake"}, NotesJSON: json.RawMessage(`[]`)}
		var effected chan struct{}
		switch scenario {
		case adaptertest.ScenarioEvents:
			fake.Script(spec.TurnID,
				adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventActivity, Summary: "tool started"}},
				adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{}}},
				adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventCompaction, Compaction: &adapter.Compaction{PreTokens: 100, PostTokens: 20}}},
				adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}},
				adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{ExitCode: 0, TerminalEvent: true}})
		case adaptertest.ScenarioNoTerminal:
			fake.Script(spec.TurnID, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{ExitCode: 0}})
		case adaptertest.ScenarioCancel:
			fake.Script(spec.TurnID, adapter.Step{Kind: adapter.StepHang})
		case adaptertest.ScenarioCrashAfterEffect:
			effected = make(chan struct{})
			fake.Script(spec.TurnID,
				adapter.Step{Kind: adapter.StepEffect, Effect: func(context.Context) error { close(effected); return nil }},
				adapter.Step{Kind: adapter.StepCrash},
				adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventActivity, Summary: "delayed tool event"}})
		}
		return fake, spec, effected
	})
}

func TestFakeFileEffectBeforeCrashAndMalformedAnswer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effect.log")
	fake := adapter.NewFake()
	spec := adapter.TurnSpec{TurnID: "effect-turn", Mode: adapter.ModeNormal}
	fake.Script(spec.TurnID,
		adapter.Step{Kind: adapter.StepEffect, FilePath: path, Append: "side effect\n"},
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[`)}},
		adapter.Step{Kind: adapter.StepCrash})
	turn, err := fake.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	events := []adapter.Event{}
	for event := range turn.Events() {
		events = append(events, event)
	}
	exit, err := turn.Wait()
	if err != nil || exit.ExitCode == 0 || exit.TerminalEvent {
		t.Fatalf("crash: %+v %v", exit, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "side effect\n" {
		t.Fatalf("effect: %q %v", data, err)
	}
	if len(events) != 2 || string(events[0].Answer) != `{"messages":[` || json.Valid(events[0].Answer) {
		t.Fatalf("candidate changed: %+v", events)
	}
	if _, err := fake.Start(context.Background(), spec); !errors.Is(err, adapter.ErrTurnAlreadyStarted) {
		t.Fatalf("repeated start: %v", err)
	}
}

func TestCapabilityVersionScope(t *testing.T) {
	fake := adapter.NewFake()
	caps := fake.Capabilities("v1", adapter.Environment{Variant: "native"})
	if caps.ReadonlyVerified || !caps.SchemaFlag || !caps.NoActionMode || !caps.CompactionEvents || !caps.ContextWindow || !caps.Cost || !caps.CancelConfirmed {
		t.Fatalf("default fake capabilities: %+v", caps)
	}
	fake.SetReadonlyVerified(true)
	if !fake.Capabilities("v1", adapter.Environment{}).ReadonlyVerified {
		t.Fatal("read-only fixture flag ignored")
	}
	registry := adapter.NewRegistry()
	key := adapter.CapabilityKey{Tool: "codex", Version: "0.162.1", Environment: adapter.Environment{Variant: "native", OS: "darwin"}}
	registry.Register(key, adapter.Capabilities{SchemaFlag: true})
	if got, ok := registry.Lookup(key); !ok || !got.SchemaFlag {
		t.Fatalf("registered capability: %+v %v", got, ok)
	}
	key.Version = "0.162.2"
	if _, ok := registry.Lookup(key); ok {
		t.Fatal("capability inherited by unverified upgrade")
	}
	key.Version = "0.162.1"
	key.Environment.Container = true
	if _, ok := registry.Lookup(key); ok {
		t.Fatal("capability inherited by different environment")
	}
}
