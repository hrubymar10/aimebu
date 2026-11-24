package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

func TestTurnLimitRecoveryNoticeNamesTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
	}{
		{"max", "turn exceeded the 2h0m0s turn limit"},
		{"idle", "turn produced no output for 30m0s (AIMEBU_TURN_IDLE)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _, lead := workerRecoveryFixture(t)
			ctx := context.Background()
			exit := adapter.ExitInfo{ExitCode: 124, ErrorEvent: tc.reason, LimitStall: true}
			if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, exit); err != nil {
				t.Fatal(err)
			}
			var childID string
			if err := s.db.QueryRow(`SELECT id FROM turn WHERE parent_turn_id=? AND mode='normal'`, f.turn.ID).Scan(&childID); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProcessInfo(ctx, childID, f.agent.ID, 0, exit); err != nil {
				t.Fatal(err)
			}
			var body string
			if err := s.db.QueryRow(`SELECT m.body FROM note n JOIN message m ON m.id=n.message_id WHERE n.agent_id=? AND n.wake_reason='worker_recovery'`, lead.ID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body, "Evidence: ") || !strings.Contains(body, "error event: "+tc.reason) {
				t.Fatalf("lead notice = %q", body)
			}
			if tc.name == "max" && strings.Contains(body, "stalled") {
				t.Fatalf("limit described as a stall: %q", body)
			}
		})
	}
}

func workerRecoveryFixture(t *testing.T) (*Store, executionFixture, string, Agent) {
	t.Helper()
	s, f, owner := recoveryFixture(t, false)
	lead, err := s.CreateAgent(context.Background(), Agent{Slug: "recoverylead", ProjectID: f.project.ID, RoleID: "role_leader_default", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.agent.MachineID, CheckoutID: f.agent.CheckoutID, KeyHash: "recovery-lead-key", CreatedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := s.AgentInRoom(context.Background(), lead.ID, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !joined {
		if err := s.AddMember(context.Background(), f.room.ID, "agent", lead.ID); err != nil {
			t.Fatal(err)
		}
	}
	return s, f, owner, lead
}

func TestWorkerRecoveryGoesToLeadAndLeadResolves(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("human inbox=%+v err=%v", items, err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	var noteCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND wake_reason='worker_recovery'`, lead.ID).Scan(&noteCount); err != nil || noteCount != 1 {
		t.Fatalf("lead notes=%d err=%v", noteCount, err)
	}
	var noticeCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND body LIKE 'Recovery handed to the lead%'`, f.room.ID).Scan(&noticeCount); err != nil || noticeCount != 1 {
		t.Fatalf("worker room notices=%d err=%v", noticeCount, err)
	}
	decision, reused, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "lead-resume", "resume", "checked evidence", recoveries[0].Rev)
	if err != nil || reused || decision.TurnID == "" {
		t.Fatalf("decision=%+v reused=%v err=%v", decision, reused, err)
	}
	again, reused, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "lead-resume", "resume", "checked evidence", recoveries[0].Rev+1)
	if err != nil || !reused || again.TurnID != decision.TurnID {
		t.Fatalf("replay=%+v reused=%v err=%v", again, reused, err)
	}
	var blocked bool
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&blocked); err != nil || blocked {
		t.Fatalf("worker blocked=%v err=%v", blocked, err)
	}
}

func TestWorkerRecoveryEscalatesWhenDueOrLeadBlocked(t *testing.T) {
	for _, blockedLead := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "blocked"}[blockedLead], func(t *testing.T) {
			s, f, owner, lead := workerRecoveryFixture(t)
			ctx := context.Background()
			if blockedLead {
				if _, err := s.db.Exec(`UPDATE agent SET recovery_block=1 WHERE id=?`, lead.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
				t.Fatal(err)
			}
			if !blockedLead {
				if _, err := s.db.Exec(`UPDATE inbox_item SET lead_due_at='2000-01-01T00:00:00.000000000Z' WHERE agent_id=? AND kind='recovery'`, f.agent.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.SweepWorkerRecoveries(ctx); err != nil {
					t.Fatal(err)
				}
			}
			items, err := s.InboxForUser(ctx, owner)
			if err != nil || len(items) != 1 {
				t.Fatalf("human inbox=%+v err=%v", items, err)
			}
		})
	}
}

func TestLeadOwnRecoveryGoesToHuman(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET role_id='role_leader_default' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("lead recovery inbox=%+v err=%v", items, err)
	}
}

func TestWorkerRecoveryWithoutLeadGoesToHuman(t *testing.T) {
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("human inbox=%+v err=%v", items, err)
	}
}

