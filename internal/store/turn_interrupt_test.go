package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// What the supervisor sends for a turn it cut on a turn_interrupt frame.
var requestedInterruptExit = adapter.ExitInfo{ExitCode: 130, ErrorEvent: "the turn was interrupted on request", Interrupted: true}

func requestInterrupt(t *testing.T, s *Store, turnID, disposition string) TurnInterruptTarget {
	t.Helper()
	target, err := s.RequestTurnInterrupt(context.Background(), TurnInterruptRequest{TurnID: turnID, ActorKind: "user", ActorID: "usr_recovery", Purpose: "test", Disposition: disposition})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestTurnInterruptRequestIsRecordedForARunningOrdinaryTurn(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	for name, request := range map[string]TurnInterruptRequest{
		"unknown disposition": {TurnID: f.turn.ID, ActorKind: "user", ActorID: "usr_recovery", Purpose: "test", Disposition: "drop"},
		"no purpose":          {TurnID: f.turn.ID, ActorKind: "user", ActorID: "usr_recovery", Disposition: TurnInterruptRequeue},
		"no actor":            {TurnID: f.turn.ID, ActorKind: "user", Purpose: "test", Disposition: TurnInterruptRequeue},
		"unknown turn":        {TurnID: "trn_missing", ActorKind: "user", ActorID: "usr_recovery", Purpose: "test", Disposition: TurnInterruptRequeue},
	} {
		if _, err := s.RequestTurnInterrupt(ctx, request); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	target := requestInterrupt(t, s, f.turn.ID, TurnInterruptRequeue)
	if target.TurnID != f.turn.ID || target.AgentID != f.agent.ID || target.MachineID != f.agent.MachineID || target.Generation != 0 {
		t.Fatalf("target=%+v", target)
	}
	var payload, desired string
	if err := s.db.QueryRow(`SELECT payload_json FROM event WHERE turn_id=? AND kind='turn_interrupt_requested'`, f.turn.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"actor":"usr_recovery"`, `"purpose":"test"`, `"disposition":"requeue"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("event payload %s lacks %s", payload, want)
		}
	}
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, f.agent.ID).Scan(&desired); err != nil || desired != "active" {
		t.Fatalf("the agent's desired state changed: %q %v", desired, err)
	}
	// A turn that has ended is not interrupted.
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, requestedInterruptExit); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestTurnInterrupt(ctx, TurnInterruptRequest{TurnID: f.turn.ID, ActorKind: "user", ActorID: "usr_recovery", Purpose: "test", Disposition: TurnInterruptRequeue}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an ended turn: %v", err)
	}
}

