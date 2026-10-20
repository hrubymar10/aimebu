package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/answer"
)

func answerFixture(t *testing.T, noAction bool) (*Store, executionFixture) {
	t.Helper()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.AddMember(ctx, f.room.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_answers','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_answers','owner')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	policy, _ := json.Marshal(TurnPolicy{Capabilities: adapter.Capabilities{NoActionMode: noAction}})
	if _, err := s.db.Exec(`UPDATE turn SET policy_json=? WHERE id=?`, string(policy), f.turn.ID); err != nil {
		t.Fatal(err)
	}
	return s, f
}

func applyCandidate(t *testing.T, s *Store, turn Turn, agentID string, rev int, raw string) (AnswerReceipt, error) {
	t.Helper()
	parsed, parseErr := answer.Parse([]byte(raw), turn.Mode == "diagnostic")
	sum := sha256.Sum256([]byte(raw))
	input := AnswerInput{TurnID: turn.ID, AgentID: agentID, Generation: turn.Generation,
		AnswerRev: rev, Digest: hex.EncodeToString(sum[:]), Candidate: []byte(raw)}
	if parseErr != nil {
		input.ParseError = parseErr.Error()
	} else {
		input.Parsed = &parsed
	}
	return s.ApplyAnswer(context.Background(), input)
}

func TestStructuredAnswerCommitsMessagesInboxAndReceiptAtomically(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"first"},{"room":%q,"body":"choose","ask":{"kind":"question","text":"Which?","options":["A","B"],"free_text":true}}]}`, f.room.ID, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 2 {
		t.Fatalf("commit receipt=%+v error=%v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 3 || messages[1].SourceOutID == "" || messages[2].AskJSON == "" {
		t.Fatalf("committed messages=%+v error=%v", messages, err)
	}
	var outbox, inbox int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE turn_id=? AND state='committed'`, f.turn.ID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE turn_id=? AND kind='question' AND state='open'`, f.turn.ID).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	if outbox != 2 || inbox != 1 {
		t.Fatalf("outbox=%d inbox=%d", outbox, inbox)
	}
	replay, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || replay.Result != "committed" || len(replay.MessageIDs) != 2 {
		t.Fatalf("replay receipt=%+v error=%v", replay, err)
	}
	if _, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[]}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("different digest replay=%v", err)
	}
	if _, err := applyCandidate(t, s, f.turn, "agt_foreign", 0, raw); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign agent replay=%v", err)
	}
}

func TestAttentionAnswerUsesOneInboxAndWakesRequester(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Choose","needs_attention":true,"open_questions":[{"question":"Scope?","options":["Small","Large"]}]}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	items, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 || len(items[0].ProposedAnswers) != 0 || len(items[0].OpenQuestions) != 1 || !items[0].FreeText {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	answer, replay, err := s.AnswerInbox(ctx, items[0].ID, "usr_answers", "attention-key", "Q1) a) Small", int(items[0].Rev))
	if err != nil || replay || answer.ReplyTo != receipt.MessageIDs[0] {
		t.Fatalf("answer=%+v replay=%v err=%v", answer, replay, err)
	}
	var state, answerID string
	if err := s.db.QueryRow(`SELECT state,answer_message_id FROM inbox_item WHERE id=?`, items[0].ID).Scan(&state, &answerID); err != nil || state != "resolved" || answerID != answer.ID {
		t.Fatalf("state=%q answer=%q err=%v", state, answerID, err)
	}
	if _, reused, err := s.AnswerInbox(ctx, items[0].ID, "usr_answers", "attention-key", "Q1) a) Small", int(items[0].Rev)); err != nil || !reused {
		t.Fatalf("idempotent replay=%v err=%v", reused, err)
	}
}

func TestThreadedInboxAnswerStaysInThread(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	root, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "thread root"})
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Choose","thread_root":%q,"needs_attention":true}]}`, f.room.ID, root.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	items, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 {
		t.Fatalf("inbox=%+v err=%v", items, err)
	}
	answer, reused, err := s.AnswerInbox(ctx, items[0].ID, "usr_answers", "thread-answer", "A", int(items[0].Rev))
	if err != nil || reused || answer.ReplyTo != receipt.MessageIDs[0] || answer.ThreadRoot != root.ID {
		t.Fatalf("thread answer=%+v reused=%v err=%v", answer, reused, err)
	}
}

