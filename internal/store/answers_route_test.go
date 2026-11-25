package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// nextNormalTurn ends the fixture's turn and starts another normal one for the same agent, optionally woken by the note of a held reply.
func nextNormalTurn(t *testing.T, s *Store, f executionFixture, prev Turn, wokenBy string) Turn {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, prev.ID); err != nil {
		t.Fatal(err)
	}
	turn, err := s.RecordTurn(ctx, Turn{AgentID: f.agent.ID, SessionID: "ses_test", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if wokenBy != "" {
		note, err := s.NoteForAgentMessage(ctx, f.agent.ID, wokenBy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE turn SET policy_json=(SELECT policy_json FROM turn WHERE id=?) WHERE id=?`, prev.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn
}

// asLead makes the fixture's agent a lead: nobody is above it, so its malformed answers reach people straight away.
func asLead(t *testing.T, s *Store, f executionFixture) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE role SET cap_is_lead=1 WHERE id=(SELECT role_id FROM agent WHERE id=?)`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
}

// holdTwice holds raw for the fixture's agent, then again on the turn the held-reply note woke (the repair failed too), which is when a lead's
// failure first reaches people. It returns the second turn.
func holdTwice(t *testing.T, s *Store, f executionFixture, raw string) Turn {
	t.Helper()
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	return holdAgain(t, s, f, raw)
}

// holdAgain is the second half of holdTwice, for a test whose first hold is already applied.
func holdAgain(t *testing.T, s *Store, f executionFixture, raw string) Turn {
	t.Helper()
	second := nextNormalTurn(t, s, f, f.turn, heldNote(t, s, f.room.ID).ID)
	if receipt, err := applyCandidate(t, s, second, f.agent.ID, 0, raw); err != nil || receipt.Result != "held" {
		t.Fatalf("second receipt=%+v err=%v", receipt, err)
	}
	return second
}

func heldNote(t *testing.T, s *Store, roomID string) Message {
	t.Helper()
	messages, err := s.ListMessagesAfter(context.Background(), roomID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if strings.HasPrefix(m.Body, heldReplyNotePrefix) {
			return m
		}
	}
	t.Fatalf("no held-reply note in %+v", messages)
	return Message{}
}

// A worker's prose answer goes back to the agent (the error only, never its text, which the room could read) with a fresh turn to resend it; no card is raised for people.
func TestWorkerProseAnswerGoesBackToTheAgentNotToPeople(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "Handoff: all done, see the branch.")
	if err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	note := heldNote(t, s, f.room.ID)
	if strings.Contains(note.Body, "Handoff: all done") || !strings.Contains(note.Body, "again") {
		t.Fatalf("note=%q", note.Body)
	}
	if _, err := s.NoteForAgentMessage(ctx, f.agent.ID, note.ID); err != nil {
		t.Fatalf("agent was not woken: %v", err)
	}
	if got := openInboxKinds(t, s, "usr_answers"); len(got) != 0 {
		t.Fatalf("people see %v after the first malformed answer", got)
	}
	// The resent answer is invalid again: with no lead to hand to, people finally hear of it.
	second := nextNormalTurn(t, s, f, f.turn, note.ID)
	if receipt, err := applyCandidate(t, s, second, f.agent.ID, 0, "still prose"); err != nil || receipt.Result != "held" {
		t.Fatalf("second receipt=%+v err=%v", receipt, err)
	}
	if got := strings.Join(openInboxKinds(t, s, "usr_answers"), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want delivery_failed", got)
	}
}

// When the correction does not settle it, a worker's failure is the lead's recovery: hidden from people, the lead woken, and the lead's
// retry asks the worker once more.
func TestWorkerFailedCorrectionGoesToLeadAsRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	turn := f.turn
	if _, err := s.db.Exec(`UPDATE turn SET exec='running' WHERE id=?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	if receipt, err := applyCandidate(t, s, turn, f.agent.ID, 0, "not json"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if recoveries, _ := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID); len(recoveries) != 0 {
		t.Fatalf("lead told before the correction failed: %+v", recoveries)
	}
	second := nextNormalTurn(t, s, f, turn, heldNote(t, s, f.room.ID).ID)
	if receipt, err := applyCandidate(t, s, second, f.agent.ID, 0, "still not json"); err != nil || receipt.Result != "held" {
		t.Fatalf("second receipt=%+v err=%v", receipt, err)
	}
	if got := openInboxKinds(t, s, owner); len(got) != 0 {
		t.Fatalf("people see %v, want nothing while the lead decides", got)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	var leadNotes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND wake_reason='worker_recovery'`, lead.ID).Scan(&leadNotes); err != nil || leadNotes != 1 {
		t.Fatalf("lead notes=%d err=%v", leadNotes, err)
	}
	if _, _, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "k-retry", "retry", "", recoveries[0].Rev); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var again int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND body LIKE ?`, f.room.ID, heldReplyNotePrefix+"%").Scan(&again); err != nil || again != 2 {
		t.Fatalf("held-reply notes=%d err=%v, want the lead's retry to add one", again, err)
	}
}

// heldNotice counts the delivery-failure room notices a room's messages carry.
func heldNotice(t *testing.T, s *Store, roomID string) int {
	t.Helper()
	messages, err := s.ListMessagesAfter(context.Background(), roomID, 0)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, m := range messages {
		if strings.Contains(m.Body, "Review the delivery failure") {
			notices++
		}
	}
	return notices
}

// A lead stopped before it can act on the repair note never resends, so the second hold that raises the card for people cannot happen: its
// first held answer reaches people at once. The repair note stays queued for when the lead resumes.
func TestStoppedLeadHoldReachesPeopleAtOnce(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "prose from a lead"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	heldNote(t, s, f.room.ID) // the repair note is still queued for the resumed lead
	if got := strings.Join(openInboxKinds(t, s, "usr_answers"), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want delivery_failed", got)
	}
	if notices := heldNotice(t, s, f.room.ID); notices != 1 {
		t.Fatalf("room notices=%d, want 1", notices)
	}
}

// A lead whose machine is lost cannot be woken either: the card and the room notice come at once, not after a repair that cannot run.
func TestLostMachineLeadHoldReachesPeopleAtOnce(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	if _, err := s.db.Exec(`UPDATE agent SET recovery_block=1,observed='lost' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "prose from a lead"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if got := strings.Join(openInboxKinds(t, s, "usr_answers"), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want delivery_failed", got)
	}
	if notices := heldNotice(t, s, f.room.ID); notices != 1 {
		t.Fatalf("room notices=%d, want 1", notices)
	}
}

