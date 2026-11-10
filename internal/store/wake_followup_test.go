package store

import (
	"context"
	"testing"
)

type replyHarness struct {
	t            *testing.T
	s            *Store
	room         Room
	worker, lead Agent
}

func newReplyHarness(t *testing.T) *replyHarness {
	t.Helper()
	s, _, room, worker, lead := wakeFixture(t)
	return &replyHarness{t: t, s: s, room: room, worker: worker, lead: lead}
}

func (h *replyHarness) say(kind, author, body, replyTo string) Message {
	h.t.Helper()
	message, err := h.s.InsertMessage(context.Background(), Message{RoomID: h.room.ID, AuthorKind: kind, AuthorID: author, Body: body, ReplyTo: replyTo})
	if err != nil {
		h.t.Fatal(err)
	}
	return message
}

func (h *replyHarness) reason(agent string, message Message) string {
	note, err := h.s.NoteForAgentMessage(context.Background(), agent, message.ID)
	if err != nil {
		return ""
	}
	return note.WakeReason
}

func (h *replyHarness) guards() int {
	var count int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='loop_guard' AND room_id=?`, h.room.ID).Scan(&count); err != nil {
		h.t.Fatal(err)
	}
	return count
}

// exchange has the two agents answer each other until the room is at the limit; it returns the first message and the last.
func (h *replyHarness) exchange() (Message, Message) {
	root := h.say("agent", h.worker.ID, "@lead root", "") // a mention: a broadcast would count against the limit
	previous := root
	agents := []string{h.lead.ID, h.worker.ID}
	for hop := 0; hop < ReplyWakeHops; hop++ {
		next := h.say("agent", agents[hop%2], "reply", previous.ID)
		if h.reason(agents[(hop+1)%2], next) != "reply" {
			h.t.Fatalf("reply %d should wake", hop+1)
		}
		previous = next
	}
	return root, previous
}

// F007: the count belongs to the room, not to one chain, so answering an older message does not start it again.
func TestReplyToAnOlderMessageDoesNotResetTheCount(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t)
	root, _ := h.exchange()
	blocked := h.say("agent", h.lead.ID, "back to the start", root.ID)
	if h.reason(h.worker.ID, blocked) != "" || h.guards() != 1 {
		t.Fatalf("a reply to an older message must hit the same limit: reason=%q guards=%d", h.reason(h.worker.ID, blocked), h.guards())
	}
	// A person's message anywhere in the room starts it again.
	h.say("user", "owner", "carry on", "")
	again := h.say("agent", h.lead.ID, "thanks", root.ID)
	if h.reason(h.worker.ID, again) != "reply" {
		t.Fatal("after a person's message the agents may answer each other again")
	}
}

// F004/F006: the notice is for a wake that was really blocked, not for a self-reply or a parent that is woken by a mention anyway.
func TestLoopGuardOnlyAnnouncesARealBlock(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t)
	_, last := h.exchange()
	mine := h.say("agent", h.worker.ID, "a note to self for @owner", "") // names a person: a broadcast here would be a blocked wake of its own
	h.say("agent", h.worker.ID, "following up on myself", mine.ID)
	if h.guards() != 0 {
		t.Fatalf("a self-reply is not a blocked wake: guards=%d", h.guards())
	}
	// last was written by the worker (hop 4); the lead replies and mentions the worker, so the worker is woken anyway.
	mentioned := h.say("agent", h.lead.ID, "@one please look", last.ID)
	if h.reason(h.worker.ID, mentioned) != "mention" || h.guards() != 0 {
		t.Fatalf("mentioned parent: reason=%q guards=%d", h.reason(h.worker.ID, mentioned), h.guards())
	}
	blocked := h.say("agent", h.lead.ID, "plain reply", last.ID)
	if h.reason(h.worker.ID, blocked) != "" || h.guards() != 1 {
		t.Fatalf("plain reply past the limit: reason=%q guards=%d", h.reason(h.worker.ID, blocked), h.guards())
	}
}

// F005: a reply never replaces the stronger reason that already targets the parent's author.
func TestReplyDoesNotOverwriteAMention(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t)
	asked := h.say("agent", h.worker.ID, "status?", "")
	answer := h.say("agent", h.lead.ID, "@one it is fine", asked.ID)
	if got := h.reason(h.worker.ID, answer); got != "mention" {
		t.Fatalf("wake reason = %q, want mention", got)
	}
}

// F003: woken_by follows the note that started the turn, not any note that joined it later.
func TestWokenByFollowsTheNoteThatStartedTheTurn(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t)
	ctx := context.Background()
	if _, err := h.s.db.Exec(`UPDATE agent SET observed='in_turn' WHERE id=?`, h.worker.ID); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := h.s.db.QueryRow(`SELECT id FROM session WHERE agent_id=? LIMIT 1`, h.worker.ID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	asked := h.say("agent", h.worker.ID, "status?", "")
	mention := h.say("agent", h.lead.ID, "@one first this", "")
	reply := h.say("agent", h.lead.ID, "and a reply", asked.ID)
	turn, err := h.s.RecordTurn(ctx, Turn{AgentID: h.worker.ID, SessionID: sessionID, Generation: 0, RoomID: h.room.ID, Attempt: 1, Mode: "normal", Exec: "running"})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{mention, reply} {
		note, err := h.s.NoteForAgentMessage(ctx, h.worker.ID, message.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.ID); err != nil {
			t.Fatal(err)
		}
	}
	card, err := h.s.AgentCardByID(ctx, h.worker.ID)
	if err != nil || card.WokenBy != "" {
		t.Fatalf("a turn begun by a mention is not woken by a reply: %q %v", card.WokenBy, err)
	}
	if _, err := h.s.db.Exec(`UPDATE note SET created_at='2000-01-01T00:00:00.000Z' WHERE message_id=?`, reply.ID); err != nil {
		t.Fatal(err)
	}
	card, err = h.s.AgentCardByID(ctx, h.worker.ID)
	if err != nil || card.WokenBy != "reply" {
		t.Fatalf("a turn begun by a reply: %q %v", card.WokenBy, err)
	}
}
