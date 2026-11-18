package store

import (
	"context"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// What the supervisor sends for a turn whose harness process was cut off: marked, with how it ended as the reason.
var interruptedExit = adapter.ExitInfo{ExitCode: 137, ErrorEvent: "the harness process was stopped by signal 9 (killed) (exit code 137)", Interrupted: true}

// An interrupted turn is continued once, on its own, with a note that names the reason; no recovery card blocks the agent.
func TestInterruptedTurnContinuesOnceOnItsOwn(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, interruptedExit); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("an interrupted turn must not raise a recovery card: items=%+v err=%v", items, err)
	}
	var childID string
	if err := s.db.QueryRow(`SELECT id FROM turn WHERE parent_turn_id=? AND mode='normal'`, f.turn.ID).Scan(&childID); err != nil {
		t.Fatalf("no continuation turn: %v", err)
	}
	var payload string
	if err := s.db.QueryRow(`SELECT payload_json FROM event WHERE turn_id=? AND kind='turn_interrupted_continuation'`, childID).Scan(&payload); err != nil {
		t.Fatalf("no system event in the room: %v", err)
	}
	for _, want := range []string{`"reason":"turn_interrupted"`, `"exit_code":"137"`, f.turn.ID} {
		if !strings.Contains(payload, want) {
			t.Fatalf("event payload %s lacks %s", payload, want)
		}
	}
	var body string
	if err := s.db.QueryRow(`SELECT m.body FROM note n JOIN message m ON m.id=n.message_id WHERE n.agent_id=? AND n.wake_reason='turn_interrupted'`, f.agent.ID).Scan(&body); err != nil {
		t.Fatalf("no continuation note: %v", err)
	}
	if !strings.HasPrefix(body, "your previous turn was interrupted (") || !strings.Contains(body, "signal 9") || !strings.HasSuffix(body, "check state and continue") {
		t.Fatalf("continuation note=%q", body)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != childID || len(delivery.Notes) != 2 {
		t.Fatalf("continuation dispatch ready=%t turn=%s notes=%d err=%v", ready, delivery.TurnID, len(delivery.Notes), err)
	}
}

// One continuation per chain, shared with the limit kind: a repeat, of either kind, raises the Recovery card.
func TestRepeatedInterruptionRaisesTheRecoveryCard(t *testing.T) {
	t.Parallel()
	for name, second := range map[string]adapter.ExitInfo{"interrupted": interruptedExit, "limit": limitStallExit} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, f, owner := recoveryFixture(t, false)
			ctx := context.Background()
			if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, interruptedExit); err != nil {
				t.Fatal(err)
			}
			var childID string
			if err := s.db.QueryRow(`SELECT id FROM turn WHERE parent_turn_id=? AND mode='normal'`, f.turn.ID).Scan(&childID); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProcessInfo(ctx, childID, f.agent.ID, 0, second); err != nil {
				t.Fatal(err)
			}
			items, err := s.InboxForUser(ctx, owner)
			if err != nil || len(items) != 1 || items[0].Kind != "recovery" || items[0].TurnID != childID {
				t.Fatalf("the repeat must raise recovery: items=%+v err=%v", items, err)
			}
		})
	}
}

// Without the supervisor's mark, the same non-zero exit is a crash and keeps the ordinary recovery path.
func TestExitWithoutTheInterruptedMarkStillRaisesRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	crash := interruptedExit
	crash.Interrupted = false
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, crash); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].Kind != "recovery" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}
