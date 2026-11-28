package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// otherProjectRoom makes a room in a second project, which the fixture's agent is not part of.
func otherProjectRoom(t *testing.T, s *Store) Room {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_other','other','/tmp/other','now')`); err != nil {
		t.Fatal(err)
	}
	room, err := s.CreateRoom(context.Background(), Room{ProjectID: "prj_other", Kind: "task", Title: "elsewhere", CreatedBy: "usr_answers"})
	if err != nil {
		t.Fatal(err)
	}
	return room
}

func TestMixedReplyDeliversValidMessageAndHoldsOnlyOtherProjectRoom(t *testing.T) {
	t.Parallel()
	for _, noAction := range []bool{false, true} {
		t.Run(fmt.Sprint(noAction), func(t *testing.T) {
			s, f := answerFixture(t, noAction)
			ctx := context.Background()
			other := otherProjectRoom(t, s)
			raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"hello person"},{"room":%q,"body":"hello agent over there"}]}`, f.room.ID, other.ID)
			receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 || len(receipt.HeldMessages) != 1 ||
				receipt.HeldMessages[0].Index != 1 || receipt.HeldMessages[0].Reason != "not_member" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			var delivered, row, note string
			for _, m := range messages {
				switch {
				case m.AuthorKind == "agent":
					delivered = m.Body
				case strings.HasPrefix(m.Body, partialHeldNotePrefix):
					note = m.Body
				case strings.Contains(m.Body, "partly delivered"):
					row = m.Body
				}
			}
			if delivered != "hello person" {
				t.Fatalf("valid message not delivered: %+v", messages)
			}
			if note == "" || !strings.Contains(note, "`dm_agent`") || !strings.Contains(note, "another project") || strings.Contains(note, "hello agent over there") || !strings.Contains(note, "must not be sent again") {
				t.Fatalf("note=%q", note)
			}
			if !strings.Contains(row, "message 2 of 2") || !strings.Contains(row, "the other 1 delivered") || !strings.Contains(row, "The author was asked to resend it.") || strings.Contains(row, "Inbox") {
				t.Fatalf("system row=%q", row)
			}
			if got, _ := s.ListMessagesAfter(ctx, other.ID, 0); len(got) != 0 {
				t.Fatalf("message reached the other project: %+v", got)
			}
			var held, committed int
			s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE turn_id=? AND state='held' AND held_reason='not_member' AND idx=1`, f.turn.ID).Scan(&held)
			s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE turn_id=? AND state='committed' AND idx=0`, f.turn.ID).Scan(&committed)
			if held != 1 || committed != 1 {
				t.Fatalf("outbox held=%d committed=%d", held, committed)
			}
			if items, err := s.InboxForUser(ctx, "usr_answers"); err != nil || len(items) != 0 {
				t.Fatalf("a partial hold must raise no inbox item: %+v err=%v", items, err)
			}
			settle, err := s.DeliveryState(ctx, f.turn.ID)
			if err != nil || settle.Settle != "settled" {
				t.Fatalf("settle=%+v %v", settle, err)
			}

			// Replaying the same answer returns the stored receipt and writes nothing more.
			again, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || again.Result != "committed" || len(again.MessageIDs) != 1 || again.MessageIDs[0] != receipt.MessageIDs[0] {
				t.Fatalf("replay=%+v err=%v", again, err)
			}
			after, _ := s.ListMessagesAfter(ctx, f.room.ID, 0)
			if len(after) != len(messages) {
				t.Fatalf("replay wrote messages: %d -> %d", len(messages), len(after))
			}
		})
	}
}

func TestPartialHoldAfterHeldReplyNoteRaisesCardWithoutAnotherNote(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	noteMessage, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: heldReplyNotePrefix + "retry", OnlyTargets: []string{f.agent.ID}})
	if err != nil {
		t.Fatal(err)
	}
	note, err := s.NoteForAgentMessage(ctx, f.agent.ID, noteMessage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, f.turn.ID, note.ID); err != nil {
		t.Fatal(err)
	}
	other := otherProjectRoom(t, s)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"delivered"},{"room":%q,"body":"refused"}]}`, f.room.ID, other.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.HeldMessages) != 1 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	var nextNotes, cards int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND body LIKE ?`, f.room.ID, partialHeldNotePrefix+"%").Scan(&nextNotes); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE kind='delivery_failed'`).Scan(&cards); err != nil {
		t.Fatal(err)
	}
	if nextNotes != 0 || cards != 1 {
		t.Fatalf("next held notes=%d, delivery cards=%d", nextNotes, cards)
	}
}

func TestMixedReplyHoldsClosedRoomAloneAndAllBadStaysWhole(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	closed, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "closing", CreatedBy: "usr_answers"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closing' WHERE id=?`, closed.ID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"to closed"},{"room":%q,"body":"to open"}]}`, closed.ID, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.HeldMessages) != 1 || receipt.HeldMessages[0].Reason != "room_closed" || receipt.HeldMessages[0].Index != 0 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestAllMessagesRefusedHoldsWholeAnswer(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	other := otherProjectRoom(t, s)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"only elsewhere"}]}`, other.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || receipt.HeldReason != "not_member" || len(receipt.HeldMessages) != 0 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestMixedReplyDeliversWhatItCanAroundAHeldTarget(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	// As a lead, so the held target also reaches people (a worker's goes back to it as a note only).
	asLead(t, s, f)
	other := otherProjectRoom(t, s)
	// A bad target beside a dropped reply reference delivers what it can: the bad target is held alone, the messages with a
	// reference that names nothing post without the link and the receipt warns.
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"ok"},{"room":%q,"body":"elsewhere"},{"room":%q,"body":"x","reply_to":"msg_nope"}]}`, f.room.ID, other.ID, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 2 || len(receipt.HeldMessages) != 1 || receipt.HeldMessages[0].Index != 1 || receipt.HeldMessages[0].Reason != "not_member" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	found := false
	for _, warning := range receipt.Warnings {
		found = found || strings.Contains(warning, "reply_to msg_nope dropped: no message carries that reference")
	}
	if !found {
		t.Fatalf("warnings=%q, want the dropped-reference warning", receipt.Warnings)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 5 { // the seed message, "ok", "x" (its dropped reference), the partial-held row, the author's note
		t.Fatalf("messages=%+v", messages)
	}
	s2, f2 := answerFixture(t, false)
	bad, err := applyCandidate(t, s2, f2.turn, f2.agent.ID, 0, `{"messages":[`)
	if err != nil || bad.Result != "held" {
		t.Fatalf("malformed=%+v err=%v", bad, err)
	}
}