func TestAttentionWithoutChoicesStillCreatesFreeTextInbox(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Please review","needs_attention":true}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	items, err := s.InboxForUser(context.Background(), "usr_answers")
	if err != nil || len(items) != 1 || !items[0].FreeText || len(items[0].Options) != 0 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || !messages[len(messages)-1].NeedsAttention {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
}

func TestAskAndAttentionControlsProduceClearReceiptError(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Choose","ask":{"kind":"question","text":"Choose"},"proposed_answers":["A","B"]}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || !strings.Contains(receipt.Error, "cannot combine ask with attention controls") {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestMixedAttentionControlsProduceClearReceiptError(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Choose","proposed_answers":["Yes","No"],"open_questions":[{"question":"Scope?","options":["Small","Large"]}]}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || !strings.Contains(receipt.Error, "cannot combine proposed answers with open questions") {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestVisualPlanPersistsOnMessageAndOutbox(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"plan","visual_plan":[{"type":"markdown","data":{"markdown":"hello"}},{"type":"future","data":{"x":1}}],"appendix_pages":[{"title":"Full plan","body":"detail"}]}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.Warnings) != 1 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) < 2 || !strings.Contains(messages[len(messages)-1].VisualPlanJSON, `"future"`) ||
		!strings.Contains(messages[len(messages)-1].AppendixPagesJSON, `"Full plan"`) {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	var plan, pages string
	if err := s.db.QueryRow(`SELECT visual_plan_json,appendix_pages_json FROM outbox WHERE turn_id=?`, f.turn.ID).Scan(&plan, &pages); err != nil || plan != messages[len(messages)-1].VisualPlanJSON || pages != messages[len(messages)-1].AppendixPagesJSON {
		t.Fatalf("outbox plan=%q pages=%q err=%v", plan, pages, err)
	}
}

func TestCommittedAnswerResolvesPriorDeliveryFailure(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	if _, err := s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,outcome,settle)
		SELECT 'turn_unrelated_failure',agent_id,session_id,generation,room_id,1,'normal','exited','failed','settled' FROM turn WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,created_at) VALUES ('inb_prior_failure',?,?,?,?,? ,?,'delivery_failed',?)`, f.project.ID, f.room.ID, f.turn.ID, f.agent.ID, "agent", f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,created_at) VALUES ('inb_unrelated_failure',?,?, 'turn_unrelated_failure',?,?,?,'delivery_failed',?)`, f.project.ID, f.room.ID, f.agent.ID, "agent", f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[]}`)
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("receipt=%+v %v", receipt, err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM inbox_item WHERE id='inb_prior_failure'`).Scan(&state); err != nil || state != "resolved" {
		t.Fatalf("state=%q %v", state, err)
	}
	if err := s.db.QueryRow(`SELECT state FROM inbox_item WHERE id='inb_unrelated_failure'`).Scan(&state); err != nil || state != "open" {
		t.Fatalf("unrelated state=%q %v", state, err)
	}
}

