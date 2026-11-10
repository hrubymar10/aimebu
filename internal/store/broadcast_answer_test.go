package store

import (
	"context"
	"fmt"
	"testing"
)

// What an agent writes in a turn that was woken by a person in the room is said to that person, not to the room: two notes from the person and an
// answer of two messages that mention nobody (so no reply link can be defaulted) wake no other agent. The same two messages from a turn that was not
// woken by a person are broadcasts and do.
func TestAnswerToAPersonIsNotABroadcast(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		personNotes int
		wantPeer    bool
	}{
		"two notes from a person":                        {2, false},
		"one note from a person":                         {1, false},
		"no note from a person (turn woken by an agent)": {0, true},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			ctx := context.Background()
			peer, err := s.CreateAgent(ctx, Agent{Slug: "peer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AddMember(ctx, f.room.ID, "agent", peer.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, f.turn.ID, f.note.ID); err != nil {
				t.Fatal(err)
			}
			author, authorID := "agent", f.agent.ID
			if tc.personNotes > 0 {
				author, authorID = "user", "usr_answers"
			}
			if _, err := s.db.Exec(`UPDATE message SET author_kind=?,author_id=? WHERE id=?`, author, authorID, f.message.ID); err != nil {
				t.Fatal(err)
			}
			for i := 1; i < tc.personNotes; i++ {
				more, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "usr_answers", Body: fmt.Sprintf("more %d", i)})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) SELECT ?,id FROM note WHERE message_id=? AND agent_id=?`, f.turn.ID, more.ID, f.agent.ID); err != nil {
					t.Fatal(err)
				}
			}
			raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"status: half way"},{"room":%q,"body":"result: done"}]}`, f.room.ID, f.room.ID)
			receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
			if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 2 {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			for _, id := range receipt.MessageIDs {
				_, err := s.NoteForAgentMessage(ctx, peer.ID, id)
				if (err == nil) != tc.wantPeer {
					t.Fatalf("peer woken=%t want=%t for message %s", err == nil, tc.wantPeer, id)
				}
			}
		})
	}
}
