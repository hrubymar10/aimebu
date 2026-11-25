package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
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

func TestMissedAgentMentionHoldsWholeAnswerAndWakesAuthor(t *testing.T) {
	for _, noAction := range []bool{false, true} {
		t.Run(fmt.Sprint(noAction), func(t *testing.T) {
			s, f := answerFixture(t, noAction)
			ctx := context.Background()
			peer, err := s.CreateAgent(ctx, Agent{Slug: "peer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
			if err != nil {
				t.Fatal(err)
			}
			task, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "peer task", CreatedBy: "usr_test"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AddMember(ctx, task.ID, "agent", peer.ID); err != nil {
				t.Fatal(err)
			}
			raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"first"},{"room":%q,"body":"@peer please review"}]}`, f.room.ID, f.room.ID)
			receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || !strings.Contains(receipt.Error, peer.ID) || strings.Contains(receipt.Error, task.ID) || !strings.Contains(receipt.Error, "dm_agent") {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if noAction {
				if receipt.Result != "correction_requested" || len(messages) != 1 {
					t.Fatalf("correction receipt=%+v messages=%+v", receipt, messages)
				}
			} else {
				if receipt.Result != "held" || receipt.HeldReason != "correction_unavailable" || len(messages) != 2 || !strings.Contains(messages[1].Body, "message out_") || strings.Contains(messages[1].Body, "@peer") || strings.Contains(messages[1].Body, task.ID) || !strings.Contains(messages[1].Body, "dm_agent") {
					t.Fatalf("held receipt=%+v messages=%+v", receipt, messages)
				}
				if _, err := s.NoteForAgentMessage(ctx, f.agent.ID, messages[1].ID); err != nil {
					t.Fatalf("author was not woken: %v", err)
				}
				replay, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
				if err != nil || replay.Result != "held" {
					t.Fatalf("replay=%+v err=%v", replay, err)
				}
				again, _ := s.ListMessagesAfter(ctx, f.room.ID, 0)
				if len(again) != len(messages) {
					t.Fatalf("replay created another note: %+v", again)
				}
			}
		})
	}
}

func TestMissedMentionForwardsOneMessageThroughSharedRoutingRoom(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	peer, err := s.CreateAgent(ctx, Agent{Slug: "peer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.CreateAgent(ctx, Agent{Slug: "third", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash3", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET title='ghi.md',restricted=1 WHERE id=?`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	routing, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "routing", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET team_flow_room_key='routing' WHERE id=?`, routing.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.agent.ID, peer.ID, third.ID} {
		if err := s.AddMember(ctx, routing.ID, "agent", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_viewer_pointer','viewer','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_viewer_pointer','viewer')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	general, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "general", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET team_flow_room_key='general' WHERE id=?`, general.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.agent.ID, peer.ID} {
		if err := s.AddMember(ctx, general.ID, "agent", id); err != nil {
			t.Fatal(err)
		}
	}
	secret, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "unrelated task detail"})
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"@peer, done: ghi.md"}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
		t.Fatalf("answer=%+v %v", receipt, err)
	}
	var reportNo int64
	if err := s.db.QueryRow(`SELECT no FROM message WHERE id=?`, receipt.MessageIDs[0]).Scan(&reportNo); err != nil {
		t.Fatal(err)
	}
	pointers, err := s.ListMessagesAfter(ctx, routing.ID, 0)
	// The read path translates the stored @agt_<id> author to the display name; the room reference keeps its id.
	if err != nil || len(pointers) != 1 || !strings.Contains(pointers[0].Body, "@"+f.agent.Slug+" mentioned you in #"+f.room.ID+": #"+fmt.Sprint(reportNo)) {
		t.Fatalf("routing pointer=%+v %v", pointers, err)
	}
	if _, err := s.NoteForAgentMessage(ctx, peer.ID, pointers[0].ID); err != nil {
		t.Fatalf("pointer did not wake recipient: %v", err)
	}
	if generalMessages, err := s.ListMessagesAfter(ctx, general.ID, 0); err != nil || len(generalMessages) != 0 {
		t.Fatalf("pointer should prefer routing: %+v %v", generalMessages, err)
	}
	if got, err := s.AgentMessageByNo(ctx, peer.ID, f.project.ID, reportNo); err != nil || !got.Available || got.Body != "@peer, done: ghi.md" {
		t.Fatalf("referenced report=%+v %v", got, err)
	}
	if got, err := s.AgentMessageByNo(ctx, third.ID, f.project.ID, reportNo); err != nil || got.Available {
		t.Fatalf("shared-room third agent read pointer target=%+v %v", got, err)
	}
	if got, err := s.ReferencedMessageForUser(ctx, "usr_viewer_pointer", pointers[0].ID, reportNo); err != nil || got.Available {
		t.Fatalf("viewer read pointer target=%+v %v", got, err)
	}
	if got, err := s.ReferencedMessageForUser(ctx, "usr_answers", pointers[0].ID, reportNo); err != nil || !got.Available {
		t.Fatalf("owner lost pointer oversight=%+v %v", got, err)
	}
	if got, err := s.AgentMessageByNo(ctx, peer.ID, f.project.ID, secret.No); err != nil || got.Available {
		t.Fatalf("unrelated task detail leaked=%+v %v", got, err)
	}
	var failures int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE turn_id=? AND kind='delivery_failed'`, f.turn.ID).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("delivery failures=%d %v", failures, err)
	}
}

func TestSharedMentionPointerKeepsOtherRoomCandidatePrivate(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	other, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "private brief", CreatedBy: "usr_answers"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, other.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	peer, err := s.CreateAgent(ctx, Agent{Slug: "privatepeer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, other.ID, "agent", peer.ID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"secret routing brief"},{"room":%q,"body":"@privatepeer please review this task"}]}`, other.ID, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) != 2 {
		t.Fatalf("turn room messages=%+v err=%v", messages, err)
	}
	if messages[1].Body != "@privatepeer please review this task" {
		t.Fatalf("original answer missing from target room: %q", messages[1].Body)
	}
	sharedMessages, err := s.ListMessagesAfter(ctx, other.ID, 0)
	if err != nil || len(sharedMessages) != 2 || sharedMessages[0].Body != "secret routing brief" ||
		!strings.Contains(sharedMessages[1].Body, fmt.Sprintf("#%d", messages[1].No)) ||
		strings.Contains(sharedMessages[1].Body, "secret routing brief") || strings.Contains(sharedMessages[1].Body, "please review this task") {
		t.Fatalf("pointer disclosed candidate text or lost the reference: %+v %v", sharedMessages, err)
	}
}