// A lead outside the worker's room, in a project with no home room to tell it in, leaves the decision to the person: the turn's exit is
// still recorded, instead of failing for want of a room.
func TestWorkerRecoveryWithLeadOutsideRoomAndNoHomeRoomGoesToHuman(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE room_member SET left_at=? WHERE room_id=? AND member_kind='agent' AND member_id=?`, timestamp(), f.room.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET kind='task' WHERE project_id=? AND kind='home'`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatalf("the exit must be recorded: %v", err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("human inbox=%+v err=%v", items, err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 0 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
}

// A lead retrying a decision on an item the sweep gave to people and someone then resolved reads who resolved it and how, not a bare
// refusal that sounds like it lacks the lead role.
func TestLateLeadDecisionOnAResolvedRecoverySaysWhoResolvedIt(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	item := recoveries[0]
	// The sweep hands the item to people: while it is open, the lead's decision is still refused.
	if _, err := s.db.Exec(`UPDATE inbox_item SET lead_due_at=NULL WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecoveryAsAgent(ctx, item.ID, lead.ID, "lead-late", "retry", "", item.Rev); err != ErrForbidden {
		t.Fatalf("an open item held by people: %v", err)
	}
	if _, _, err := s.DecideRecovery(ctx, item.ID, owner, "person-retry", "retry", int(item.Rev)); err != nil {
		t.Fatalf("person decision: %v", err)
	}
	rev, err := s.RecoveryItemRevision(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.DecideRecoveryAsAgent(ctx, item.ID, lead.ID, "lead-late", "retry", "", rev)
	var settled *AlreadySettledError
	if !errors.As(err, &settled) || !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("late lead decision: %v", err)
	}
	if settled.By != owner || settled.How != "retry" || settled.Error() != "already resolved by "+owner+" (choice retry)" {
		t.Fatalf("settled=%+v text=%q", settled, settled.Error())
	}
}

// A lead the item was not assigned to learns nothing about how a settled item ended: it gets the plain refusal, as for any item it never held.
func TestOtherLeadGetsNoSettledMetadata(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	other, err := s.CreateAgent(ctx, Agent{Slug: "otherlead", ProjectID: f.project.ID, RoleID: "role_leader_default", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.agent.MachineID, CheckoutID: f.agent.CheckoutID, KeyHash: "other-lead-key", CreatedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	item := recoveries[0]
	if _, err := s.db.Exec(`UPDATE inbox_item SET lead_due_at=NULL WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecovery(ctx, item.ID, owner, "person-retry", "retry", int(item.Rev)); err != nil {
		t.Fatal(err)
	}
	rev, err := s.RecoveryItemRevision(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecoveryAsAgent(ctx, item.ID, other.ID, "other-late", "retry", "", rev); err != ErrForbidden {
		t.Fatalf("an unassigned lead on a settled item: %v", err)
	}
}

func TestStopDuringTurnCancelsWithoutRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	// The fixture's turn is admitted and acknowledged, so it is running with its notes in turn when the stop lands.
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 143, false, true); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("human inbox=%+v err=%v", items, err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 0 {
		t.Fatalf("lead recoveries=%+v err=%v", recoveries, err)
	}
	var notes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE wake_reason='worker_recovery'`).Scan(&notes); err != nil || notes != 0 {
		t.Fatalf("worker_recovery notes=%d err=%v", notes, err)
	}
	var outcome, settle string
	if err := s.db.QueryRow(`SELECT outcome,settle FROM turn WHERE id=?`, f.turn.ID).Scan(&outcome, &settle); err != nil || outcome != "cancelled" || settle != "settled" {
		t.Fatalf("stopped turn outcome=%q settle=%q err=%v", outcome, settle, err)
	}
	var noteState string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&noteState); err != nil || noteState != "aborted" {
		t.Fatalf("stopped turn note=%q err=%v", noteState, err)
	}
	var blocked bool
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&blocked); err != nil || blocked {
		t.Fatalf("recovery_block=%v err=%v", blocked, err)
	}
	var cancelReason string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.cancel_reason') FROM event WHERE turn_id=? AND kind='turn_process_exit'`, f.turn.ID).Scan(&cancelReason); err != nil || cancelReason != "agent stopped" {
		t.Fatalf("exit cancel_reason=%q err=%v", cancelReason, err)
	}
}

func TestStopDropsQueuedNotesWithoutStartingTurn(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	// The seeded turn ends by the deliberate stop itself: cancelled, nothing recovered.
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 143, false, true); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmAgentProcessExit(ctx, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	// The agent starts again and a new message queues a note for it, which waits while it is stopped once more.
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "active", 1); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "one more thing"}); err != nil {
		t.Fatal(err)
	}
	var queued string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE agent_id=? AND message_id=(SELECT id FROM message WHERE body='one more thing')`, f.agent.ID).Scan(&queued); err != nil || queued != "queued" {
		t.Fatalf("queued note=%q err=%v", queued, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "stopped", 2); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT state FROM note WHERE agent_id=? AND message_id=(SELECT id FROM message WHERE body='one more thing')`, f.agent.ID).Scan(&queued); err != nil || queued != "dropped" {
		t.Fatalf("stopped agent's queued note=%q err=%v", queued, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.count') FROM event WHERE kind='notes_dropped' AND agent_id=? AND json_extract(payload_json,'$.reason')='agent stopped'`, f.agent.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("notes_dropped event count=%d err=%v", count, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || ready || delivery.TurnID != "" {
		t.Fatalf("dispatch after stop=%+v ready=%t err=%v", delivery, ready, err)
	}
	var turns int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND id<>?`, f.agent.ID, f.turn.ID).Scan(&turns); err != nil || turns != 0 {
		t.Fatalf("turns started after stop=%d err=%v", turns, err)
	}
}

func TestCrashAfterStopStillRaisesRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	// A genuine crash (no cancel intent) keeps the ordinary recovery path exactly as before.
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead recoveries=%+v err=%v", recoveries, err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) > 0 {
		t.Fatalf("crash recovery reached the human inbox=%+v", items)
	}
}