// A stopped worker with no lead has nobody to wake it either: its first held answer reaches people at once.
func TestStoppedWorkerWithoutLeadHoldReachesPeopleAtOnce(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "prose from a worker"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if got := strings.Join(openInboxKinds(t, s, "usr_answers"), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want delivery_failed", got)
	}
}

// A stopped worker with a lead hands the failure to its lead as designed: the lead decides, people stay out of it for now.
func TestStoppedWorkerWithLeadHoldGoesToItsLead(t *testing.T) {
	t.Parallel()
	s, f, owner, lead := workerRecoveryFixture(t)
	if _, err := s.db.Exec(`UPDATE turn SET exec='running' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "not json"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if got := openInboxKinds(t, s, owner); len(got) != 0 {
		t.Fatalf("people see %v, want nothing while the lead decides", got)
	}
	recoveries, err := s.ListLeadRecoveries(context.Background(), lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
}

// A lead's own malformed answer is the one that reaches people.
func TestLeadMalformedAnswerReachesPeople(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	holdTwice(t, s, f, "prose from a lead")
	if got := strings.Join(openInboxKinds(t, s, "usr_answers"), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want delivery_failed", got)
	}
}

// A lead's held reply left a card for people; when its resend succeeds, the card settles on its own — even though the resend went to the
// room the reply was meant for and the card was raised in the turn's room.
func TestResendToTheIntendedRoomSettlesTheHeldCard(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	ctx := context.Background()
	other, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "other", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, other.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	// The reply to other is held whole for repeating an attachment (a store-level hold, so the candidate parses and the card records its
	// target rooms), and the card is raised in the turn's room.
	if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_route',?,'hash','image/png',1,'p','owner',?)`, f.project.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	second := holdTwice(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"x","attachments":["att_route"]},{"room":%q,"body":"y","attachments":["att_route"]}]}`, other.ID, other.ID))
	if got := strings.Join(openInboxKinds(t, s, "usr_answers"), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want delivery_failed", got)
	}
	third := nextNormalTurn(t, s, f, second, heldNote(t, s, f.room.ID).ID)
	if receipt, err := applyCandidate(t, s, third, f.agent.ID, 0, fmt.Sprintf(`{"messages":[{"room":%q,"body":"fixed"}]}`, other.ID)); err != nil || receipt.Result != "committed" {
		t.Fatalf("resend receipt=%+v err=%v", receipt, err)
	}
	if got := openInboxKinds(t, s, "usr_answers"); len(got) != 0 {
		t.Fatalf("the card stayed open after the resend reached its room: %v", got)
	}
}

// A committed reply that reaches neither the card's room nor the held reply's target leaves the card: the failed content still reached nobody.
func TestResendToAnotherRoomLeavesTheHeldCardOpen(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	ctx := context.Background()
	other, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "other", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, other.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	third, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "third", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, third.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	second := holdTwice(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"x","handoff":true,"outcome":"nope"}]}`, other.ID))
	next := nextNormalTurn(t, s, f, second, heldNote(t, s, f.room.ID).ID)
	if receipt, err := applyCandidate(t, s, next, f.agent.ID, 0, fmt.Sprintf(`{"messages":[{"room":%q,"body":"elsewhere"}]}`, third.ID)); err != nil || receipt.Result != "committed" {
		t.Fatalf("reply receipt=%+v err=%v", receipt, err)
	}
	if got := strings.Join(openInboxKinds(t, s, "usr_answers"), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want the card still open", got)
	}
}

