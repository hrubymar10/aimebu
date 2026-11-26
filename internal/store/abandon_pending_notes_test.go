package store

import (
	"context"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// A note queued for an agent while its killed turn was still running (a message that mentioned it) is not part of that turn, so the
// abandon of the turn's recovery must still withdraw it: otherwise the agent, released and idle, is handed it by the next dispatch pass.
func TestRecoveryAbandonWithdrawsNotesQueuedBeforeIt(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	var late Note
	if err := s.Tx(ctx, func(tx *Tx) error {
		message, err := tx.InsertMessage(Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "late nudge", OnlyTargets: []string{}})
		if err != nil {
			return err
		}
		late, err = tx.InsertNote(Note{AgentID: f.agent.ID, MessageID: message.ID, RoomID: f.room.ID, WakeReason: "mention"})
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
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon-late", "abandon", int(items[0].Rev)); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, late.ID).Scan(&state); err != nil || state != "aborted" {
		t.Fatalf("note queued before the abandon is %q (%v), want aborted", state, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("an abandoned agent was dispatched a turn: ready=%t %v", ready, err)
	}
}

// A message that mentions only one agent never queues a note for another agent of the room, and this holds around a recovery too.
func TestMentionOfOneAgentNeverWakesTheOtherAfterAbandon(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	other, err := s.CreateAgent(ctx, Agent{Slug: "umar", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test",
		Provider: "test", MachineID: f.agent.MachineID, CheckoutID: "chk_test", KeyHash: "hash2", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", other.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon-mention", "abandon", int(items[0].Rev)); err != nil {
		t.Fatal(err)
	}
	message, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "@" + other.ID + " please look"})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND message_id=?`, f.agent.ID, message.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the agent that was not mentioned got %d notes (%v)", n, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND message_id=?`, other.ID, message.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the mentioned agent got %d notes (%v)", n, err)
	}
}

// Stop is unlike Abandon: a note addressed to an agent that is already stopped stays queued and runs when it is started again.
func TestStoppedAgentQueuedNoteSurvivesRestart(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 143, false, true); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmAgentProcessExit(ctx, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	message, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "@" + f.agent.ID + " when you are back"})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE agent_id=? AND message_id=?`, f.agent.ID, message.ID).Scan(&state); err != nil || state != "queued" {
		t.Fatalf("note for the stopped agent=%q (%v)", state, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "active", 1); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT state FROM note WHERE agent_id=? AND message_id=?`, f.agent.ID, message.ID).Scan(&state); err != nil || state != "queued" {
		t.Fatalf("note after the restart=%q (%v)", state, err)
	}
}

// queueUnrelatedNote queues a note for the fixture agent that belongs to no turn.
func queueUnrelatedNote(t *testing.T, s *Store, f executionFixture) Note {
	t.Helper()
	return queueNoteFor(t, s, f, f.agent.ID)
}

func queueNoteFor(t *testing.T, s *Store, f executionFixture, agentID string) Note {
	t.Helper()
	var note Note
	if err := s.Tx(context.Background(), func(tx *Tx) error {
		message, err := tx.InsertMessage(Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "unrelated", OnlyTargets: []string{}})
		if err != nil {
			return err
		}
		note, err = tx.InsertNote(Note{AgentID: agentID, MessageID: message.ID, RoomID: f.room.ID, WakeReason: "mention"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return note
}

func abandonNoteState(t *testing.T, s *Store, id string) string {
	t.Helper()
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// The abandon withdraws only the abandoned agent's notes: a note queued for another agent before the decision stays queued.
func TestRecoveryAbandonLeavesOtherAgentsQueuedNotes(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	other, err := s.CreateAgent(ctx, Agent{Slug: "umar", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test",
		Provider: "test", MachineID: f.agent.MachineID, CheckoutID: "chk_test", KeyHash: "hash2", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", other.ID); err != nil {
		t.Fatal(err)
	}
	own := queueNoteFor(t, s, f, f.agent.ID)
	theirs := queueNoteFor(t, s, f, other.ID)
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon-filter", "abandon", int(items[0].Rev)); err != nil {
		t.Fatal(err)
	}
	if got := abandonNoteState(t, s, own.ID); got != "aborted" {
		t.Fatalf("abandoned agent's note=%q, want aborted", got)
	}
	if got := abandonNoteState(t, s, theirs.ID); got != "queued" {
		t.Fatalf("other agent's note=%q, want queued", got)
	}
}

// Withdrawal wins for a stopped agent: an abandon is a decision about the work, so it aborts the queued notes of an agent that was stopped too.
func TestRecoveryAbandonWithdrawsNotesOfStoppedAgent(t *testing.T) {
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
	// A stop settles the agent's open recovery items, so the stopped state is set directly to keep the item decidable: the abandon must still
	// withdraw the queued note (a note addressed to a stopped agent otherwise waits for its restart).
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	queued := queueUnrelatedNote(t, s, f)
	if got := abandonNoteState(t, s, queued.ID); got != "queued" {
		t.Fatalf("note before the abandon=%q, want queued", got)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon-stopped", "abandon", int(items[0].Rev)); err != nil {
		t.Fatal(err)
	}
	if got := abandonNoteState(t, s, queued.ID); got != "aborted" {
		t.Fatalf("stopped agent's note after the abandon=%q, want aborted", got)
	}
}

// The context-overflow abandon withdraws the agent's queued notes as well.
func TestContextOverflowAbandonWithdrawsQueuedNotes(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	queued := queueUnrelatedNote(t, s, f)
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, ErrorEvent: "Prompt is too long"}); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon-overflow-queued", "abandon", int(items[0].Rev)); err != nil {
		t.Fatal(err)
	}
	if got := abandonNoteState(t, s, queued.ID); got != "aborted" {
		t.Fatalf("overflow abandon left note %q, want aborted", got)
	}
}
