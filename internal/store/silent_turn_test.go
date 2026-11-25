package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// silentFixture is an answer fixture with a second agent, boss, in the worker's room: the one that addresses the worker.
type silentFixture struct {
	s    *Store
	f    executionFixture
	boss Agent
}

func newSilentFixture(t *testing.T) silentFixture {
	t.Helper()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	boss, err := s.CreateAgent(ctx, Agent{Slug: "boss", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash_boss", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", boss.ID); err != nil {
		t.Fatal(err)
	}
	return silentFixture{s, f, boss}
}

// turnWith records a new normal turn for the worker carrying one note: a message by authorKind/authorID that reached the worker for reason.
func (sf silentFixture) turnWith(t *testing.T, authorKind, authorID, reason string) Turn {
	t.Helper()
	ctx := context.Background()
	if _, err := sf.s.db.Exec(`UPDATE turn SET exec='exited' WHERE agent_id=?`, sf.f.agent.ID); err != nil {
		t.Fatal(err)
	}
	// OnlyTargets empty: the message wakes nobody by itself, so the note below is the only one and carries the reason under test.
	message, err := sf.s.InsertMessage(ctx, Message{RoomID: sf.f.room.ID, AuthorKind: authorKind, AuthorID: authorID, Body: "@worker please do it", OnlyTargets: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	note, err := sf.s.InsertNote(ctx, Note{AgentID: sf.f.agent.ID, MessageID: message.ID, RoomID: sf.f.room.ID, WakeReason: reason})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := sf.s.RecordTurn(ctx, Turn{AgentID: sf.f.agent.ID, SessionID: "ses_test", Generation: 0, RoomID: sf.f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sf.s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.ID); err != nil {
		t.Fatal(err)
	}
	return turn
}

func (sf silentFixture) answer(t *testing.T, turn Turn, raw string) {
	t.Helper()
	receipt, err := applyCandidate(t, sf.s, turn, sf.f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

// notices counts the room's silent-turn lines and the boss's silent_turn notes.
func (sf silentFixture) notices(t *testing.T) (lines, wakes int) {
	t.Helper()
	if err := sf.s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body=?`, sf.f.room.ID, "@"+sf.f.agent.ID+" ended its turn without a message").Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if err := sf.s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND wake_reason='silent_turn'`, sf.boss.ID).Scan(&wakes); err != nil {
		t.Fatal(err)
	}
	return lines, wakes
}

const emptyAnswer = `{"messages":[]}`

// A worker addressed by another agent that ends its turn with no message gets one system line in the room, and the agent that addressed it
// is woken (reason silent_turn) to check on it.
func TestSilentTurnNoticeWakesTheAuthor(t *testing.T) {
	sf := newSilentFixture(t)
	turn := sf.turnWith(t, "agent", sf.boss.ID, "mention")
	sf.answer(t, turn, emptyAnswer)
	if lines, wakes := sf.notices(t); lines != 1 || wakes != 1 {
		t.Fatalf("lines=%d wakes=%d, want 1 and 1", lines, wakes)
	}
	var events int
	if err := sf.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='silent_turn' AND agent_id=? AND turn_id=?`, sf.f.agent.ID, turn.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("silent_turn events=%d err=%v", events, err)
	}
	// The line itself wakes nobody else: the worker gets no note for it.
	var workerNotes int
	if err := sf.s.db.QueryRow(`SELECT COUNT(*) FROM note n JOIN message m ON m.id=n.message_id WHERE n.agent_id=? AND m.body LIKE '%ended its turn without a message'`, sf.f.agent.ID).Scan(&workerNotes); err != nil || workerNotes != 0 {
		t.Fatalf("worker notes for its own notice=%d err=%v", workerNotes, err)
	}
	// Why the author was woken reads as such in its turn.
	if why := noteWhy("silent_turn", nil); !strings.Contains(why, "ended its turn without a message") {
		t.Fatalf("why=%q", why)
	}
}

// A person's note gives the line (the person reads it) but there is no agent to wake.
func TestSilentTurnNoticeForAPersonPostsTheLineOnly(t *testing.T) {
	sf := newSilentFixture(t)
	sf.answer(t, sf.turnWith(t, "user", "usr_answers", "mention"), emptyAnswer)
	if lines, wakes := sf.notices(t); lines != 1 || wakes != 0 {
		t.Fatalf("lines=%d wakes=%d, want 1 and 0", lines, wakes)
	}
}

// No notice for a broadcast (fyi), a group tag, a system note (a scheduled ping, a command's end), a lead, a turn that posted a message, or a
// second silent turn before the agent posts again.
func TestSilentTurnNoticeNotGiven(t *testing.T) {
	cases := map[string]func(t *testing.T, sf silentFixture) Turn{
		"fyi broadcast": func(t *testing.T, sf silentFixture) Turn { return sf.turnWith(t, "agent", sf.boss.ID, "broadcast") },
		"group tag":     func(t *testing.T, sf silentFixture) Turn { return sf.turnWith(t, "agent", sf.boss.ID, "group") },
		"scheduled ping": func(t *testing.T, sf silentFixture) Turn {
			return sf.turnWith(t, "system", "system", "inbox_answer")
		},
		"command finished": func(t *testing.T, sf silentFixture) Turn {
			return sf.turnWith(t, "system", "system", "command_finished")
		},
		"lead": func(t *testing.T, sf silentFixture) Turn {
			if _, err := sf.s.db.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key,cap_is_lead) VALUES ('role_lead_t','leader','Leader','body','leader',1)`); err != nil {
				t.Fatal(err)
			}
			if _, err := sf.s.db.Exec(`UPDATE agent SET role_id='role_lead_t' WHERE id=?`, sf.f.agent.ID); err != nil {
				t.Fatal(err)
			}
			return sf.turnWith(t, "agent", sf.boss.ID, "mention")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			sf := newSilentFixture(t)
			sf.answer(t, setup(t, sf), emptyAnswer)
			if lines, wakes := sf.notices(t); lines != 0 || wakes != 0 {
				t.Fatalf("lines=%d wakes=%d, want none", lines, wakes)
			}
		})
	}
	t.Run("posted a message", func(t *testing.T) {
		sf := newSilentFixture(t)
		sf.answer(t, sf.turnWith(t, "agent", sf.boss.ID, "mention"), fmt.Sprintf(`{"messages":[{"room":%q,"body":"done, @boss"}]}`, sf.f.room.ID))
		if lines, wakes := sf.notices(t); lines != 0 || wakes != 0 {
			t.Fatalf("lines=%d wakes=%d, want none", lines, wakes)
		}
	})
}

// The notice is said once until the agent next posts: a second silent turn gives none, and after the agent posts a message the next silent
// turn is announced again.
func TestSilentTurnNoticeDebouncedUntilTheAgentPosts(t *testing.T) {
	sf := newSilentFixture(t)
	sf.answer(t, sf.turnWith(t, "agent", sf.boss.ID, "mention"), emptyAnswer)
	sf.answer(t, sf.turnWith(t, "agent", sf.boss.ID, "reply"), emptyAnswer)
	if lines, wakes := sf.notices(t); lines != 1 || wakes != 1 {
		t.Fatalf("after a second silent turn: lines=%d wakes=%d, want 1 and 1", lines, wakes)
	}
	sf.answer(t, sf.turnWith(t, "agent", sf.boss.ID, "mention"), fmt.Sprintf(`{"messages":[{"room":%q,"body":"on it, @boss"}]}`, sf.f.room.ID))
	sf.answer(t, sf.turnWith(t, "agent", sf.boss.ID, "handoff"), emptyAnswer)
	if lines, wakes := sf.notices(t); lines != 2 || wakes != 2 {
		t.Fatalf("after posting and going silent again: lines=%d wakes=%d, want 2 and 2", lines, wakes)
	}
}

// A reaction is the acknowledgement the prompt asks for: a turn that reacted during the turn is not silent, one with neither a reaction nor a
// message is. A reaction from before the turn does not count.
func TestSilentTurnNoticeSkipsATurnThatReacted(t *testing.T) {
	sf := newSilentFixture(t)
	ctx := context.Background()
	react := func(turn Turn) {
		t.Helper()
		var messageID string
		if err := sf.s.db.QueryRow(`SELECT n.message_id FROM turn_note tn JOIN note n ON n.id=tn.note_id WHERE tn.turn_id=?`, turn.ID).Scan(&messageID); err != nil {
			t.Fatal(err)
		}
		if err := sf.s.Tx(ctx, func(tx *Tx) error { return tx.SetReaction(messageID, "agent", sf.f.agent.ID, "\U0001F44D", true) }); err != nil {
			t.Fatal(err)
		}
	}
	reacted := sf.turnWith(t, "agent", sf.boss.ID, "mention")
	react(reacted)
	sf.answer(t, reacted, emptyAnswer)
	if lines, wakes := sf.notices(t); lines != 0 || wakes != 0 {
		t.Fatalf("a turn that reacted was announced: lines=%d wakes=%d", lines, wakes)
	}
	sf.answer(t, sf.turnWith(t, "agent", sf.boss.ID, "mention"), emptyAnswer)
	if lines, wakes := sf.notices(t); lines != 1 || wakes != 1 {
		t.Fatalf("a turn with neither a reaction nor a message: lines=%d wakes=%d, want 1 and 1", lines, wakes)
	}
}