// Two empty answers in a row with no tool activity are a broken session: a recovery for the lead offering a fresh session, nothing for
// people, and no second correction note.
func TestTwoEmptyAnswersRaiseBrokenSessionRecoveryForTheLead(t *testing.T) {
	t.Parallel()
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, ""); err != nil || receipt.Result != "held" || !strings.Contains(receipt.Error, "EOF") {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if recoveries, _ := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID); len(recoveries) != 0 {
		t.Fatalf("one empty answer raised %+v", recoveries)
	}
	second := nextNormalTurn(t, s, f, f.turn, "")
	if receipt, err := applyCandidate(t, s, second, f.agent.ID, 0, ""); err != nil || receipt.Result != "held" {
		t.Fatalf("second receipt=%+v err=%v", receipt, err)
	}
	if got := openInboxKinds(t, s, owner); len(got) != 0 {
		t.Fatalf("people see %v", got)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 || recoveries[0].Reason != brokenSessionReason {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	var notes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND body LIKE ?`, f.room.ID, heldReplyNotePrefix+"%").Scan(&notes); err != nil || notes != 1 {
		t.Fatalf("held-reply notes=%d err=%v, want only the first", notes, err)
	}
	// The interrupted task: a note of the broken turn that was taken and is done.
	if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, second.ID, f.note.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE note SET state='done' WHERE id=?`, f.note.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='native-1' WHERE id='ses_test'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "k-restart", "restart", "", recoveries[0].Rev); err != nil {
		t.Fatalf("restart: %v", err)
	}
	var native *string
	if err := s.db.QueryRow(`SELECT native_session_id FROM session WHERE id='ses_test'`).Scan(&native); err != nil || native != nil {
		t.Fatalf("native session=%v err=%v, want cleared", native, err)
	}
	var noteState string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&noteState); err != nil || noteState != "queued" {
		t.Fatalf("note state=%q err=%v, want queued so the task runs again on the fresh session", noteState, err)
	}
}

// Tool activity in either turn means the empty answer is not a broken session.
func TestEmptyAnswersWithToolActivityAreNotABrokenSession(t *testing.T) {
	t.Parallel()
	s, f, _, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if _, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	second := nextNormalTurn(t, s, f, f.turn, "")
	if err := s.RecordTurnEvent(ctx, second.ID, f.agent.ID, "src-1", "activity", "tool_use", []byte(`{"summary":"tool_use"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := applyCandidate(t, s, second, f.agent.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if recoveries, _ := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID); len(recoveries) != 0 {
		t.Fatalf("lead items=%+v", recoveries)
	}
}

// After escalation a person decides the lead-routed items too: Retry on a held answer, Restart on a broken session.
func TestPeopleDecideEscalatedAnswerHeldAndBrokenSession(t *testing.T) {
	t.Parallel()
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if _, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "not json"); err != nil {
		t.Fatal(err)
	}
	second := nextNormalTurn(t, s, f, f.turn, heldNote(t, s, f.room.ID).ID)
	if _, err := applyCandidate(t, s, second, f.agent.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	third := nextNormalTurn(t, s, f, second, "")
	if _, err := applyCandidate(t, s, third, f.agent.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE inbox_item SET lead_due_at=? WHERE recovery_lead_id=?`, "2000-01-01T00:00:00.000000000Z", lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepWorkerRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 2 {
		t.Fatalf("people see %+v err=%v, want the escalated items", items, err)
	}
	for _, item := range items {
		choice := map[string]string{answerHeldReason: "retry", brokenSessionReason: "restart"}[item.RecoveryPhase]
		if choice == "" {
			t.Fatalf("unexpected item %+v", item)
		}
		if _, _, err := s.DecideRecovery(ctx, item.ID, owner, "k-"+item.ID, choice, int(item.Rev)); err != nil {
			t.Fatalf("%s: %v", choice, err)
		}
	}
	if items, _ := s.InboxForUser(ctx, owner); len(items) != 0 {
		t.Fatalf("still open: %+v", items)
	}
}

// room_closed and not_member holds are never re-asked by an agent, so they stay with people even for a worker with a lead.
func TestClosedRoomHoldOfAWorkerStaysWithPeople(t *testing.T) {
	t.Parallel()
	s, f, owner, _ := workerRecoveryFixture(t)
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closing' WHERE id=?`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"late"}]}`, f.room.ID)
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw); err != nil || receipt.HeldReason != "room_closed" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if got := strings.Join(openInboxKinds(t, s, owner), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want delivery_failed", got)
	}
}

// A recovery the settle pass left open on purpose (a lost turn nothing proved gone) stays listed even once its room is closed.
func TestOpenRecoveryInClosedRoomStaysVisible(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 143, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='lost' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE state='open' AND kind='recovery'`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if open != 1 || err != nil || len(items) != 1 {
		t.Fatalf("a lost turn's recovery in a closed room must stay open and listed: open=%d items=%+v err=%v", open, items, err)
	}
}