func TestMixedAgentMentionsCommitWithWarning(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	peer, err := s.CreateAgent(ctx, Agent{Slug: "presentpeer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", peer.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateAgent(ctx, Agent{Slug: "absentpeer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash2", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"@presentpeer done; @absentpeer helped"}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.Warnings) != 1 || !strings.Contains(receipt.Warnings[0], "@absentpeer") {
		t.Fatalf("mixed mentions: %+v %v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) != 2 || messages[1].AuthorKind != "agent" {
		t.Fatalf("committed message: %+v %v", messages, err)
	}
}

func TestMissedMentionHintOmitsInaccessibleRestrictedRoom(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	peer, err := s.CreateAgent(ctx, Agent{Slug: "routingpeer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	routing, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "routing secret", CreatedBy: "usr_answers"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, routing.ID, "agent", peer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET restricted=1 WHERE id=?`, routing.ID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"@routingpeer please review"}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || strings.Contains(receipt.Error, routing.ID) || strings.Contains(receipt.Error, routing.Title) || !strings.Contains(receipt.Error, "dm_agent") {
		t.Fatalf("restricted room leaked in hint: %+v %v", receipt, err)
	}
}

func TestMissedAgentMentionForceAndCode(t *testing.T) {
	for _, item := range []struct {
		name, body string
		force      bool
	}{
		{"force", "@peer please review", true},
		{"code", "`@peer` is an example", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			_, err := s.CreateAgent(context.Background(), Agent{Slug: "peer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
			if err != nil {
				t.Fatal(err)
			}
			raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":%q,"force":%t}]}`, f.room.ID, item.body, item.force)
			receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
		})
	}
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
	before, err := s.ListMessagesLatest(ctx, f.room.ID, 10)
	if err != nil || len(before) < 1 || before[len(before)-1].AnswerState != "open" {
		t.Fatalf("pending answer state=%+v err=%v", before, err)
	}
	answer, replay, err := s.AnswerInbox(ctx, items[0].ID, "usr_answers", "attention-key", "Q1) a) Small", int(items[0].Rev))
	if err != nil || replay || answer.ReplyTo != receipt.MessageIDs[0] {
		t.Fatalf("answer=%+v replay=%v err=%v", answer, replay, err)
	}
	var state, answerID string
	if err := s.db.QueryRow(`SELECT state,answer_message_id FROM inbox_item WHERE id=?`, items[0].ID).Scan(&state, &answerID); err != nil || state != "resolved" || answerID != answer.ID {
		t.Fatalf("state=%q answer=%q err=%v", state, answerID, err)
	}
	after, err := s.ListMessagesLatest(ctx, f.room.ID, 10)
	if err != nil || len(after) < 2 || after[len(after)-2].AnswerState != "resolved" || after[len(after)-2].AnswerBody != "Q1) a) Small" || after[len(after)-2].AnswerMessageNo != answer.No || after[len(after)-2].AnswerAuthorName != "owner" {
		t.Fatalf("chosen answer state=%+v err=%v", after, err)
	}
	if _, reused, err := s.AnswerInbox(ctx, items[0].ID, "usr_answers", "attention-key", "Q1) a) Small", int(items[0].Rev)); err != nil || !reused {
		t.Fatalf("idempotent replay=%v err=%v", reused, err)
	}
}

// An ask that replies into a conversation puts the person's answer in that conversation too.
func TestThreadedInboxAnswerStaysInThread(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	root, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "thread root"})
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Choose","reply_to":%q,"needs_attention":true}]}`, f.room.ID, root.ID)
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

func TestAskAndAttentionControlsAreReducedToTheAsk(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Choose","ask":{"kind":"question","text":"Choose"},"proposed_answers":["A","B"]}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.Warnings) != 1 || !strings.Contains(receipt.Warnings[0], "ask implies attention") {
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
	if err != nil || len(messages) < 2 || strings.Contains(messages[len(messages)-1].VisualPlanJSON, `"future"`) ||
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
	// A reply that posts nothing clears only its own turn's failure: the earlier one's content never reached anyone.
	if err := s.db.QueryRow(`SELECT state FROM inbox_item WHERE id='inb_unrelated_failure'`).Scan(&state); err != nil || state != "open" {
		t.Fatalf("unrelated state=%q %v", state, err)
	}
}

func TestInvalidBatchRequestsOneCorrectionThenCommits(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, true)
	// An unknown room is a store-level shape error, so the whole batch is held (and recorded) and the no-action harness is asked to correct it.
	bad := fmt.Sprintf(`{"messages":[{"room":%q,"body":"valid"},{"room":"room_unknown","body":"bad"}]}`, f.room.ID)
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

// A cross-room reply_to no longer holds the answer (it is dropped with a warning, see TestAnswerReplyToInAnotherRoomIsDroppedWithAWarning);
// these two causes still hold the reply whole.
func TestStructuredAnswerHoldsUnavailableAndClosed(t *testing.T) {
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
	} {
		t.Run(test.name, func(t *testing.T) {
			s, f := answerFixture(t, test.noAction)
			asLead(t, s, f)
			raw := test.candidate(t, s, f)
			receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || receipt.Result != test.reason && receipt.HeldReason != test.reason {
				t.Fatalf("answer=%+v error=%v", receipt, err)
			}
			messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
			wantMessages := 1
			if test.reason == "correction_unavailable" {
				wantMessages = 2 // the author's own note saying why; the room is told only if the repair fails too
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

// A plain single answer to the one person's message that woke the turn links to it without the agent saying so, and the turn's events say
// the server did it. Every other shape keeps exactly the link the agent wrote.
func TestSingleAnswerToTheWakingPersonDefaultsItsReply(t *testing.T) {
	t.Parallel()
	replyOf := func(t *testing.T, s *Store, id string) string {
		t.Helper()
		var reply sql.NullString
		if err := s.db.QueryRow(`SELECT reply_to FROM message WHERE id=?`, id).Scan(&reply); err != nil {
			t.Fatal(err)
		}
		return reply.String
	}
	run := func(t *testing.T, prepare func(*Store, executionFixture), body func(room string) string) (*Store, executionFixture, AnswerReceipt) {
		t.Helper()
		s, f := answerFixture(t, false)
		if _, err := s.db.Exec(`UPDATE message SET author_kind='user',author_id='usr_answers' WHERE id=?`, f.message.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, f.turn.ID, f.note.ID); err != nil {
			t.Fatal(err)
		}
		if prepare != nil {
			prepare(s, f)
		}
		receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, body(f.room.ID))
		if err != nil || receipt.Result != "committed" {
			t.Fatalf("receipt=%+v err=%v", receipt, err)
		}
		return s, f, receipt
	}
	plain := func(room string) string { return fmt.Sprintf(`{"messages":[{"room":%q,"body":"done"}]}`, room) }

	s, f, receipt := run(t, nil, plain)
	if got := replyOf(t, s, receipt.MessageIDs[0]); got != f.message.ID {
		t.Fatalf("reply_to=%q, want the waking message %q", got, f.message.ID)
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='reply_defaulted' AND turn_id=? AND json_extract(payload_json,'$.message_id')=?`, f.turn.ID, receipt.MessageIDs[0]).Scan(&events); err != nil || events != 1 {
		t.Fatalf("reply_defaulted events=%d err=%v", events, err)
	}

	// A handoff answers whoever briefed or nudged the worker, a person or an agent, and keeps the link back to them.
	handoff := func(room string) string {
		return fmt.Sprintf(`{"messages":[{"room":%q,"body":"over to you","handoff":true}]}`, room)
	}
	for name, prepare := range map[string]func(*Store, executionFixture){
		"handoff after a person's note": nil,
		"handoff after an agent's note": func(s *Store, f executionFixture) {
			if _, err := s.db.Exec(`UPDATE message SET author_kind='agent',author_id=? WHERE id=?`, f.agent.ID, f.message.ID); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, f, receipt := run(t, prepare, handoff)
			if got := replyOf(t, s, receipt.MessageIDs[0]); got != f.message.ID {
				t.Fatalf("reply_to=%q, want the waking message %q", got, f.message.ID)
			}
		})
	}

	var otherRoom string
	for name, c := range map[string]struct {
		prepare func(*Store, executionFixture)
		body    func(room string) string
	}{
		"two messages": {nil, func(room string) string {
			return fmt.Sprintf(`{"messages":[{"room":%q,"body":"a"},{"room":%q,"body":"b"}]}`, room, room)
		}},
		// One message to the waking room and another elsewhere is not "a single plain message back": the answer holds two messages, so nothing is linked.
		"second message to another room": {func(s *Store, f executionFixture) {
			other, err := s.CreateRoom(context.Background(), Room{ProjectID: f.project.ID, Kind: "task", Title: "elsewhere", CreatedBy: "usr_answers"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AddMember(context.Background(), other.ID, "agent", f.agent.ID); err != nil {
				t.Fatal(err)
			}
			otherRoom = other.ID
		}, func(room string) string {
			return fmt.Sprintf(`{"messages":[{"room":%q,"body":"here"},{"room":%q,"body":"there"}]}`, room, otherRoom)
		}},
		"ask": {nil, func(room string) string {
			return fmt.Sprintf(`{"messages":[{"room":%q,"body":"q","ask":{"kind":"question","text":"Which?","options":["A","B"]}}]}`, room)
		}},
		"agent note": {func(s *Store, f executionFixture) {
			if _, err := s.db.Exec(`UPDATE message SET author_kind='agent',author_id=? WHERE id=?`, f.agent.ID, f.message.ID); err != nil {
				t.Fatal(err)
			}
		}, plain},
		"two notes": {func(s *Store, f executionFixture) {
			other, err := s.InsertMessage(context.Background(), Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "usr_answers", Body: "more"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT OR IGNORE INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES ('note_second',?,?,?,'dispatched','room',?)`, f.agent.ID, other.ID, f.room.ID, timestamp()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) SELECT ?,id FROM note WHERE message_id=? AND agent_id=?`, f.turn.ID, other.ID, f.agent.ID); err != nil {
				t.Fatal(err)
			}
		}, plain},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, receipt := run(t, c.prepare, c.body)
			for _, id := range receipt.MessageIDs {
				if got := replyOf(t, s, id); got != "" {
					t.Fatalf("reply_to=%q, want none", got)
				}
			}
		})
	}

	// A reply_to the agent wrote is never replaced.
	var earlier Message
	s, _, receipt = run(t, func(s *Store, f executionFixture) {
		var err error
		if earlier, err = s.InsertMessage(context.Background(), Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "earlier"}); err != nil {
			t.Fatal(err)
		}
	}, func(room string) string {
		return fmt.Sprintf(`{"messages":[{"room":%q,"body":"done","reply_to":%q}]}`, room, earlier.ID)
	})
	if got := replyOf(t, s, receipt.MessageIDs[0]); got != earlier.ID {
		t.Fatalf("explicit reply_to=%q, want %q", got, earlier.ID)
	}
}

func TestAskWithAttentionControlsIsCommittedWithAWarning(t *testing.T) {
	t.Parallel()
	for name, controls := range map[string]string{
		"proposed answers": `"proposed_answers":["A","B"]`,
		"needs attention":  `"needs_attention":true`,
		"open questions":   `"open_questions":[{"question":"Scope?","options":["a","b"]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Choose","ask":{"kind":"question","text":"Choose","options":["X","Y"]},%s}]}`, f.room.ID, controls)
			receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || receipt.Result != "committed" || len(receipt.Warnings) != 1 || !strings.Contains(receipt.Warnings[0], "ask implies attention") {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			items, err := s.InboxForUser(context.Background(), "usr_answers")
			if err != nil || len(items) != 1 {
				t.Fatalf("items=%+v err=%v", items, err)
			}
		})
	}
}

func TestInvalidReplyWithoutCorrectionTellsTheAgentWhy(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	ctx := context.Background()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"x","ask":{"kind":"bad","text":"x"}}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || receipt.HeldReason != "correction_unavailable" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) != 2 || messages[1].AuthorKind != "system" || !strings.Contains(messages[1].Body, "Your previous reply was held: ") || !strings.Contains(messages[1].Body, "invalid ask kind") {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	if _, err := s.NoteForAgentMessage(ctx, f.agent.ID, messages[1].ID); err != nil {
		t.Fatalf("author was not woken: %v", err)
	}
	replay, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	again, _ := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || replay.Result != "held" || len(again) != len(messages) {
		t.Fatalf("replay=%+v err=%v messages=%+v", replay, err, again)
	}
}

// A committed reply of the same agent in the same room closes its earlier delivery failures, whichever turn they belong to.
func TestCommittedReplyResolvesEarlierDeliveryFailuresInTheRoom(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	ctx := context.Background()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"x","ask":{"kind":"bad","text":"x"}}]}`, f.room.ID)
	holdTwice(t, s, f, raw)
	open := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE agent_id=? AND kind='delivery_failed' AND state='open'`, f.agent.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if open() != 1 {
		t.Fatalf("open failures=%d, want 1", open())
	}
	// Another room, another turn: not this failure.
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.resolveDeliveryFailures(f.agent.ID, "trn_other", "room_other") }); err != nil || open() != 1 {
		t.Fatalf("an unrelated commit resolved the failure: open=%d err=%v", open(), err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.resolveDeliveryFailures(f.agent.ID, "trn_other", f.room.ID) }); err != nil || open() != 0 {
		t.Fatalf("a commit in the room left the failure open: open=%d err=%v", open(), err)
	}
}

// The note wakes the agent; if the turn it woke is held again, that does not make a second note (no chain), and the person still has the card.
func TestHeldReplyNoteDoesNotChain(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	asLead(t, s, f)
	ctx := context.Background()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"x","ask":{"kind":"bad","text":"x"}}]}`, f.room.ID)
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	note, err := s.NoteForAgentMessage(ctx, f.agent.ID, messages[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	turn, err := s.RecordTurn(ctx, Turn{AgentID: f.agent.ID, SessionID: "ses_test", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.ID); err != nil {
		t.Fatal(err)
	}
	policy, _ := json.Marshal(TurnPolicy{Capabilities: adapter.Capabilities{NoActionMode: false}})
	if _, err := s.db.Exec(`UPDATE turn SET policy_json=? WHERE id=?`, string(policy), turn.ID); err != nil {
		t.Fatal(err)
	}
	other := fmt.Sprintf(`{"messages":[{"room":%q,"body":"y","ask":{"kind":"bad","text":"y"}}]}`, f.room.ID)
	if receipt, err := applyCandidate(t, s, turn, f.agent.ID, 0, other); err != nil || receipt.Result != "held" {
		t.Fatalf("second receipt=%+v err=%v", receipt, err)
	}
	again, _ := s.ListMessagesAfter(ctx, f.room.ID, 0)
	notes := 0
	for _, m := range again {
		if strings.HasPrefix(m.Body, heldReplyNotePrefix) {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("held reply notes=%d, want 1 (no chain): %+v", notes, again)
	}
}

// A message that addresses nobody and wakes nobody gets its author a system note saying so, so the chain does not stop without a word. A
// message that reaches somebody (a handoff that wakes the dispatcher, a mention of a person) gets none, and a turn woken by that note gets
// no second one.
func TestMessageThatWokeNobodyNotesItsAuthor(t *testing.T) {
	t.Parallel()
	notes := func(t *testing.T, s *Store, f executionFixture) []string {
		t.Helper()
		rows, err := s.db.Query(`SELECT m.body FROM message m JOIN note n ON n.message_id=m.id WHERE m.author_kind='system' AND n.agent_id=? AND m.body LIKE ?`, f.agent.ID, noWakeNotePrefix+"%")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				t.Fatal(err)
			}
			out = append(out, body)
		}
		return out
	}
	commit := func(t *testing.T, s *Store, f executionFixture, raw string) AnswerReceipt {
		t.Helper()
		receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
		if err != nil || receipt.Result != "committed" {
			t.Fatalf("receipt=%+v err=%v", receipt, err)
		}
		return receipt
	}
	// The fixture's room is the home room, which a person reads: make it a task room of agents alone.
	agentsOnly := func(t *testing.T) (*Store, executionFixture) {
		t.Helper()
		s, f := answerFixture(t, false)
		if _, err := s.db.Exec(`UPDATE room SET kind='task' WHERE id=?`, f.room.ID); err != nil {
			t.Fatal(err)
		}
		return s, f
	}
	t.Run("alone in the room", func(t *testing.T) {
		t.Parallel()
		s, f := agentsOnly(t)
		receipt := commit(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"done"}]}`, f.room.ID))
		var no int64
		if err := s.db.QueryRow(`SELECT no FROM message WHERE id=?`, receipt.MessageIDs[0]).Scan(&no); err != nil {
			t.Fatal(err)
		}
		got := notes(t, s, f)
		if len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("#%d", no)) {
			t.Fatalf("notes=%q, want one naming #%d", got, no)
		}
	})
	t.Run("a handoff the dispatcher receives", func(t *testing.T) {
		t.Parallel()
		s, f := agentsOnly(t)
		dispatcher, err := s.CreateAgent(context.Background(), Agent{Slug: "disp", ProjectID: f.project.ID, RoleID: "role_dispatcher_default", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(context.Background(), f.room.ID, "agent", dispatcher.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE project SET team_flow_key='leader-dispatcher' WHERE id=?`, f.project.ID); err != nil {
			t.Fatal(err)
		}
		receipt := commit(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"done","handoff":true}]}`, f.room.ID))
		if got := wakeReasons(t, s, receipt.MessageIDs[0]); !sameWakes(got, map[string]string{"disp": "handoff"}) {
			t.Fatalf("handoff woke %v, want the dispatcher", got)
		}
		if got := notes(t, s, f); len(got) != 0 {
			t.Fatalf("notes=%q, want none", got)
		}
	})
	t.Run("a lead's message", func(t *testing.T) {
		t.Parallel()
		s, f := agentsOnly(t)
		if _, err := s.db.Exec(`UPDATE role SET cap_is_lead=1 WHERE id=(SELECT role_id FROM agent WHERE id=?)`, f.agent.ID); err != nil {
			t.Fatal(err)
		}
		commit(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"status"}]}`, f.room.ID))
		if got := notes(t, s, f); len(got) != 0 {
			t.Fatalf("notes=%q, want none for a lead", got)
		}
	})
	t.Run("a room with a human member", func(t *testing.T) {
		t.Parallel()
		s, f := agentsOnly(t)
		if err := s.AddMember(context.Background(), f.room.ID, "user", "usr_answers"); err != nil {
			t.Fatal(err)
		}
		commit(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"status"}]}`, f.room.ID))
		if got := notes(t, s, f); len(got) != 0 {
			t.Fatalf("notes=%q, want none where a person reads the room", got)
		}
	})
	t.Run("the home room", func(t *testing.T) {
		t.Parallel()
		s, f := answerFixture(t, false)
		commit(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"status"}]}`, f.room.ID))
		if got := notes(t, s, f); len(got) != 0 {
			t.Fatalf("notes=%q, want none in the home room", got)
		}
	})
	t.Run("a mention of a person", func(t *testing.T) {
		t.Parallel()
		s, f := agentsOnly(t)
		commit(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"@owner done"}]}`, f.room.ID))
		if got := notes(t, s, f); len(got) != 0 {
			t.Fatalf("notes=%q, want none", got)
		}
	})
	t.Run("a turn woken by the note", func(t *testing.T) {
		t.Parallel()
		s, f := agentsOnly(t)
		earlier, err := s.InsertMessage(context.Background(), Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: noWakeNotePrefix + "#1 reached nobody", OnlyTargets: []string{f.agent.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) SELECT ?,id FROM note WHERE message_id=? AND agent_id=?`, f.turn.ID, earlier.ID, f.agent.ID); err != nil {
			t.Fatal(err)
		}
		commit(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"done"}]}`, f.room.ID))
		if got := notes(t, s, f); len(got) != 1 {
			t.Fatalf("notes=%q, want only the one that woke the turn", got)
		}
	})
}

