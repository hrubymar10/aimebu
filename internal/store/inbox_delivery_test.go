package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestPiGemmaRehearsalCandidateDelivers(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw, err := os.ReadFile("../answer/testdata/pi_gemma_rehearsal_fenced.txt")
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "room_01M4Z024PVJK6Y0XT3Q3J5GYQ7", f.room.ID))
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, string(raw))
	if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
		t.Fatalf("rehearsal receipt=%+v err=%v", receipt, err)
	}
	items, err := s.InboxForUser(context.Background(), "usr_answers")
	if err != nil || len(items) != 0 {
		t.Fatalf("rehearsal inbox=%+v err=%v", items, err)
	}
}

func TestUnavailableCorrectionCreatesVisibleDeliveryFailureOnce(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Private DM code: cedar lake","ask":{"question":"Which team?","options":["Build team"]}}]}`, f.room.ID)
	first, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || first.Result != "held" || first.HeldReason != "correction_unavailable" {
		t.Fatalf("held result=%+v err=%v", first, err)
	}
	items, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 || items[0].HeldReason != "correction_unavailable" ||
		!strings.Contains(items[0].DeliveryError, "invalid") || !strings.Contains(items[0].CandidatePreview, "cedar lake") {
		t.Fatalf("delivery failure=%+v err=%v", items, err)
	}
	messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) != 2 || !strings.Contains(messages[1].Body, "correction_unavailable") ||
		strings.Contains(messages[1].Body, "cedar lake") || strings.Contains(messages[1].Body, "invalid") {
		t.Fatalf("system notice=%+v err=%v", messages, err)
	}
	if _, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw); err != nil {
		t.Fatal(err)
	}
	items, err = s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 {
		t.Fatalf("replayed inbox=%+v err=%v", items, err)
	}
	messages, err = s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) != 2 {
		t.Fatalf("replayed notices=%+v err=%v", messages, err)
	}
}

func TestDismissDeliveryFailure(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Draft held for review"}]}`, f.room.ID)
	// A malformed candidate reaches the informational delivery-failure item.
	if _, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw[:len(raw)-1]); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 || items[0].Kind != "delivery_failed" {
		t.Fatalf("inbox=%+v err=%v", items, err)
	}
	item := items[0]
	if err := s.DismissInboxItem(ctx, item.ID, "usr_missing", int(item.Rev)); err != ErrForbidden {
		t.Fatalf("unauthorized dismiss: %v", err)
	}
	if err := s.DismissInboxItem(ctx, item.ID, "usr_answers", int(item.Rev)+1); err != ErrStaleInboxRev {
		t.Fatalf("stale rev: %v", err)
	}
	if err := s.DismissInboxItem(ctx, item.ID, "usr_answers", int(item.Rev)); err != nil {
		t.Fatal(err)
	}
	items, err = s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 0 {
		t.Fatalf("dismissed inbox=%+v err=%v", items, err)
	}
	if err := s.DismissInboxItem(ctx, item.ID, "usr_answers", int(item.Rev)+1); err != ErrAlreadyResolved {
		t.Fatalf("resolved: %v", err)
	}
}

