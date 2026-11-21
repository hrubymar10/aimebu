package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func deliveredNotes(t *testing.T, s *Store, agentID string) []protocol.DeliveryNote {
	t.Helper()
	ctx := context.Background()
	delivery, ready, err := s.PrepareDispatch(ctx, agentID, "")
	if err != nil || !ready {
		t.Fatalf("prepare ready=%t err=%v", ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, delivery.TurnID); err != nil {
		t.Fatal(err)
	}
	return delivery.Notes
}

// A person replies to an agent's "ola" with "ola", in the room and in its thread. The agent is told why each reached it and what it
// answers: its own message, by number, author and text, not an opaque message id.
func TestNotesSayWhyAndWhatTheyAnswer(t *testing.T) {
	t.Parallel()
	s, _, room, owen, _ := wakeFixture(t)
	ctx := context.Background()
	ola, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: owen.ID, Body: "ola"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "ola", ReplyTo: ola.ID})
	if err != nil {
		t.Fatal(err)
	}
	notes := deliveredNotes(t, s, owen.ID)
	if len(notes) != 1 || notes[0].MessageID != reply.ID {
		t.Fatalf("notes = %+v", notes)
	}
	note := notes[0]
	want := protocol.NoteParent{No: ola.No, From: owen.Slug, Body: "ola", Yours: true}
	if note.InReplyTo == nil || *note.InReplyTo != want || note.Conversation != nil {
		t.Fatalf("in_reply_to = %+v, conversation = %+v; want %+v and no conversation", note.InReplyTo, note.Conversation, want)
	}
	if note.Why != fmt.Sprintf("a reply to your message #%d", ola.No) {
		t.Fatalf("why = %q", note.Why)
	}
}

// A message deep in a thread carries both the message it replies to and the thread's first message; a person's room-wide message says so.
func TestNotesCarryTheThreadStartAndAPersonsMessage(t *testing.T) {
	t.Parallel()
	s, _, room, owen, lead := wakeFixture(t)
	ctx := context.Background()
	root, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "the plan"})
	if err != nil {
		t.Fatal(err)
	}
	inThread, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: owen.ID, Body: "a question on it", ReplyTo: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "answer here", ReplyTo: inThread.ID}); err != nil {
		t.Fatal(err)
	}
	notes := deliveredNotes(t, s, owen.ID)
	last := notes[len(notes)-1]
	if last.InReplyTo == nil || last.InReplyTo.No != inThread.No || !last.InReplyTo.Yours {
		t.Fatalf("in_reply_to = %+v", last.InReplyTo)
	}
	if last.Conversation == nil || last.Conversation.No != root.No || last.Conversation.From != "lead" || last.Conversation.Yours || last.Conversation.Body != "the plan" {
		t.Fatalf("conversation = %+v", last.Conversation)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "anyone?"}); err != nil {
		t.Fatal(err)
	}
	notes = deliveredNotes(t, s, owen.ID)
	if got := notes[len(notes)-1].Why; got != "a person's message to the room" {
		t.Fatalf("why = %q", got)
	}
}
