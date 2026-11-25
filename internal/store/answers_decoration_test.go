package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// An invalid optional decoration never holds the answer: the message is posted without it and the receipt names the field and the rule.
func TestInvalidDecorationsPostTheMessageWithAReceiptWarning(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, fields, warn string }{
		{"one proposed answer", `"proposed_answers":["only"]`, "proposed_answers dropped: it needs 2 to 4 entries"},
		{"five proposed answers", `"proposed_answers":["a","b","c","d","e"]`, "proposed_answers dropped: it needs 2 to 4 entries"},
		{"both controls", `"proposed_answers":["Yes","No"],"open_questions":[{"question":"Scope?","options":["Small","Large"]}]`, "not both"},
		{"bad open questions", `"open_questions":[{"question":"Scope?","options":["Small"]}]`, "open_questions dropped"},
		{"bad visual plan", `"visual_plan":[{"type":"nope","data":{}}]`, "visual_plan block 0 dropped"},
		{"bad appendix", `"appendix_pages":[{"title":"t"}]`, "appendix_pages page 0 dropped"},
		{"ask and attention", `"ask":{"kind":"question","text":"Choose"},"needs_attention":true,"open_questions":[{"question":"Scope?","options":["Small","Large"]}]`, "ask implies attention"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, f := answerFixture(t, false)
			raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"hello",%s}]}`, f.room.ID, tc.fields)
			receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 || !strings.Contains(strings.Join(receipt.Warnings, "|"), tc.warn) {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
			if err != nil || messages[len(messages)-1].Body != "hello" {
				t.Fatalf("messages=%+v err=%v", messages, err)
			}
		})
	}
}

// A hold of a lead that its own next answer settles bothers nobody: no Inbox item and no room notice. A failed correction reaches people.
func TestLeadHoldSettledByItsNextAnswerCreatesNoInboxItemNorNotice(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "prose from a lead"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if got := openInboxKinds(t, s, "usr_answers"); len(got) != 0 {
		t.Fatalf("people see %v before the correction failed", got)
	}
	note := heldNote(t, s, f.room.ID)
	second := nextNormalTurn(t, s, f, f.turn, note.ID)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"fixed"}]}`, f.room.ID)
	if receipt, err := applyCandidate(t, s, second, f.agent.ID, 0, raw); err != nil || receipt.Result != "committed" {
		t.Fatalf("correction receipt=%+v err=%v", receipt, err)
	}
	if got := openInboxKinds(t, s, "usr_answers"); len(got) != 0 {
		t.Fatalf("people see %v after the correction settled it", got)
	}
	messages, _ := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	for _, m := range messages {
		if strings.Contains(m.Body, "Review the delivery failure") {
			t.Fatalf("room notice %q for a self-corrected hold", m.Body)
		}
	}
}

func TestLeadFailedCorrectionStillReachesPeople(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "prose"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	second := nextNormalTurn(t, s, f, f.turn, heldNote(t, s, f.room.ID).ID)
	if receipt, err := applyCandidate(t, s, second, f.agent.ID, 0, "still prose"); err != nil || receipt.Result != "held" {
		t.Fatalf("second receipt=%+v err=%v", receipt, err)
	}
	if got := strings.Join(openInboxKinds(t, s, "usr_answers"), ","); got != "delivery_failed" {
		t.Fatalf("people see %q, want delivery_failed", got)
	}
	messages, _ := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	notices := 0
	for _, m := range messages {
		if strings.Contains(m.Body, "reply was held (correction_unavailable)") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("room notices=%d, want 1: %+v", notices, messages)
	}
}

func TestWorkerHoldSettledByItsNextAnswerCreatesNoInboxItemNorNotice(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "prose from a worker"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	second := nextNormalTurn(t, s, f, f.turn, heldNote(t, s, f.room.ID).ID)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"fixed"}]}`, f.room.ID)
	if receipt, err := applyCandidate(t, s, second, f.agent.ID, 0, raw); err != nil || receipt.Result != "committed" {
		t.Fatalf("correction receipt=%+v err=%v", receipt, err)
	}
	if got := openInboxKinds(t, s, "usr_answers"); len(got) != 0 {
		t.Fatalf("people see %v", got)
	}
	messages, _ := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	for _, m := range messages {
		if strings.Contains(m.Body, "reply was held (") {
			t.Fatalf("room notice %q for a self-corrected hold", m.Body)
		}
	}
}