// A mention that two agents of the room answer to addresses neither, so its author is told at once, with the full IDs that name each one,
// even in a room a person reads (where the woke-nobody note stays quiet). An agent that shares its name only with a person is addressed with
// that person, and its author gets no such note.
func TestAmbiguousMentionNotesItsAuthor(t *testing.T) {
	t.Parallel()
	ambiguityNotes := func(t *testing.T, s *Store, f executionFixture) []string {
		t.Helper()
		rows, err := s.db.Query(`SELECT m.body FROM message m JOIN note n ON n.message_id=m.id WHERE m.author_kind='system' AND n.agent_id=? AND m.body LIKE ?`, f.agent.ID, ambiguousNotePrefix+"%")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				t.Fatal(err)
			}
			out = append(out, body)
		}
		return out
	}
	bob := func(t *testing.T, s *Store, projectID string, slug ...string) Agent {
		t.Helper()
		name := "bob"
		if len(slug) > 0 {
			name = slug[0]
		}
		agent, err := s.CreateAgent(context.Background(), Agent{Slug: name, ProjectID: projectID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}
	t.Run("two agents share the slug", func(t *testing.T) {
		t.Parallel()
		s, f := answerFixture(t, false)
		ctx := context.Background()
		other, err := s.CreateProject(ctx, Project{Name: "other", Folder: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		here, there := bob(t, s, f.project.ID), bob(t, s, f.project.ID, "bob-elsewhere")
		if _, err := s.db.Exec(`UPDATE agent SET project_id=?,slug='bob' WHERE id=?`, other.ID, there.ID); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{here.ID, there.ID} {
			if err := s.AddMember(ctx, f.room.ID, "agent", id); err != nil {
				t.Fatal(err)
			}
		}
		receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, fmt.Sprintf(`{"messages":[{"room":%q,"body":"@bob please look"}]}`, f.room.ID))
		if err != nil || receipt.Result != "committed" {
			t.Fatalf("receipt=%+v err=%v", receipt, err)
		}
		if got := wakeReasons(t, s, receipt.MessageIDs[0]); len(got) != 0 {
			t.Fatalf("an ambiguous mention woke %v", got)
		}
		var no int64
		if err := s.db.QueryRow(`SELECT no FROM message WHERE id=?`, receipt.MessageIDs[0]).Scan(&no); err != nil {
			t.Fatal(err)
		}
		got := ambiguityNotes(t, s, f)
		if len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("#%d", no)) || !strings.Contains(got[0], "@bob is ambiguous") ||
			!strings.Contains(got[0], "@"+here.ID) || !strings.Contains(got[0], "@"+there.ID) {
			t.Fatalf("notes=%q, want one naming #%d and both full IDs", got, no)
		}
	})
	t.Run("an agent and a person share the name", func(t *testing.T) {
		t.Parallel()
		s, f := answerFixture(t, false)
		ctx := context.Background()
		agent := bob(t, s, f.project.ID)
		if err := s.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_bob','bob','hash','{}','now')`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_bob','owner')`, f.project.ID); err != nil {
			t.Fatal(err)
		}
		receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, fmt.Sprintf(`{"messages":[{"room":%q,"body":"@bob please look"}]}`, f.room.ID))
		if err != nil || receipt.Result != "committed" {
			t.Fatalf("receipt=%+v err=%v", receipt, err)
		}
		if got := wakeReasons(t, s, receipt.MessageIDs[0]); !sameWakes(got, map[string]string{"bob": "mention"}) {
			t.Fatalf("woke %v, want the agent by mention", got)
		}
		if got := ambiguityNotes(t, s, f); len(got) != 0 {
			t.Fatalf("notes=%q, want none", got)
		}
	})
}