// Requeue: the notes go back to queued, with a caution once the turn had worked; no turn is made, nothing blocks the agent, no card is raised.
func TestRequeuedInterruptPutsTheNotesBack(t *testing.T) {
	t.Parallel()
	for name, worked := range map[string]bool{"before any work": false, "after work": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, f, owner := recoveryFixture(t, false)
			ctx := context.Background()
			if worked {
				if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "src-1", "activity", "ran a command", []byte(`{"kind":"activity","source_event_id":"src-1","summary":"ran a command"}`)); err != nil {
					t.Fatal(err)
				}
			}
			requestInterrupt(t, s, f.turn.ID, TurnInterruptRequeue)
			if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, requestedInterruptExit); err != nil {
				t.Fatal(err)
			}
			var exec, outcome, settle, noteState, observed, exitPayload string
			var block, children, cautions int
			if err := s.db.QueryRow(`SELECT exec,outcome,settle FROM turn WHERE id=?`, f.turn.ID).Scan(&exec, &outcome, &settle); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&noteState); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT observed,recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&observed, &block); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM turn WHERE parent_turn_id=?`, f.turn.ID).Scan(&children); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM provider_limit_retry WHERE note_id=? AND caution=1`, f.note.ID).Scan(&cautions); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT payload_json FROM event WHERE turn_id=? AND kind='turn_process_exit'`, f.turn.ID).Scan(&exitPayload); err != nil {
				t.Fatal(err)
			}
			if exec != "exited" || outcome != "cancelled" || settle != "settled" || noteState != "queued" || observed != "idle" || block != 0 || children != 0 {
				t.Fatalf("exec=%s outcome=%s settle=%s note=%s observed=%s block=%d children=%d", exec, outcome, settle, noteState, observed, block, children)
			}
			if (cautions == 1) != worked {
				t.Fatalf("cautions=%d worked=%t", cautions, worked)
			}
			if !strings.Contains(exitPayload, `"interrupt":"requeue"`) {
				t.Fatalf("exit event payload=%s", exitPayload)
			}
			if items, err := s.InboxForUser(ctx, owner); err != nil || len(items) != 0 {
				t.Fatalf("a requeued interrupt must raise no card: %+v %v", items, err)
			}
			delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
			if err != nil || !ready || len(delivery.Notes) != 1 || delivery.Notes[0].NoteID != f.note.ID || (delivery.Notes[0].RetryNotice != "") != worked {
				t.Fatalf("next dispatch ready=%t notes=%+v err=%v", ready, delivery.Notes, err)
			}
		})
	}
}

// Continue: the turn is continued on its own, and that does not use up the chain's one automatic continuation: the continuation, cut off by
// itself later, is still continued once, and a second requested interrupt of the chain is continued too.
func TestRequestedContinuationIsOutsideTheChainLimit(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	child := func(parent string) string {
		t.Helper()
		var id string
		if err := s.db.QueryRow(`SELECT id FROM turn WHERE parent_turn_id=? AND mode='normal'`, parent).Scan(&id); err != nil {
			t.Fatalf("no continuation of %s: %v", parent, err)
		}
		if _, err := s.db.Exec(`UPDATE turn SET exec='running',started_at=? WHERE id=?`, timestamp(), id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	requestInterrupt(t, s, f.turn.ID, TurnInterruptContinue)
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, requestedInterruptExit); err != nil {
		t.Fatal(err)
	}
	first := child(f.turn.ID)
	var payload, body string
	if err := s.db.QueryRow(`SELECT payload_json FROM event WHERE turn_id=? AND kind='turn_interrupted_continuation'`, first).Scan(&payload); err != nil || !strings.Contains(payload, `"requested":"1"`) {
		t.Fatalf("continuation event payload=%s err=%v", payload, err)
	}
	if err := s.db.QueryRow(`SELECT m.body FROM note n JOIN message m ON m.id=n.message_id WHERE n.agent_id=? AND n.wake_reason='turn_interrupted'`, f.agent.ID).Scan(&body); err != nil || !strings.Contains(body, "interrupted on request") {
		t.Fatalf("continuation note=%q err=%v", body, err)
	}
	// A second requested interrupt, now of the continuation, continues again.
	requestInterrupt(t, s, first, TurnInterruptContinue)
	if err := s.RecordProcessInfo(ctx, first, f.agent.ID, 0, requestedInterruptExit); err != nil {
		t.Fatal(err)
	}
	second := child(first)
	// The chain's own allowance is untouched: a cut-off nobody asked for is continued once...
	if err := s.RecordProcessInfo(ctx, second, f.agent.ID, 0, interruptedExit); err != nil {
		t.Fatal(err)
	}
	third := child(second)
	if items, err := s.InboxForUser(ctx, owner); err != nil || len(items) != 0 {
		t.Fatalf("no card is due yet: %+v %v", items, err)
	}
	// ...and its repeat raises the Recovery card, as before.
	if err := s.RecordProcessInfo(ctx, third, f.agent.ID, 0, interruptedExit); err != nil {
		t.Fatal(err)
	}
	if items, err := s.InboxForUser(ctx, owner); err != nil || len(items) != 1 || items[0].Kind != "recovery" || items[0].TurnID != third {
		t.Fatalf("the repeat must raise recovery: %+v %v", items, err)
	}
}

// A request changes nothing for a turn that ends another way: an answer that came first, or a stop of the agent.
func TestTurnInterruptRequestDoesNotApplyToAnotherEnd(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	requestInterrupt(t, s, f.turn.ID, TurnInterruptRequeue)
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 130, CancelIntent: true}); err != nil {
		t.Fatal(err)
	}
	var exec, outcome, noteState string
	if err := s.db.QueryRow(`SELECT exec,outcome FROM turn WHERE id=?`, f.turn.ID).Scan(&exec, &outcome); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&noteState); err != nil {
		t.Fatal(err)
	}
	if exec != "killed" || outcome == "cancelled" || noteState == "queued" {
		t.Fatalf("a stop's cancel was settled as a requeue: exec=%s outcome=%s note=%s", exec, outcome, noteState)
	}
}