func TestInvalidBatchRequestsOneCorrectionThenCommits(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, true)
	bad := fmt.Sprintf(`{"messages":[{"room":%q,"body":"valid"},{"room":%q,"body":"bad","reply_to":"msg_missing"}]}`, f.room.ID, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, bad)
	if err != nil || receipt.Result != "correction_requested" {
		t.Fatalf("first answer=%+v %v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("partial batch committed: %+v %v", messages, err)
	}
	if err := s.RecordProcess(context.Background(), f.turn.ID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	correction, err := s.CorrectionTurn(context.Background(), f.turn.ID)
	if err != nil || correction.Mode != "correction" {
		t.Fatalf("correction turn=%+v %v", correction, err)
	}
	good := fmt.Sprintf(`{"messages":[{"room":%q,"body":"fixed"}]}`, f.room.ID)
	fixed, err := applyCandidate(t, s, correction, f.agent.ID, 1, good)
	if err != nil || fixed.Result != "committed" || len(fixed.Additional) != 1 || fixed.Additional[0].Result != "superseded" {
		t.Fatalf("correction receipt=%+v %v", fixed, err)
	}
	messages, err = s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 2 || messages[1].Body != "fixed" {
		t.Fatalf("corrected messages=%+v %v", messages, err)
	}
	var superseded, committed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE turn_id=? AND answer_rev=0 AND state='superseded'`, f.turn.ID).Scan(&superseded); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE turn_id=? AND answer_rev=1 AND state='committed'`, f.turn.ID).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if superseded != 2 || committed != 1 {
		t.Fatalf("outbox rev0=%d rev1=%d", superseded, committed)
	}
}

func TestNeverStartedCorrectionSettlesParentAndRaisesBlockedFailure(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, true)
	ctx := context.Background()
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "journal-test", []byte("[]"), nil); err != nil {
		t.Fatal(err)
	}
	first, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[`)
	if err != nil || first.Result != "correction_requested" {
		t.Fatalf("first=%+v %v", first, err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	child, err := s.CorrectionTurn(ctx, f.turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET recovery_block=1 WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: child.ID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "journal-test", []byte("[]"), nil); err != nil {
		t.Fatal(err)
	}
	childState, err := s.DeliveryState(ctx, child.ID)
	if err != nil || childState.Exec != "killed" || childState.Settle != "settled" {
		t.Fatalf("child=%+v %v", childState, err)
	}
	parentState, err := s.DeliveryState(ctx, f.turn.ID)
	if err != nil || parentState.Settle != "settled" {
		t.Fatalf("parent=%+v %v", parentState, err)
	}
	_, raw, err := s.ReceiptByTurn(ctx, f.turn.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var held AnswerReceipt
	if err := json.Unmarshal([]byte(raw), &held); err != nil || held.Result != "held" || held.HeldReason != "correction_unavailable" {
		t.Fatalf("held=%+v %v", held, err)
	}
	var inbox int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE turn_id=? AND kind='delivery_failed'`, f.turn.ID).Scan(&inbox); err != nil || inbox != 1 {
		t.Fatalf("inbox=%d %v", inbox, err)
	}
}

func TestStructuredAnswerHoldsUnavailableClosedAndCrossRoom(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, reason string
		noAction     bool
		candidate    func(*testing.T, *Store, executionFixture) string
	}{
		{"unavailable", "correction_unavailable", false, func(_ *testing.T, _ *Store, f executionFixture) string { return `{"messages":[` }},
		{"closed", "room_closed", true, func(t *testing.T, s *Store, f executionFixture) string {
			if _, err := s.db.Exec(`UPDATE room SET lifecycle='closing' WHERE id=?`, f.room.ID); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf(`{"messages":[{"room":%q,"body":"late"}]}`, f.room.ID)
		}},
		{"cross_room_reply", "correction_requested", true, func(t *testing.T, s *Store, f executionFixture) string {
			other, err := s.CreateRoom(context.Background(), Room{ProjectID: f.project.ID, Kind: "task", Title: "other", CreatedBy: "usr_answers"})
			if err != nil {
				t.Fatal(err)
			}
			message, err := s.InsertMessage(context.Background(), Message{RoomID: other.ID, AuthorKind: "system", AuthorID: "system", Body: "other"})
			if err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf(`{"messages":[{"room":%q,"body":"wrong ref","reply_to":%q}]}`, f.room.ID, message.ID)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, f := answerFixture(t, test.noAction)
			raw := test.candidate(t, s, f)
			receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || receipt.Result != test.reason && receipt.HeldReason != test.reason {
				t.Fatalf("answer=%+v error=%v", receipt, err)
			}
			messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
			wantMessages := 1
			if test.reason == "correction_unavailable" {
				wantMessages = 2
			}
			if err != nil || len(messages) != wantMessages {
				t.Fatalf("unexpected message commit=%+v %v", messages, err)
			}
		})
	}
}

