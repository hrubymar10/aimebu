package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// An answer may name the message it replies to by its number, as a JSON number or the "#N" form, and the reply is stored to that message.
func TestAnswerReplyToAcceptsMessageNumbers(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	number := func(t *testing.T, turn Turn, raw string) string {
		t.Helper()
		receipt, err := applyCandidate(t, s, turn, f.agent.ID, 0, raw)
		if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
			t.Fatalf("receipt=%+v err=%v", receipt, err)
		}
		var reply sql.NullString
		if err := s.db.QueryRow(`SELECT reply_to FROM message WHERE id=?`, receipt.MessageIDs[0]).Scan(&reply); err != nil {
			t.Fatal(err)
		}
		return reply.String
	}
	if got := number(t, f.turn, fmt.Sprintf(`{"messages":[{"room":%q,"body":"done","reply_to":%d}]}`, f.room.ID, f.message.No)); got != f.message.ID {
		t.Fatalf("numeric reply_to=%q, want the waking message %q", got, f.message.ID)
	}
	second := nextNormalTurn(t, s, f, f.turn, "")
	if got := number(t, second, fmt.Sprintf(`{"messages":[{"room":%q,"body":"done","reply_to":"#%d"}]}`, f.room.ID, f.message.No)); got != f.message.ID {
		t.Fatalf("hash reply_to=%q, want the waking message %q", got, f.message.ID)
	}
}

// A reply_to that points into another room (whether the agent may post there or not), or that names no message, never holds the answer:
// the message is posted without the reply link and the receipt carries a warning saying why.
func TestAnswerReplyToInAnotherRoomIsDroppedWithAWarning(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	joined, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "joined", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, joined.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	joinedMessage, err := s.InsertMessage(ctx, Message{RoomID: joined.ID, AuthorKind: "system", AuthorID: "system", Body: "there"})
	if err != nil {
		t.Fatal(err)
	}
	strange, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "strange", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	strangeMessage, err := s.InsertMessage(ctx, Message{RoomID: strange.ID, AuthorKind: "system", AuthorID: "system", Body: "elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	dropped := func(t *testing.T, turn Turn, raw, want string) {
		t.Helper()
		receipt, err := applyCandidate(t, s, turn, f.agent.ID, 0, raw)
		if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
			t.Fatalf("receipt=%+v err=%v", receipt, err)
		}
		var reply sql.NullString
		if err := s.db.QueryRow(`SELECT reply_to FROM message WHERE id=?`, receipt.MessageIDs[0]).Scan(&reply); err != nil {
			t.Fatal(err)
		}
		if reply.Valid {
			t.Fatalf("message kept a reply link %q, want none", reply.String)
		}
		found := false
		for _, warning := range receipt.Warnings {
			found = found || strings.Contains(warning, want)
		}
		if !found {
			t.Fatalf("warnings=%q, want one saying %q", receipt.Warnings, want)
		}
	}
	dropped(t, f.turn, fmt.Sprintf(`{"messages":[{"room":%q,"body":"x","reply_to":%d}]}`, f.room.ID, joinedMessage.No),
		fmt.Sprintf("reply_to %d dropped: message is in another room", joinedMessage.No))
	second := nextNormalTurn(t, s, f, f.turn, "")
	dropped(t, second, fmt.Sprintf(`{"messages":[{"room":%q,"body":"x","reply_to":%d}]}`, f.room.ID, strangeMessage.No),
		fmt.Sprintf("reply_to %d dropped: message is in a room you are not a member of", strangeMessage.No))
	third := nextNormalTurn(t, s, f, second, "")
	dropped(t, third, fmt.Sprintf(`{"messages":[{"room":%q,"body":"x","reply_to":"msg_%s"}]}`, f.room.ID, strings.Repeat("gone", 20)),
		"dropped: no message carries that reference")
}

// The held-reply note states the exact cause of each common fault: which message, which field, what was wrong and how to fix it, without
// quoting anything the agent wrote.
func TestHeldReplyNoteCarriesTheExactCause(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, candidate, want string
	}{
		{"unknown field", `{"messages":[],"source":"memory"}`, "the reply object has an unknown field source"},
		{"wrong field type", `{"messages":[{"room":"ROOM","body":"b","handoff":"yes"}]}`, "message 0: handoff must be true or false"},
		{"text around the object", `All done. {"messages":[{"room":"ROOM","body":"b"}]}`, "the reply has text around the JSON object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			raw := strings.ReplaceAll(c.candidate, "ROOM", f.room.ID)
			if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw); err != nil || receipt.Result != "held" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			note := heldNote(t, s, f.room.ID)
			if !strings.Contains(note.Body, c.want) || strings.Contains(note.Body, `"`) {
				t.Fatalf("note=%q, want it to name %q and to quote nothing of the answer", note.Body, c.want)
			}
		})
	}
}

// The lead's answer_held recovery carries the reason the answer was held, and the retry correction note repeats it to the worker.
func TestAnswerHeldRecoveryCarriesTheReason(t *testing.T) {
	t.Parallel()
	s, f, _, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, "not json"); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	second := nextNormalTurn(t, s, f, f.turn, heldNote(t, s, f.room.ID).ID)
	if receipt, err := applyCandidate(t, s, second, f.agent.ID, 0, "still not json"); err != nil || receipt.Result != "held" {
		t.Fatalf("second receipt=%+v err=%v", receipt, err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 || recoveries[0].Reason != answerHeldReason {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	if !strings.Contains(recoveries[0].Evidence, "the reply has text around the JSON object") {
		t.Fatalf("recovery evidence=%q, want the held reason in it", recoveries[0].Evidence)
	}
	var leadNote string
	if err := s.db.QueryRow(`SELECT m.body FROM note n JOIN message m ON m.id=n.message_id WHERE n.agent_id=? AND n.wake_reason='worker_recovery'
		ORDER BY n.id DESC LIMIT 1`, lead.ID).Scan(&leadNote); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(leadNote, "Reason: the reply has text around the JSON object") {
		t.Fatalf("lead note=%q, want the held reason as evidence", leadNote)
	}
	if _, _, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "k-retry", "retry", "", recoveries[0].Rev); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var retryNote string
	if err := s.db.QueryRow(`SELECT body FROM message WHERE room_id=? AND body LIKE ? ORDER BY seq DESC LIMIT 1`, f.room.ID, heldReplyNotePrefix+"%").Scan(&retryNote); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(retryNote, "could not be delivered: the reply has text around the JSON object") {
		t.Fatalf("retry note=%q, want the held reason in it", retryNote)
	}
}
