package store

import (
	"context"
	"testing"
)

type abandonStopState struct{ desired, item string }

func abandonStopRead(t *testing.T, s *Store, agentID, itemID string) abandonStopState {
	t.Helper()
	var st abandonStopState
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, agentID).Scan(&st.desired); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT state FROM inbox_item WHERE id=?`, itemID).Scan(&st.item); err != nil {
		t.Fatal(err)
	}
	return st
}

// Abandon and stop is the Abandon plus the stop of the agent: the queued notes are withdrawn, the item resolves under the choice it was
// asked with, and the agent's desired state is stopped. The decision hands the stopped agent back for the caller to tell its machine.
func TestRecoveryAbandonAndStopWithdrawsNotesAndStopsAgent(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	queued := queueUnrelatedNote(t, s, f)
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	decision, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon-stop", "abandon_stop", int(items[0].Rev))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Choice != "abandon_stop" || decision.StoppedAgent == nil || decision.StoppedAgent.Desired != "stopped" {
		t.Fatalf("decision=%+v stopped=%+v", decision, decision.StoppedAgent)
	}
	if got := abandonStopRead(t, s, f.agent.ID, items[0].ID); got != (abandonStopState{"stopped", "resolved"}) {
		t.Fatalf("state=%+v, want stopped and resolved", got)
	}
	if got := abandonNoteState(t, s, queued.ID); got != "aborted" {
		t.Fatalf("queued note=%q, want aborted", got)
	}
	var choice string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.choice') FROM event WHERE kind='inbox_item_resolved' AND json_extract(payload_json,'$.inbox_item_id')=?`, items[0].ID).Scan(&choice); err != nil || choice != "abandon_stop" {
		t.Fatalf("resolution choice=%q (%v)", choice, err)
	}
	var recoveryBlock int
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&recoveryBlock); err != nil || recoveryBlock != 0 {
		t.Fatalf("recovery_block=%d (%v), want lifted", recoveryBlock, err)
	}
}

// A plain Abandon still leaves the agent's admin state alone.
func TestRecoveryPlainAbandonDoesNotStopAgent(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	decision, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon-only", "abandon", int(items[0].Rev))
	if err != nil || decision.StoppedAgent != nil {
		t.Fatalf("decision=%+v %v", decision, err)
	}
	if got := abandonStopRead(t, s, f.agent.ID, items[0].ID); got.desired != "active" {
		t.Fatalf("desired=%q, want active", got.desired)
	}
}

// All or nothing: when the stop fails, the Abandon is rolled back with it (the note stays queued, the recovery block stays, the item stays
// open and the agent keeps its desired state).
func TestRecoveryAbandonAndStopFailureRollsBothBack(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	queued := queueUnrelatedNote(t, s, f)
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_stop BEFORE UPDATE OF desired ON agent WHEN NEW.desired='stopped' BEGIN SELECT RAISE(ABORT,'stop refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon-stop-fails", "abandon_stop", int(items[0].Rev)); err == nil {
		t.Fatal("the stop was refused but the decision succeeded")
	}
	if got := abandonStopRead(t, s, f.agent.ID, items[0].ID); got != (abandonStopState{"active", "open"}) {
		t.Fatalf("state=%+v, want active and open", got)
	}
	if got := abandonNoteState(t, s, queued.ID); got != "queued" {
		t.Fatalf("queued note=%q after the rollback, want queued", got)
	}
	var recoveryBlock int
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&recoveryBlock); err != nil || recoveryBlock != 1 {
		t.Fatalf("recovery_block=%d (%v), want it kept", recoveryBlock, err)
	}
}

// The item says how many queued notes an Abandon withdraws and in how many rooms: the agent's queued notes in any room.
func TestRecoveryItemCountsWithdrawnNotesAndRooms(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	queueUnrelatedNote(t, s, f)
	queueUnrelatedNote(t, s, f)
	other, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "other", CreatedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		message, err := tx.InsertMessage(Message{RoomID: other.ID, AuthorKind: "system", AuthorID: "system", Body: "elsewhere", OnlyTargets: []string{}})
		if err != nil {
			return err
		}
		_, err = tx.InsertNote(Note{AgentID: f.agent.ID, MessageID: message.ID, RoomID: other.ID, WakeReason: "mention"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	if items[0].WithdrawNotes != 3 || items[0].WithdrawRooms != 2 {
		t.Fatalf("counts=%d notes in %d rooms, want 3 in 2", items[0].WithdrawNotes, items[0].WithdrawRooms)
	}
}

// A lead's Abandon and stop of its worker's recovery goes through the stop_agent rule (StopAgentAsTool): the worker is stopped, its queued
// notes withdrawn, and the stop notice is posted like any stop_agent's.
func TestLeadAbandonAndStopStopsWorker(t *testing.T) {
	t.Parallel()
	s, f, _, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	queued := queueUnrelatedNote(t, s, f)
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	decision, _, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "lead-abandon-stop", "abandon_stop", "", recoveries[0].Rev)
	if err != nil {
		t.Fatal(err)
	}
	if decision.StoppedAgent == nil || decision.StoppedAgent.ID != f.agent.ID {
		t.Fatalf("stopped=%+v", decision.StoppedAgent)
	}
	if got := abandonStopRead(t, s, f.agent.ID, recoveries[0].ID); got != (abandonStopState{"stopped", "resolved"}) {
		t.Fatalf("state=%+v", got)
	}
	if got := abandonNoteState(t, s, queued.ID); got != "aborted" {
		t.Fatalf("queued note=%q", got)
	}
}

// A lead that may not stop the worker (stop_agent refuses it: here the target is another lead) is refused and nothing is applied.
func TestLeadAbandonAndStopRefusedLeavesItemOpen(t *testing.T) {
	t.Parallel()
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	queued := queueUnrelatedNote(t, s, f)
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	// The worker becomes a lead role: stop_agent never stops another lead, so Abandon and stop must not either.
	if _, err := s.db.Exec(`UPDATE agent SET role_id='role_leader_default' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	_ = owner
	if _, _, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "lead-refused", "abandon_stop", "", recoveries[0].Rev); err != ErrForbidden {
		t.Fatalf("err=%v, want ErrForbidden", err)
	}
	if got := abandonStopRead(t, s, f.agent.ID, recoveries[0].ID); got != (abandonStopState{"active", "open"}) {
		t.Fatalf("state=%+v, want nothing applied", got)
	}
	if got := abandonNoteState(t, s, queued.ID); got != "queued" {
		t.Fatalf("queued note=%q, want queued", got)
	}
}
