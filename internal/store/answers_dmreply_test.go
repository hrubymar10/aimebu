package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// dmReplyFixture is the answer fixture (a worker with a turn in a task room) plus two peers in its project: one shares a private chat
// with the worker, the two peers share another one the worker is not in.
type dmReplyFixture struct {
	s          *Store
	f          executionFixture
	own, other Room
	peer1      Agent
	peer2      Agent
}

func newDMReplyFixture(t *testing.T) dmReplyFixture {
	t.Helper()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	mk := func(slug string) Agent {
		agent, err := s.CreateAgent(ctx, Agent{Slug: slug, ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash-" + slug, CreatedBy: "usr_test"})
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}
	d := dmReplyFixture{s: s, f: f, peer1: mk("peerone"), peer2: mk("peertwo")}
	open := func(a, b string) Room {
		var room Room
		if err := s.Tx(ctx, func(tx *Tx) (err error) { room, _, err = tx.OpenAgentDM(a, b, a); return }); err != nil {
			t.Fatal(err)
		}
		return room
	}
	d.own = open(f.agent.ID, d.peer1.ID)
	d.other = open(d.peer1.ID, d.peer2.ID)
	return d
}

func (d dmReplyFixture) noteTo(t *testing.T, roomID string) string {
	t.Helper()
	var body string
	d.s.db.QueryRow(`SELECT COALESCE((SELECT m.body FROM message m JOIN note n ON n.message_id=m.id WHERE m.room_id=? AND m.author_kind='system' AND n.agent_id=? AND m.body LIKE 'Your previous reply was held:%' ORDER BY m.no DESC LIMIT 1),'')`, roomID, d.f.agent.ID).Scan(&body)
	return body
}

func (d dmReplyFixture) cards(t *testing.T) int {
	t.Helper()
	var n int
	if err := d.s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE kind='delivery_failed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An agent may answer into a private chat it is in from a turn woken in a task room; a chat it is not in stays refused.
func TestFinalAnswerReachesOwnPrivateChatFromTaskTurn(t *testing.T) {
	t.Parallel()
	d := newDMReplyFixture(t)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"ack to my peer"}]}`, d.own.ID)
	receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.MessageIDs) != 1 || len(receipt.HeldMessages) != 0 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	got, err := d.s.ListMessagesAfter(context.Background(), d.own.ID, 0)
	if err != nil || len(got) == 0 || got[len(got)-1].Body != "ack to my peer" {
		t.Fatalf("private chat messages=%+v err=%v", got, err)
	}
}

func TestFinalAnswerIntoForeignPrivateChatIsRefused(t *testing.T) {
	t.Parallel()
	d := newDMReplyFixture(t)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"into a chat I am not in"}]}`, d.other.ID)
	receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || receipt.HeldReason != "not_member" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if got, _ := d.s.ListMessagesAfter(context.Background(), d.other.ID, 0); len(got) != 0 {
		t.Fatalf("message reached a chat the agent is not in: %+v", got)
	}
}

func TestFinalAnswerCannotReachCrossProjectChatFromTaskTurn(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	chat := f.open(t, f.lead1, f.lead2)
	turn := f.leadTurn(t, f.lead1, f.alice)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"cross-project final answer"}]}`, chat.ID)
	receipt, err := applyCandidate(t, f.s, turn, f.lead1.ID, 0, raw)
	if err != nil || receipt.Result != "held" || receipt.HeldReason != "not_member" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	var note string
	if err := f.s.db.QueryRow(`SELECT m.body FROM message m JOIN note n ON n.message_id=m.id WHERE m.room_id=? AND n.agent_id=? AND m.body LIKE 'Your previous reply was held:%' ORDER BY m.no DESC LIMIT 1`, f.task1.ID, f.lead1.ID).Scan(&note); err != nil {
		t.Fatalf("failure note: %v", err)
	}
	if !strings.Contains(note, "dm_agent") || !strings.Contains(note, "cross-project final answer") {
		t.Fatalf("failure note=%q, want dm_agent hint and original text", note)
	}
}

func TestAskIntoAgentChatChecksThatChatsUserMembers(t *testing.T) {
	t.Parallel()
	d := newDMReplyFixture(t)
	if err := d.s.AddMember(context.Background(), d.f.room.ID, "user", "usr_answers"); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"question","ask":{"kind":"question","text":"Answer?"}}]}`, d.own.ID)
	receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || receipt.Error != "ask has no authorized recipient" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestHeldAgentReplyAfterLeavingTurnRoomRaisesFailureCard(t *testing.T) {
	t.Parallel()
	d := newDMReplyFixture(t)
	if _, err := d.s.db.Exec(`UPDATE room_member SET left_at=? WHERE room_id=? AND member_kind='agent' AND member_id=?`, timestamp(), d.f.room.ID, d.f.agent.ID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"reply to peer"}]}`, d.other.ID)
	receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "held" || d.cards(t) != 1 {
		t.Fatalf("receipt=%+v cards=%d err=%v", receipt, d.cards(t), err)
	}
}

func TestPartlyHeldReplyAfterLeavingTurnRoomRaisesFailureCard(t *testing.T) {
	t.Parallel()
	d := newDMReplyFixture(t)
	if _, err := d.s.db.Exec(`UPDATE room_member SET left_at=? WHERE room_id=? AND member_kind='agent' AND member_id=?`, timestamp(), d.f.room.ID, d.f.agent.ID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"delivered"},{"room":%q,"body":"held"}]}`, d.own.ID, d.other.ID)
	receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" || len(receipt.HeldMessages) != 1 || d.cards(t) != 1 {
		t.Fatalf("receipt=%+v cards=%d err=%v", receipt, d.cards(t), err)
	}
}

