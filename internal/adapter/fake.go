package adapter

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/goccy/go-json"
)

type StepKind string

const (
	StepEmit   StepKind = "emit"
	StepEffect StepKind = "effect"
	StepGate   StepKind = "gate"
	StepHang   StepKind = "hang"
	StepExit   StepKind = "exit"
	StepCrash  StepKind = "crash"
)

// StepEffect can append to FilePath, call Effect, or both. Place an effect
// before its activity event and follow it with StepCrash to model a lost tool
// event after a real side effect.
type Step struct {
	Kind  StepKind
	Event Event
	// AnswerFunc lets a scripted turn form its final answer from an earlier
	// StepEffect (for example, a room ID returned by a toolbox call).
	AnswerFunc func() json.RawMessage
	FilePath   string
	Append     string
	Effect     func(context.Context) error
	Gate       <-chan struct{}
	Exit       ExitInfo
}

type Fake struct {
	mu               sync.Mutex
	scripts          map[string][]Step
	defaultScript    []Step
	started          map[string]bool
	readonlyVerified bool
	echoDefault      bool
}

func NewFake() *Fake { return &Fake{scripts: make(map[string][]Step), started: make(map[string]bool)} }
func (f *Fake) Catalog() []CatalogEntry {
	return []CatalogEntry{{Tool: "fake", Version: "test", Environment: Environment{Variant: "test"}}}
}
func (f *Fake) SetEchoDefault(value bool) { f.mu.Lock(); f.echoDefault = value; f.mu.Unlock() }
func (f *Fake) SetReadonlyVerified(value bool) {
	f.mu.Lock()
	f.readonlyVerified = value
	f.mu.Unlock()
}
func (f *Fake) Capabilities(_ string, _ Environment) Capabilities {
	f.mu.Lock()
	readonly := f.readonlyVerified
	f.mu.Unlock()
	return Capabilities{SchemaFlag: true, NoActionMode: true, ReadonlyVerified: readonly, CompactionEvents: true, ContextWindow: true, Cost: true, CancelConfirmed: true}
}

// Script installs one script for a future turn. Start consumes it exactly
// once so repeated turn IDs cannot silently repeat side effects.
func (f *Fake) Script(turnID string, steps ...Step) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[turnID] = append([]Step(nil), steps...)
}

// ScriptDefault supplies the same script to future turn IDs without an exact
// script. It is useful when the dispatcher allocates a turn during a test.
func (f *Fake) ScriptDefault(steps ...Step) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultScript = append([]Step(nil), steps...)
}

func (f *Fake) Start(parent context.Context, spec TurnSpec) (Turn, error) {
	if spec.TurnID == "" {
		return nil, fmt.Errorf("turn ID required")
	}
	f.mu.Lock()
	if f.started[spec.TurnID] {
		f.mu.Unlock()
		return nil, ErrTurnAlreadyStarted
	}
	steps, ok := f.scripts[spec.TurnID]
	if !ok && len(f.defaultScript) > 0 {
		steps = append([]Step(nil), f.defaultScript...)
		ok = true
	}
	if !ok && f.echoDefault && spec.Mode == ModeNormal {
		var notes []struct {
			RoomID    string `json:"room_id"`
			Body      string `json:"body"`
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(spec.NotesJSON, &notes); err != nil {
			f.mu.Unlock()
			return nil, err
		}
		if len(notes) > 0 {
			body := strings.TrimSpace(notes[len(notes)-1].Body)
			if len(body) > 200 {
				body = body[:200]
			}
			answer, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"room": notes[len(notes)-1].RoomID, "body": "I heard: " + body, "reply_to": notes[len(notes)-1].MessageID}}})
			steps = []Step{{Kind: StepEmit, Event: Event{Kind: EventFinalAnswer, Answer: answer}}, {Kind: StepExit, Exit: ExitInfo{TerminalEvent: true}}}
			ok = true
		}
	}
	if ok {
		delete(f.scripts, spec.TurnID)
		f.started[spec.TurnID] = true
	}
	f.mu.Unlock()
	if !ok {
		return nil, ErrNoScript
	}
	seen := map[string]bool{}
	eventIndex := 0
	for i := range steps {
		if steps[i].Kind != StepEmit {
			continue
		}
		if steps[i].Event.Kind == EventProcessExit {
			return nil, fmt.Errorf("process_exit is emitted by fake")
		}
		if steps[i].Event.SourceEventID == "" {
			steps[i].Event.SourceEventID = fmt.Sprintf("%s:%04d", spec.TurnID, eventIndex)
		}
		if seen[steps[i].Event.SourceEventID] {
			return nil, fmt.Errorf("duplicate source_event_id %q", steps[i].Event.SourceEventID)
		}
		seen[steps[i].Event.SourceEventID] = true
		eventIndex++
	}
	exitID := spec.TurnID + ":exit"
	if seen[exitID] {
		return nil, fmt.Errorf("duplicate source_event_id %q", exitID)
	}
	ctx, cancel := context.WithCancel(parent)
	t := &fakeTurn{events: make(chan Event, len(steps)+1), done: make(chan struct{}), cancel: cancel}
	go t.run(ctx, steps, exitID)
	return t, nil
}