func TestDeliveryFailureNoticeOmitsPrivateParserField(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := `{"messages":[],"private cedar lake":true}`
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || !strings.Contains(receipt.Error, "private cedar lake") {
		t.Fatalf("parse receipt=%+v err=%v", receipt, err)
	}
	items, err := s.InboxForUser(context.Background(), "usr_answers")
	if err != nil || len(items) != 1 || !strings.Contains(items[0].DeliveryError, "private cedar lake") {
		t.Fatalf("private parser detail absent from Inbox: %+v err=%v", items, err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 2 || strings.Contains(messages[1].Body, "private cedar lake") ||
		!strings.Contains(messages[1].Body, "correction_unavailable") {
		t.Fatalf("room notice disclosed parser field: %+v err=%v", messages, err)
	}
}

func TestCandidatePreviewIsBoundedAndRedactsCredentials(t *testing.T) {
	t.Parallel()
	preview := candidatePreview(`aak_sensitive123 Bearer tokenValue sk-1234567890 "access_token":"secretValue" ` + strings.Repeat("text ", 60))
	if len([]rune(preview)) > 241 || strings.Contains(preview, "sensitive123") || strings.Contains(preview, "tokenValue") || strings.Contains(preview, "1234567890") || strings.Contains(preview, "secretValue") {
		t.Fatalf("unsafe preview: %q", preview)
	}
}

func TestDeliveryFailureInboxShowsParentHeldReason(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, true)
	ctx := context.Background()
	first, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[`)
	if err != nil || first.Result != "correction_requested" {
		t.Fatalf("correction request=%+v %v", first, err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	child, err := s.CorrectionTurn(ctx, f.turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AbandonSpecialTurn(ctx, child.ID, "dispatch_failed"); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 || items[0].Kind != "delivery_failed" || items[0].HeldReason != "correction_unavailable" {
		t.Fatalf("delivery-failure item=%+v %v", items, err)
	}
}

func TestDeliveryFailureInboxShowsCorrectionHeldReason(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, true)
	ctx := context.Background()
	first, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[`)
	if err != nil || first.Result != "correction_requested" {
		t.Fatalf("correction request=%+v %v", first, err)
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
	second, err := applyCandidate(t, s, child, f.agent.ID, 1, `{"messages":[`)
	if err != nil || second.HeldReason != "invalid_after_correction" {
		t.Fatalf("held correction=%+v %v", second, err)
	}
	items, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 || items[0].Kind != "delivery_failed" || items[0].HeldReason != "invalid_after_correction" {
		t.Fatalf("delivery-failure item=%+v %v", items, err)
	}
}

// Dismissing a question closes it without a reply or an answer choice.
func TestDismissClosesQuestionWithoutAnswer(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	ask := fmt.Sprintf(`{"messages":[{"room":%q,"body":"choose","ask":{"kind":"question","text":"Which?","options":["A","B"],"free_text":true}}]}`, f.room.ID)
	if _, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, ask); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 || items[0].Kind != "question" {
		t.Fatalf("question inbox = %+v err=%v", items, err)
	}
	var questionSeq int64
	if err := s.db.QueryRow(`SELECT seq FROM message WHERE id=?`, items[0].MessageID).Scan(&questionSeq); err != nil {
		t.Fatal(err)
	}
	if state, err := s.MarkRoomRead(ctx, "usr_answers", f.room.ID, questionSeq-1); err != nil || state.AttentionCount != 1 {
		t.Fatalf("older page marked question seen: %+v err=%v", state, err)
	}
	if state, err := s.MarkRoomRead(ctx, "usr_answers", f.room.ID, questionSeq); err != nil || state.AttentionCount != 0 {
		t.Fatalf("reading question did not clear bell: %+v err=%v", state, err)
	}
	items, err = s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 || !items[0].Seen {
		t.Fatalf("seen question left Inbox: %+v err=%v", items, err)
	}
	if err := s.DismissInboxItem(ctx, items[0].ID, "usr_answers", int(items[0].Rev)); err != nil {
		t.Fatalf("dismissing a question: %v", err)
	}
	after, _ := s.InboxForUser(ctx, "usr_answers")
	if len(after) != 0 {
		t.Fatalf("the dismissed question stayed open: %+v", after)
	}
	var state string
	var answerID *string
	if err := s.db.QueryRow(`SELECT state,answer_message_id FROM inbox_item WHERE id=?`, items[0].ID).Scan(&state, &answerID); err != nil || state != "withdrawn" || answerID != nil {
		t.Fatalf("dismissed question state=%q answer=%v err=%v", state, answerID, err)
	}
}

// A project viewer may not dismiss a failure in an ordinary room; the owner may.
func TestDismissNeedsAnOperatorInAnOrdinaryRoom(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	if _, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[{"room":"`+f.room.ID+`","body":"held"}`); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(items) != 1 || items[0].Kind != "delivery_failed" {
		t.Fatalf("inbox = %+v err=%v", items, err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_viewer','viewer','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_viewer','viewer')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissInboxItem(ctx, items[0].ID, "usr_viewer", int(items[0].Rev)); err != ErrForbidden {
		t.Fatalf("a viewer dismissed a delivery failure: %v", err)
	}
	if err := s.DismissInboxItem(ctx, items[0].ID, "usr_answers", int(items[0].Rev)); err != nil {
		t.Fatalf("the owner could not dismiss it: %v", err)
	}
}