func TestQuestionWithUnknownMessageMetadataCommitsInboxAndWarning(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Which team?","type":"question","ask":{"kind":"question","text":"Choose a team","options":["scratch pair"],"free_text":false}}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.Warnings) != 1 || !strings.Contains(receipt.Warnings[0], `"type"`) {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	items, err := s.InboxForUser(context.Background(), "usr_answers")
	if err != nil || len(items) != 1 || items[0].Kind != "question" || len(items[0].Options) != 1 || items[0].Options[0] != "scratch pair" {
		t.Fatalf("team inbox=%+v err=%v", items, err)
	}
	_, saved, err := s.ReceiptByTurn(context.Background(), f.turn.ID, 0)
	if err != nil || !strings.Contains(saved, `ignored unknown field`) {
		t.Fatalf("saved receipt=%s err=%v", saved, err)
	}
}

func TestDiagnosticAnswerStaysOutOfRooms(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	if _, err := s.db.Exec(`UPDATE turn SET mode='diagnostic' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	f.turn.Mode = "diagnostic"
	raw := `{"messages":[],"reconciliation":{"status":"clean","findings":["no effects"]}}`
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("diagnostic=%+v %v", receipt, err)
	}
	var report string
	if err := s.db.QueryRow(`SELECT report_json FROM turn WHERE id=?`, f.turn.ID).Scan(&report); err != nil || report != raw {
		t.Fatalf("diagnostic report=%q %v", report, err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("diagnostic reached room=%+v %v", messages, err)
	}
}

func TestAnswerRejectsAttachmentReusedAcrossMessages(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_answer',?,'hash','image/png',1,'p','owner','now')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"one","attachments":["att_answer"]},{"room":%q,"body":"two","attachments":["att_answer"]}]}`, f.room.ID, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || receipt.HeldReason != "correction_unavailable" {
		t.Fatalf("reused attachment=%+v %v", receipt, err)
	}
	var bound int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attachment WHERE id='att_answer' AND message_id IS NOT NULL`).Scan(&bound); err != nil || bound != 0 {
		t.Fatalf("attachment bound=%d %v", bound, err)
	}
}

func TestInvalidCorrectionHoldsAndRaisesFailureForHuman(t *testing.T) {
	t.Parallel()
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			s, f := answerFixture(t, true)
			first, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[`)
			if err != nil || first.Result != "correction_requested" {
				t.Fatalf("initial correction request=%+v %v", first, err)
			}
			if err := s.RecordProcess(context.Background(), f.turn.ID, f.agent.ID, 0, 0, true, false); err != nil {
				t.Fatal(err)
			}
			correction, err := s.CorrectionTurn(context.Background(), f.turn.ID)
			if err != nil {
				t.Fatal(err)
			}
			if blocked {
				if _, err := s.db.Exec(`UPDATE agent SET recovery_block=1 WHERE id=?`, f.agent.ID); err != nil {
					t.Fatal(err)
				}
			}
			receipt, err := applyCandidate(t, s, correction, f.agent.ID, 1, `{"messages":[`)
			if err != nil || receipt.Result != "held" || receipt.HeldReason != "invalid_after_correction" || len(receipt.Additional) != 1 || receipt.Additional[0].Result != "superseded" {
				t.Fatalf("invalid correction=%+v %v", receipt, err)
			}
			var inbox int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE turn_id=? AND kind='delivery_failed'`, f.turn.ID).Scan(&inbox); err != nil {
				t.Fatal(err)
			}
			want := 1
			if inbox != want {
				t.Fatalf("delivery failure inbox=%d want=%d", inbox, want)
			}
		})
	}
}
