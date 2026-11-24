package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// structuredCard writes an agent's message with proposed answers (an attention card, no explicit ask) and returns its inbox item.
func structuredCard(t *testing.T, s *Store, f executionFixture) (Message, string, int) {
	t.Helper()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Pick","needs_attention":true,"proposed_answers":["A","B"]}]}`, f.room.ID)
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw); err != nil || receipt.Result != "committed" {
		t.Fatalf("card receipt=%+v err=%v", receipt, err)
	}
	var id, messageID string
	var rev int
	if err := s.db.QueryRow(`SELECT id,message_id,rev FROM inbox_item WHERE room_id=? ORDER BY created_at DESC LIMIT 1`, f.room.ID).Scan(&id, &messageID, &rev); err != nil {
		t.Fatal(err)
	}
	return Message{ID: messageID}, id, rev
}

// A card with structured choices keeps them until answered however it left the Inbox: a dismissed (withdrawn) one is still listed for the chat and an
// answer resolves it, as for a superseded one. The Inbox itself (the open items) does not list it.
func TestWithdrawnStructuredCardStaysAnswerable(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	message, id, rev := structuredCard(t, s, f)
	if err := s.DismissInboxItem(ctx, id, "usr_answers", rev); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := inboxState(t, s, id); state != "withdrawn" {
		t.Fatalf("state after dismiss = %q", state)
	}
	if open, err := s.InboxForUser(ctx, "usr_answers"); err != nil || len(open) != 0 {
		t.Fatalf("Inbox (open items) = %+v err=%v", open, err)
	}
	kept, err := s.SupersededAsksForUser(ctx, "usr_answers")
	if err != nil || len(kept) != 1 || kept[0].ID != id || kept[0].State != "withdrawn" || kept[0].MessageID != message.ID {
		t.Fatalf("answerable list = %+v err=%v", kept, err)
	}
	answer, _, err := s.AnswerInbox(ctx, id, "usr_answers", "late", "A", int(kept[0].Rev))
	if err != nil {
		t.Fatalf("answer of a withdrawn structured card: %v", err)
	}
	if answer.ReplyTo != message.ID {
		t.Fatalf("answer replies to %q, want the card's message %q", answer.ReplyTo, message.ID)
	}
	if state, _, _ := inboxState(t, s, id); state != "resolved" {
		t.Fatalf("state after answer = %q", state)
	}
	if _, _, err := s.AnswerInbox(ctx, id, "usr_answers", "again", "B", int(kept[0].Rev)+1); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second answer: %v", err)
	}
	if kept, err := s.SupersededAsksForUser(ctx, "usr_answers"); err != nil || len(kept) != 0 {
		t.Fatalf("answered card still listed: %+v err=%v", kept, err)
	}
}

// What cannot receive an answer stays final: a closed room, a removed agent, and a card without structured choices.
func TestWithdrawnCardStaysFinalWhenNothingCanAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *Store, f executionFixture, id string)
	}{
		{"room closed", func(t *testing.T, s *Store, f executionFixture, _ string) {
			if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, f.room.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"agent removed", func(t *testing.T, s *Store, f executionFixture, _ string) {
			if _, err := s.db.Exec(`UPDATE agent SET revoked_at='now' WHERE id=?`, f.agent.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"no structured choices", func(t *testing.T, s *Store, _ executionFixture, id string) {
			if _, err := s.db.Exec(`UPDATE message SET proposed_answers_json=NULL WHERE id=(SELECT message_id FROM inbox_item WHERE id=?)`, id); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			ctx := context.Background()
			_, id, rev := structuredCard(t, s, f)
			if err := s.DismissInboxItem(ctx, id, "usr_answers", rev); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, s, f, id)
			if kept, err := s.SupersededAsksForUser(ctx, "usr_answers"); err != nil || len(kept) != 0 {
				t.Fatalf("listed: %+v err=%v", kept, err)
			}
			if _, _, err := s.AnswerInbox(ctx, id, "usr_answers", "late", "A", rev+1); !errors.Is(err, ErrAlreadyWithdrawn) {
				t.Fatalf("answer: %v", err)
			}
		})
	}
}

// The Inbox's own rules do not change: it lists the open cards only, an agent's newer card in a room replaces its older ones (they leave the Inbox
// as superseded, still answerable in the chat), and a card dismissed from the Inbox leaves it for good. Keeping choices answerable in the chat adds
// nothing to the Inbox's list or counts.
func TestInboxStillListsOnlyTheLatestOpenCard(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	_, older, _ := structuredCard(t, s, f)
	newer, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: f.agent.ID, Body: "Newer", NeedsAttention: true, ProposedAnswersJSON: `["C","D"]`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,agent_id,requester_kind,requester_id,kind,created_at)
		VALUES ('inb_newer',?,?,?,?,'agent',?,'question','2999-01-01T00:00:00.000000000Z')`, f.project.ID, f.room.ID, newer.ID, f.agent.ID, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	open, err := s.InboxForUser(ctx, "usr_answers")
	if err != nil || len(open) != 1 || open[0].ID == older || open[0].State != "open" {
		t.Fatalf("Inbox = %+v err=%v, want only the newer open card", open, err)
	}
	if state, _, _ := inboxState(t, s, older); state != "superseded" {
		t.Fatalf("older card = %q, want superseded", state)
	}
	if err := s.DismissInboxItem(ctx, open[0].ID, "usr_answers", int(open[0].Rev)); err != nil {
		t.Fatal(err)
	}
	if open, err := s.InboxForUser(ctx, "usr_answers"); err != nil || len(open) != 0 {
		t.Fatalf("Inbox after dismissing the latest = %+v err=%v", open, err)
	}
	kept, err := s.SupersededAsksForUser(ctx, "usr_answers")
	if err != nil || len(kept) != 2 {
		t.Fatalf("chat list = %+v err=%v, want both cards kept answerable", kept, err)
	}
}
