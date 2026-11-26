package store

import (
	"context"
	"errors"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// A process exit for a turn the server has already ended in the OTHER state is refused with ErrAlreadyTerminal, not the general
// invalid_transition: the turn cannot change any more, so a supervisor's journal may settle the refused exit instead of resending it
// forever (the invalid_transition variant of the A14-4 drain bug). The turn's recorded state is left exactly as it was.
func TestTerminalMismatchExitIsAlreadyTerminalNotInvalidTransition(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		first, second adapter.ExitInfo
		wantExec      string
	}{
		"the server killed the turn, the journal reports an exit": {adapter.ExitInfo{ExitCode: 137, CancelIntent: true}, adapter.ExitInfo{ExitCode: 0}, "killed"},
		"the server ended the turn, the journal reports a kill":   {adapter.ExitInfo{ExitCode: 0}, adapter.ExitInfo{ExitCode: 137, CancelIntent: true}, "exited"},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			ctx := context.Background()
			if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, f.turn.Generation, tc.first); err != nil {
				t.Fatal(err)
			}
			err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, f.turn.Generation, tc.second)
			if !errors.Is(err, ErrAlreadyTerminal) {
				t.Fatalf("the second exit is err=%v, want already_terminal", err)
			}
			if errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("the terminal mismatch still reports the general invalid_transition: %v", err)
			}
			var exec string
			if err := s.db.QueryRow(`SELECT exec FROM turn WHERE id=?`, f.turn.ID).Scan(&exec); err != nil || exec != tc.wantExec {
				t.Fatalf("the refused exit changed the turn: exec=%q %v", exec, err)
			}
		})
	}
}

// A plain out-of-order transition keeps the general code: a refused answer (wrong revision) of a turn that may still be running is
// invalid_transition, and nothing about it says the turn is over.
func TestAnOutOfOrderAnswerStaysInvalidTransition(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	_, err := s.ApplyAnswer(context.Background(), AnswerInput{TurnID: f.turn.ID, AgentID: f.agent.ID, Generation: f.turn.Generation, AnswerRev: 1, Digest: "d", Candidate: []byte(`{}`)})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("the wrong-revision answer is err=%v, want invalid_transition", err)
	}
}
