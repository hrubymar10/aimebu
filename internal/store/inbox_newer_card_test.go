package store

import (
	"context"
	"testing"
)

// supersededReason is the reason the inbox_item_resolved event gave for an item.
func supersededReason(t *testing.T, s *Store, itemID string) string {
	t.Helper()
	var reason string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.reason') FROM event WHERE kind='inbox_item_resolved' AND json_extract(payload_json,'$.inbox_item_id')=?`, itemID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	return reason
}

// peerAgent adds a second agent to the fixture's room and returns its ID.
func peerAgent(t *testing.T, s *Store, f executionFixture) string {
	t.Helper()
	ctx := context.Background()
	other, err := s.CreateAgent(ctx, Agent{Slug: "peer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_answers"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", other.ID); err != nil {
		t.Fatal(err)
	}
	return other.ID
}

// structureCard gives an attention card's message proposed answers, the structured kind of card that waits for its answer.
func structureCard(t *testing.T, s *Store, messageID string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE message SET proposed_answers_json='["a","b"]' WHERE id=?`, messageID); err != nil {
		t.Fatal(err)
	}
}

// An agent's newer card (an attention message or an ask) in the same room settles its older open cards, structured answers or not. Another
// agent's card, the same agent's card in another room, and the same agent's plain message leave them open.
func TestNewerCardFromTheSameAgentSupersedesItsOlderCard(t *testing.T) {
	for _, tc := range []struct {
		name       string
		structured bool
		message    func(f executionFixture, other, otherRoom string) Message
		settled    bool
	}{
		{"newer attention card", false, func(f executionFixture, _, _ string) Message {
			return Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: f.agent.ID, Body: "Now this?", NeedsAttention: true}
		}, true},
		{"newer card replaces a structured one", true, func(f executionFixture, _, _ string) Message {
			return Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: f.agent.ID, Body: "Now this?", OpenQuestionsJSON: `[{"question":"Which?","options":["x","y"]}]`}
		}, true},
		{"newer ask", true, func(f executionFixture, _, _ string) Message {
			return Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: f.agent.ID, Body: "Choose", AskJSON: `{"kind":"question","text":"Which?","options":["A"]}`}
		}, true},
		{"plain message from the same agent", false, func(f executionFixture, _, _ string) Message {
			return Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: f.agent.ID, Body: "status"}
		}, false},
		{"another agent's card", false, func(f executionFixture, other, _ string) Message {
			return Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: other, Body: "Mine?", NeedsAttention: true}
		}, false},
		{"the same agent's card in another room", false, func(f executionFixture, _, otherRoom string) Message {
			return Message{RoomID: otherRoom, AuthorKind: "agent", AuthorID: f.agent.ID, Body: "Elsewhere?", NeedsAttention: true}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			ctx := context.Background()
			item := attentionQuestion(t, s, f)
			if tc.structured {
				structureCard(t, s, item.MessageID)
			}
			other := peerAgent(t, s, f)
			room, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "Other", CreatedBy: "usr_answers"})
			if err != nil {
				t.Fatal(err)
			}
			m := tc.message(f, other, room.ID)
			if _, err := s.InsertMessage(ctx, m); err != nil {
				t.Fatal(err)
			}
			state, _, by := inboxState(t, s, item.ID)
			if !tc.settled {
				if state != "open" {
					t.Fatalf("state=%q, want open", state)
				}
				return
			}
			if state != "superseded" || by != f.agent.ID {
				t.Fatalf("state=%q resolved_by=%q, want superseded by the agent", state, by)
			}
			if reason := supersededReason(t, s, item.ID); reason != "newer card" {
				t.Fatalf("reason=%q", reason)
			}
		})
	}
}

// A card with structured answers waits for its answer: a person's plain message in the room (the room rule, which settles a lone plain
// attention card) or a reply in another thread leaves it open; a reply in its own thread settles it.
func TestStructuredCardIsNotSettledByAnUnrelatedReply(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	item := attentionQuestion(t, s, f)
	structureCard(t, s, item.MessageID)
	other, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "usr_answers", Body: "something else"})
	if err != nil {
		t.Fatal(err)
	}
	if state, _, _ := inboxState(t, s, item.ID); state != "open" {
		t.Fatalf("a plain message settled a structured card: state=%q", state)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "usr_answers", Body: "re other", ReplyTo: other.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := inboxState(t, s, item.ID); state != "open" {
		t.Fatalf("a reply in another thread settled a structured card: state=%q", state)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "usr_answers", Body: "a", ReplyTo: item.MessageID}); err != nil {
		t.Fatal(err)
	}
	if state, _, by := inboxState(t, s, item.ID); state != "superseded" || by != "usr_answers" {
		t.Fatalf("a reply in its thread: state=%q by=%q", state, by)
	}
	if reason := supersededReason(t, s, item.ID); reason != "answered in thread" {
		t.Fatalf("reason=%q", reason)
	}
}

// A person's reply anywhere in the card's thread settles it, also when it replies to a later message of that thread rather than the card.
func TestReplyAnywhereInTheCardsThreadSettlesIt(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	item := attentionQuestion(t, s, f)
	structureCard(t, s, item.MessageID)
	follow, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: f.agent.ID, Body: "more detail", ReplyTo: item.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	// The asking agent's own reply settles it already (it answers its card); reopen it to test the person's reply deeper in the thread.
	if _, err := s.db.Exec(`UPDATE inbox_item SET state='open' WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "usr_answers", Body: "ok", ReplyTo: follow.ID}); err != nil {
		t.Fatal(err)
	}
	if state, _, by := inboxState(t, s, item.ID); state != "superseded" || by != "usr_answers" {
		t.Fatalf("state=%q by=%q", state, by)
	}
}

// Cards opened before the newer-card rule: two open cards of one agent in one room. The sweep settles the older one and keeps the newest.
func TestSweepSettlesAnAgentsOlderCards(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	first := attentionQuestion(t, s, f)
	// The newer card goes in as rows, as it was before supersedeAsks knew newer cards.
	if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,needs_attention,created_at)
		VALUES ('msg_newer',(SELECT MAX(seq)+1 FROM message),?,'agent',?,'And now?','[]',1,?)`, f.room.ID, f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		SELECT 'inb_newer',project_id,room_id,'msg_newer',turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at FROM inbox_item WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, by := inboxState(t, s, first.ID); state != "superseded" || by != "system (newer card)" {
		t.Fatalf("older card state=%q by=%q", state, by)
	}
	if state, _, _ := inboxState(t, s, "inb_newer"); state != "open" {
		t.Fatalf("newest card state=%q", state)
	}
}