type fakeTurn struct {
	mu           sync.Mutex
	events       chan Event
	done         chan struct{}
	cancel       context.CancelFunc
	cancelIntent bool
	finished     bool
	exit         ExitInfo
	err          error
}

func (t *fakeTurn) Events() <-chan Event { return t.events }
func (t *fakeTurn) Cancel() error {
	t.mu.Lock()
	if !t.finished {
		t.cancelIntent = true
		t.cancel()
	}
	t.mu.Unlock()
	return nil
}
func (t *fakeTurn) Wait() (ExitInfo, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit, t.err
}

func (t *fakeTurn) run(ctx context.Context, steps []Step, exitID string) {
	exit := ExitInfo{ExitCode: 0}
	var runErr error
	defer func() {
		t.mu.Lock()
		if t.cancelIntent {
			exit.CancelIntent = true
		}
		t.exit, t.err = exit, runErr
		t.finished = true
		t.mu.Unlock()
		t.events <- Event{Kind: EventProcessExit, SourceEventID: exitID, Exit: &exit}
		close(t.events)
		close(t.done)
		t.cancel()
	}()
	for _, step := range steps {
		if ctx.Err() != nil {
			exit.ExitCode = 130
			return
		}
		switch step.Kind {
		case StepEmit:
			event := step.Event
			if step.AnswerFunc != nil {
				event.Answer = step.AnswerFunc()
			}
			t.events <- event
		case StepEffect:
			if step.FilePath != "" {
				file, err := os.OpenFile(step.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					exit.ExitCode = 1
					runErr = err
					return
				}
				_, err = file.WriteString(step.Append)
				closeErr := file.Close()
				if err == nil {
					err = closeErr
				}
				if err != nil {
					exit.ExitCode = 1
					runErr = err
					return
				}
			}
			if step.Effect != nil {
				if err := step.Effect(ctx); err != nil {
					exit.ExitCode = 1
					runErr = err
					return
				}
			}
		case StepGate:
			select {
			case <-ctx.Done():
				exit.ExitCode = 130
				return
			case <-step.Gate:
			}
		case StepHang:
			<-ctx.Done()
			exit.ExitCode = 130
			return
		case StepExit:
			exit = step.Exit
			return
		case StepCrash:
			exit = step.Exit
			if exit.ExitCode == 0 {
				exit.ExitCode = 137
			}
			exit.TerminalEvent = false
			return
		default:
			exit.ExitCode = 1
			runErr = fmt.Errorf("unknown fake step %q", step.Kind)
			return
		}
	}
}