// A message to another agent that cannot be delivered raises no Inbox item: the sender gets a note, in the room it was woken in, with the
// reason and the full text.
func TestUndeliverableMessageToAgentNotesTheSenderAndRaisesNoCard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	long := strings.Repeat("full text of the message ", 8)
	for name, tc := range map[string]func(d dmReplyFixture, t *testing.T) (target string, body string){
		"private chat with agents the sender is not in": func(d dmReplyFixture, t *testing.T) (string, string) { return d.other.ID, long },
		"closed room, mention of an agent": func(d dmReplyFixture, t *testing.T) (string, string) {
			room, err := d.s.CreateRoom(ctx, Room{ProjectID: d.f.project.ID, Kind: "task", Title: "closing", CreatedBy: "usr_answers"})
			if err != nil {
				t.Fatal(err)
			}
			if err := d.s.AddMember(ctx, room.ID, "agent", d.f.agent.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.s.AddMember(ctx, room.ID, "agent", d.peer1.ID); err != nil {
				t.Fatal(err)
			}
			d.s.db.Exec(`UPDATE room SET lifecycle='closing' WHERE id=?`, room.ID)
			return room.ID, "@" + d.peer1.ID + " " + long
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDMReplyFixture(t)
			target, body := tc(d, t)
			raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":%q}]}`, target, body)
			receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, raw)
			if err != nil || receipt.Result != "held" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			if n := d.cards(t); n != 0 {
				t.Fatalf("an undeliverable agent-to-agent message raised %d delivery_failed items", n)
			}
			note := d.noteTo(t, d.f.room.ID)
			if note == "" || !strings.Contains(note, body) || !strings.Contains(note, "dm_agent") || strings.Contains(note, "Send it to a room of your project") {
				t.Fatalf("note=%q, want the reason, the full text and the dm_agent hint", note)
			}
			if items, err := d.s.InboxForUser(ctx, "usr_answers"); err != nil || len(items) != 0 {
				t.Fatalf("inbox=%+v err=%v", items, err)
			}
		})
	}
}

// A message that cannot be delivered to a person, or to a person and an agent, still raises the card; so does one from an agent that
// was already woken by a held-reply note (no loop), and one from an agent that no longer exists.
func TestUndeliverableMessageToPeopleStillRaisesCard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	closedWith := func(d dmReplyFixture, t *testing.T) Room {
		room, err := d.s.CreateRoom(ctx, Room{ProjectID: d.f.project.ID, Kind: "task", Title: "closing", CreatedBy: "usr_answers"})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{d.f.agent.ID, d.peer1.ID} {
			if err := d.s.AddMember(ctx, room.ID, "agent", id); err != nil {
				t.Fatal(err)
			}
		}
		d.s.db.Exec(`UPDATE room SET lifecycle='closing' WHERE id=?`, room.ID)
		return room
	}
	for name, body := range map[string]func(d dmReplyFixture) string{
		"plain text":                     func(d dmReplyFixture) string { return "to whoever reads this" },
		"mention of an agent and a user": func(d dmReplyFixture) string { return "@" + d.peer1.ID + " and @owner please see" },
		"ask of a person":                func(d dmReplyFixture) string { return "@" + d.peer1.ID + " question" },
	} {
		t.Run(name, func(t *testing.T) {
			d := newDMReplyFixture(t)
			room := closedWith(d, t)
			extra := ""
			if name == "ask of a person" {
				extra = `,"needs_attention":true`
			}
			raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":%q%s}]}`, room.ID, body(d), extra)
			receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, raw)
			if err != nil || receipt.Result != "held" || receipt.HeldReason != "room_closed" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			if n := d.cards(t); n != 1 {
				t.Fatalf("delivery_failed items=%d, want 1", n)
			}
		})
	}
}

func TestUndeliverableAgentMessageOfRemovedAgentRaisesCard(t *testing.T) {
	t.Parallel()
	d := newDMReplyFixture(t)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"to a chat I am not in"}]}`, d.other.ID)
	d.s.db.Exec(`UPDATE agent SET revoked_at=? WHERE id=?`, timestamp(), d.f.agent.ID)
	if receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, raw); err != nil || receipt.Result != "held" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if n := d.cards(t); n != 1 {
		t.Fatalf("delivery_failed items=%d, want 1 for an agent that no longer exists", n)
	}
}

// Each reason is one sentence of its own; a room outside the project also names the missing membership.
func TestRefusedTargetReasonsAreSeparateSentences(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"other project": "This room is outside the agent's project. The agent is not a member of this room.",
		"foreign chat":  "The agent is not a member of this room.",
	} {
		t.Run(name, func(t *testing.T) {
			d := newDMReplyFixture(t)
			room := d.other.ID
			if name == "other project" {
				room = otherProjectRoom(t, d.s).ID
			}
			receipt, err := applyCandidate(t, d.s, d.f.turn, d.f.agent.ID, 0, fmt.Sprintf(`{"messages":[{"room":%q,"body":"x"}]}`, room))
			if err != nil || receipt.Result != "held" || receipt.Error != want {
				t.Fatalf("receipt=%+v err=%v, want error %q", receipt, err, want)
			}
		})
	}
}
