package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func askForSupersession(t *testing.T, s *Store, f executionFixture) InboxEntry {
	t.Helper()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Choose","ask":{"kind":"question","text":"Which?","options":["A","B"]}}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("ask receipt=%+v err=%v", receipt, err)
	}
	items, err := s.InboxForUser(context.Background(), "usr_answers")
	if err != nil || len(items) != 1 {
		t.Fatalf("open ask=%+v err=%v", items, err)
	}
	return items[0]
}

func inboxState(t *testing.T, s *Store, id string) (string, int, string) {
	t.Helper()
	var state, by string
	var rev int
	if err := s.db.QueryRow(`SELECT state,rev,COALESCE(resolved_by,'') FROM inbox_item WHERE id=?`, id).Scan(&state, &rev, &by); err != nil {
		t.Fatal(err)
	}
	return state, rev, by
}

func TestNewRoomMessageSupersedesOnlyItsOpenAsk(t *testing.T) {
	for _, tc := range []struct {
		name, kind, author string
		otherRoom, closed  bool
	}{
		{"person", "user", "usr_answers", false, true},
		{"asking agent", "agent", "asker", false, true},
		{"other agent", "agent", "other agent", false, false},
		{"system", "system", "system", false, false},
		{"other room", "user", "usr_answers", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			ctx := context.Background()
			item := askForSupersession(t, s, f)
			roomID := f.room.ID
			if tc.otherRoom {
				other, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "Other", CreatedBy: "usr_answers"})
				if err != nil {
					t.Fatal(err)
				}
				roomID = other.ID
			}
			author := tc.author
			if author == "asker" {
				author = f.agent.ID
			}
			if author == "other agent" {
				other, err := s.CreateAgent(ctx, Agent{Slug: "peer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_answers"})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.AddMember(ctx, f.room.ID, "agent", other.ID); err != nil {
					t.Fatal(err)
				}
				author = other.ID
			}
			if _, err := s.InsertMessage(ctx, Message{RoomID: roomID, AuthorKind: tc.kind, AuthorID: author, Body: "New message"}); err != nil {
				t.Fatal(err)
			}
			state, rev, by := inboxState(t, s, item.ID)
			wantState, wantRev := "open", 0
			if tc.closed {
				wantState, wantRev = "superseded", 1
			}
			if state != wantState || rev != wantRev || tc.closed && by != author {
				t.Fatalf("item state=%q rev=%d by=%q; want %q rev=%d", state, rev, by, wantState, wantRev)
			}
			items, err := s.InboxForUser(ctx, "usr_answers")
			if err != nil || len(items) != 1-boolInt(tc.closed) {
				t.Fatalf("inbox=%+v err=%v", items, err)
			}
			if tc.closed {
				messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, message := range messages {
					if message.ID == item.MessageID && message.AnswerState == "superseded" {
						found = true
					}
				}
				if !found {
					t.Fatal("ask card did not carry superseded state")
				}
				// A superseded ask is not over: the Inbox hides it while an answer still resolves it and wakes the asking agent.
				answer, reused, err := s.AnswerInbox(ctx, item.ID, "usr_answers", "late-answer", "A", wantRev)
				if err != nil || reused {
					t.Fatalf("late answer: reused=%v err=%v", reused, err)
				}
				state, rev, by = inboxState(t, s, item.ID)
				if state != "resolved" || rev != wantRev+1 || by != "usr_answers" {
					t.Fatalf("answered item state=%q rev=%d by=%q", state, rev, by)
				}
				if answer.ReplyTo != item.MessageID {
					t.Fatalf("answer reply_to=%q, want ask %q", answer.ReplyTo, item.MessageID)
				}
				var answerID string
				if err := s.db.QueryRow(`SELECT answer_message_id FROM inbox_item WHERE id=?`, item.ID).Scan(&answerID); err != nil {
					t.Fatal(err)
				}
				if answerID != answer.ID {
					t.Fatalf("answer_message_id=%q, want %q", answerID, answer.ID)
				}
				if _, err := s.NoteForAgentMessage(ctx, f.agent.ID, answer.ID); err != nil {
					t.Fatalf("asking agent was not woken: %v", err)
				}
				// Resolved stays final: a second answer is refused exactly as for an open ask answered twice.
				if _, _, err := s.AnswerInbox(ctx, item.ID, "usr_answers", "late-answer-2", "B", rev); !errors.Is(err, ErrAlreadyResolved) {
					t.Fatalf("second late answer: %v", err)
				}
				items, err := s.InboxForUser(ctx, "usr_answers")
				if err != nil || len(items) != 0 {
					t.Fatalf("resolved ask in inbox=%+v err=%v", items, err)
				}
			}
		})
	}
}

// The answer guard now admits a superseded ask; withdrawn stays final and refuses one.
func TestAnswerRefusesWithdrawnAsk(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	item := askForSupersession(t, s, f)
	if err := s.DismissInboxItem(ctx, item.ID, "usr_answers", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerInbox(ctx, item.ID, "usr_answers", "after-withdraw", "A", 1); !errors.Is(err, ErrAlreadyWithdrawn) {
		t.Fatalf("answer after withdraw: %v", err)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestSupersessionMigrationClosesOnlyQualifyingAsks(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	item := askForSupersession(t, s, f)
	if _, err := s.db.Exec(`INSERT INTO inbox_item_seen(item_id,user_id,seen_at) VALUES (?,'usr_answers','now')`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "usr_answers", Body: "Later"}); err != nil {
		t.Fatal(err)
	}
	stillOpen, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: f.agent.ID, Body: "Another ask", AskJSON: `{"kind":"question","text":"Still?"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,agent_id,requester_kind,requester_id,kind,created_at)
		VALUES ('inb_migration_open',?,?,?,?,'agent',?,'question','now')`, f.project.ID, f.room.ID, stillOpen.ID, f.agent.ID, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "Notice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE inbox_item SET state='open',rev=0,resolved_by=NULL,resolved_at=NULL WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	rewindWorkerRecoveryMigration(t, s)
	if _, err := s.db.Exec(`ALTER TABLE agent DROP COLUMN approved_by`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version>=117`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	state, rev, by := inboxState(t, s, item.ID)
	if state != "superseded" || rev != 1 || by != "newer message" {
		t.Fatalf("migrated state=%q rev=%d by=%q", state, rev, by)
	}
	state, rev, _ = inboxState(t, s, "inb_migration_open")
	if state != "open" || rev != 0 {
		t.Fatalf("system-only later message changed ask: %q rev=%d", state, rev)
	}
	var seen, violations int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item_seen WHERE item_id=?`, item.ID).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if seen != 1 || violations != 0 {
		t.Fatalf("seen=%d foreign key violations=%d", seen, violations)
	}
}
