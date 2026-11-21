package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// runScripted runs one normal turn of scripted steps to its end and returns the journal that recorded it.
func runScripted(t *testing.T, steps ...adapter.Step) *Journal {
	t.Helper()
	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	fake := adapter.NewFake()
	fake.Script("turn-cut", steps...)
	turn, err := fake.Start(ctx, adapter.TurnSpec{TurnID: "turn-cut", Mode: adapter.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: "turn-cut", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, fake)
	client.active["agent"] = "turn-cut"
	client.running["agent"] = turn
	client.turns.Add(1)
	go client.runTurn(ctx, protocol.Delivery{AgentID: "agent", TurnID: "turn-cut", Mode: adapter.ModeNormal, Agent: adapter.AgentSpec{Tool: "claude"}}, turn)
	done := make(chan struct{})
	go func() { client.turns.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("turn never ended")
	}
	return journal
}

func answerStep(raw string) adapter.Step {
	return adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, SourceEventID: "answer", Answer: []byte(raw)}}
}

func pendingExit(t *testing.T, journal *Journal) adapter.ExitInfo {
	t.Helper()
	exits, err := journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 {
		t.Fatalf("exits=%+v %v", exits, err)
	}
	return exits[0].Exit
}

func recordedAnswers(t *testing.T, journal *Journal) int {
	t.Helper()
	answers, err := journal.PendingAnswers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(answers)
}

// A process killed by a signal that leaves nothing but an empty answer was interrupted: no answer reaches the server (it would be held
// as invalid_answer: EOF), and the exit is marked with how it ended.
func TestEmptyAnswerOfAKilledProcessIsAnInterruption(t *testing.T) {
	for name, code := range map[string]int{"sigkill 137": 137, "sigterm 143": 143, "signalled -1": -1} {
		t.Run(name, func(t *testing.T) {
			journal := runScripted(t, answerStep(""), adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{ExitCode: code, TerminalEvent: true}})
			exit := pendingExit(t, journal)
			if !exit.Interrupted || exit.TerminalEvent || exit.CancelIntent || exit.LimitStall {
				t.Fatalf("exit=%+v", exit)
			}
			if !strings.Contains(exit.ErrorEvent, "harness process") {
				t.Fatalf("the reason must say what happened: %q", exit.ErrorEvent)
			}
			if n := recordedAnswers(t, journal); n != 0 {
				t.Fatalf("an interrupted turn must not send its empty answer: %d recorded", n)
			}
		})
	}
}

// With no answer at all and the process killed, the turn is interrupted too.
func TestKilledProcessWithoutOutputIsAnInterruption(t *testing.T) {
	journal := runScripted(t, adapter.Step{Kind: adapter.StepCrash})
	exit := pendingExit(t, journal)
	if !exit.Interrupted || !strings.Contains(exit.ErrorEvent, "signal 9") || !strings.Contains(exit.ErrorEvent, "137") {
		t.Fatalf("exit=%+v", exit)
	}
}

// A genuinely invalid answer stays as it is: it is sent, the exit is not marked.
func TestInvalidAnswerIsNotAnInterruption(t *testing.T) {
	for name, code := range map[string]int{"clean exit": 0, "killed afterwards": 137} {
		t.Run(name, func(t *testing.T) {
			journal := runScripted(t, answerStep("{not json"), adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{ExitCode: code, TerminalEvent: true}})
			exit := pendingExit(t, journal)
			if exit.Interrupted || !exit.TerminalEvent {
				t.Fatalf("exit=%+v", exit)
			}
			if n := recordedAnswers(t, journal); n != 1 {
				t.Fatalf("the invalid answer must still be sent: %d", n)
			}
		})
	}
}

// An empty answer from a process that exited cleanly keeps today's path: the server holds it as invalid, nothing was cut off.
func TestEmptyAnswerOfACleanExitStillGoesToTheServer(t *testing.T) {
	journal := runScripted(t, answerStep(""), adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	exit := pendingExit(t, journal)
	if exit.Interrupted || !exit.TerminalEvent {
		t.Fatalf("exit=%+v", exit)
	}
	if n := recordedAnswers(t, journal); n != 1 {
		t.Fatalf("answers=%d", n)
	}
}

func TestInterruptedReasonLeavesExplainedExitsAlone(t *testing.T) {
	cases := map[string]struct {
		exit adapter.ExitInfo
		want bool
	}{
		"terminal":          {adapter.ExitInfo{ExitCode: 137, TerminalEvent: true}, false},
		"owner cancel":      {adapter.ExitInfo{ExitCode: 137, CancelIntent: true}, false},
		"start failure":     {adapter.ExitInfo{ExitCode: 1, StartFailure: true}, false},
		"error result":      {adapter.ExitInfo{ExitCode: 1, ErrorEvent: "boom"}, false},
		"definite failure":  {adapter.ExitInfo{ExitCode: 1, DefiniteFailure: true}, false},
		"limit kill":        {adapter.ExitInfo{ExitCode: 124, LimitStall: true}, false},
		"stderr and code 1": {adapter.ExitInfo{ExitCode: 1, StderrTail: "401 unauthorized"}, false},
		"sigkill":           {adapter.ExitInfo{ExitCode: 137, StderrTail: "Killed"}, true},
		"silent code 1":     {adapter.ExitInfo{ExitCode: 1}, true},
		"silent code 0":     {adapter.ExitInfo{}, true},
	}
	for name, tc := range cases {
		if _, got := interruptedReason(tc.exit); got != tc.want {
			t.Errorf("%s: got %t want %t", name, got, tc.want)
		}
	}
}
