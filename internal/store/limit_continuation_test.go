package store

import (
	"context"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

// The stall evidence the supervisor sends for a turn its own watchdog killed: marked, and its evidence names the deadline.
var limitStallExit = adapter.ExitInfo{ExitCode: 124, ErrorEvent: "turn exceeded the 30m0s wall-clock limit (AIMEBU_TURN_MAX)", LimitStall: true}

// A limit kill is continued once, on its own: the notes move to a child turn with a note that says why, the room gets a
// system event, and no recovery card blocks the agent.
func TestLimitStallContinuesOnceOnItsOwn(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, limitStallExit); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("a limit kill must not raise a recovery card: items=%+v err=%v", items, err)
	}
	var outcome, settle string
	var blocked bool
	if err := s.db.QueryRow(`SELECT outcome,settle FROM turn WHERE id=?`, f.turn.ID).Scan(&outcome, &settle); err != nil {
		t.Fatal(err)
	}
	if outcome != "unknown" || settle != "settled" {
		t.Fatalf("origin turn=%s/%s", outcome, settle)
	}
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&blocked); err != nil || blocked {
		t.Fatalf("recovery_block=%t err=%v", blocked, err)
	}
	var childID string
	if err := s.db.QueryRow(`SELECT id FROM turn WHERE parent_turn_id=? AND mode='normal'`, f.turn.ID).Scan(&childID); err != nil {
		t.Fatalf("no continuation turn: %v", err)
	}
	var kind string
	if err := s.db.QueryRow(`SELECT kind FROM event WHERE turn_id=? AND kind='turn_limit_continuation'`, childID).Scan(&kind); err != nil {
		t.Fatalf("no system event in the room: %v", err)
	}
	var noteState, messageBody string
	if err := s.db.QueryRow(`SELECT n.state,m.body FROM note n JOIN message m ON m.id=n.message_id WHERE n.agent_id=? AND n.wake_reason='turn_limit'`, f.agent.ID).Scan(&noteState, &messageBody); err != nil {
		t.Fatalf("no continuation note: %v", err)
	}
	if noteState != "queued" || !strings.Contains(messageBody, "your previous turn was stopped by the turn limit") {
		t.Fatalf("continuation note=%s body=%q", noteState, messageBody)
	}
	// The original note was re-queued on the child, not left unknown.
	if err := s.db.QueryRow(`SELECT n.state FROM note n JOIN turn_note tn ON tn.note_id=n.id WHERE tn.turn_id=? AND n.id=?`, childID, f.note.ID).Scan(&noteState); err != nil || noteState != "queued" {
		t.Fatalf("original note on the child=%s err=%v", noteState, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != childID {
		t.Fatalf("continuation dispatch ready=%t turn=%s err=%v", ready, delivery.TurnID, err)
	}
	if len(delivery.Notes) != 2 || delivery.Notes[0].NoteID != f.note.ID {
		t.Fatalf("notes=%+v", delivery.Notes)
	}
	var contextData map[string]any
	if err := json.Unmarshal(delivery.Context, &contextData); err != nil {
		t.Fatal(err)
	}
	if _, held := contextData["recovery_evidence"]; held {
		t.Fatal("a limit continuation is not a recovery")
	}
}

// The continuation is one per chain: when the continuation turn is limit-killed too, the agent falls back to the recovery
// card exactly as before the automatic continuation existed.
func TestSecondLimitStallRaisesTheRecoveryCard(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, limitStallExit); err != nil {
		t.Fatal(err)
	}
	var childID string
	if err := s.db.QueryRow(`SELECT id FROM turn WHERE parent_turn_id=? AND mode='normal'`, f.turn.ID).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, childID, f.agent.ID, 0, limitStallExit); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].Kind != "recovery" || items[0].TurnID != childID {
		t.Fatalf("the second limit kill must raise recovery: items=%+v err=%v", items, err)
	}
	var blocked bool
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("recovery_block=%t err=%v", blocked, err)
	}
}

// The marking is what separates a limit kill from a crash: the same failed exit without it takes the ordinary recovery path.
func TestFailureWithoutTheLimitMarkStillRaisesRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	crash := limitStallExit
	crash.LimitStall = false
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, crash); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].Kind != "recovery" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	var blocked bool
	var noteState string
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("recovery_block=%t err=%v", blocked, err)
	}
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&noteState); err != nil || noteState != "unknown" {
		t.Fatalf("note=%s err=%v", noteState, err)
	}
}

// An agent that is not active is not continued behind its own back: the exit takes the ordinary recovery path.
func TestLimitStallOfAStoppedAgentFallsBackToRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(f.agent.ID, "usr_recovery", "stopped", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, limitStallExit); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].Kind != "recovery" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	var children int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turn WHERE parent_turn_id=?`, f.turn.ID).Scan(&children); err != nil || children != 0 {
		t.Fatalf("children=%d err=%v", children, err)
	}
}
