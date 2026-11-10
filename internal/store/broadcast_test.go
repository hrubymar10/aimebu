package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// broadcastFixture is the wake fixture with a third agent in the room.
func broadcastFixture(t *testing.T) (s *Store, room Room, one, lead, three Agent) {
	t.Helper()
	s, project, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	three, err := s.CreateAgent(ctx, Agent{Slug: "three", ProjectID: project.ID, RoleID: "role_worker", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_wake", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "agent", three.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_three', ?,0,'fresh_with_history',?)`, three.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	return s, room, one, lead, three
}

// wakeReasons is who a message woke, by slug, and why.
func wakeReasons(t *testing.T, s *Store, messageID string) map[string]string {
	t.Helper()
	rows, err := s.db.Query(`SELECT a.slug,n.wake_reason FROM note n JOIN agent a ON a.id=n.agent_id WHERE n.message_id=?`, messageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var slug, reason string
		if err := rows.Scan(&slug, &reason); err != nil {
			t.Fatal(err)
		}
		out[slug] = reason
	}
	return out
}

// An agent's message with no mention and no reply is a broadcast: it wakes every other agent in the room (reason "broadcast") and never its sender.
func TestBroadcastWakesEveryAgentButTheSender(t *testing.T) {
	t.Parallel()
	s, room, one, _, _ := broadcastFixture(t)
	message, err := s.InsertMessage(context.Background(), Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "status: the build is green"})
	if err != nil {
		t.Fatal(err)
	}
	if got := wakeReasons(t, s, message.ID); !sameWakes(got, map[string]string{"lead": "broadcast", "three": "broadcast"}) {
		t.Fatalf("a broadcast woke %v, want lead and three (reason broadcast) and not the sender", got)
	}
}

// Anything that addresses somebody is not a broadcast: a mention (even one that reaches nobody, or only a person), a reply (it wakes only its
// parent's author), a handoff (the lead), and a private chat. A mention written inside code is not a mention, so it is one.
func TestBroadcastIsOnlyForAMessageThatAddressesNobody(t *testing.T) {
	t.Parallel()
	s, room, one, lead, three := broadcastFixture(t)
	ctx := context.Background()
	post := func(author Agent, body, replyTo string, handoff bool) Message {
		m, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: author.ID, Body: body, ReplyTo: replyTo, Handoff: handoff})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for name, test := range map[string]struct {
		message Message
		want    map[string]string
	}{
		"a mention of one agent":             {post(one, "@three please look", "", false), map[string]string{"three": "mention"}},
		"a mention of nobody":                {post(one, "@ghost please look", "", false), map[string]string{}},
		"a mention of a person":              {post(one, "@owner please look", "", false), map[string]string{}},
		"a mention in code is not a mention": {post(one, "the `@three` tag", "", false), map[string]string{"lead": "broadcast", "three": "broadcast"}},
		"a handoff":                          {post(one, "finished", "", true), map[string]string{"lead": "handoff"}},
	} {
		if got := wakeReasons(t, s, test.message.ID); !sameWakes(got, test.want) {
			t.Errorf("%s woke %v, want %v", name, got, test.want)
		}
	}
	// A reply without a mention wakes only the author of its parent (the hop guard still applies to it), never the rest of the room.
	parent := post(three, "@lead ready for review", "", false)
	reply := post(lead, "looking now", parent.ID, false)
	if got := wakeReasons(t, s, reply.ID); !sameWakes(got, map[string]string{"three": "reply"}) {
		t.Fatalf("a reply without a mention woke %v, want only the parent's author (reply)", got)
	}
}

// A message that asks a person for something (an Inbox ask, needs_attention, proposed answers, open questions) is addressed to that person, who
// sees it in the Inbox: it is not a broadcast, so the other agents are not put through an FYI turn for it. The same message without the request is.
func TestBroadcastLeavesAMessageThatAsksAPersonAlone(t *testing.T) {
	t.Parallel()
	s, room, one, _, _ := broadcastFixture(t)
	ctx := context.Background()
	for name, test := range map[string]struct {
		message Message
		want    map[string]string
	}{
		"plain":            {Message{Body: "the build is green"}, map[string]string{"lead": "broadcast", "three": "broadcast"}},
		"empty fields":     {Message{Body: "the build is green", ProposedAnswersJSON: "[]", OpenQuestionsJSON: "null"}, map[string]string{"lead": "broadcast", "three": "broadcast"}},
		"needs attention":  {Message{Body: "may I merge?", NeedsAttention: true}, map[string]string{}},
		"an Inbox ask":     {Message{Body: "may I merge?", AskJSON: `{"id":"ask_1"}`}, map[string]string{}},
		"proposed answers": {Message{Body: "which one?", ProposedAnswersJSON: `["yes","no"]`}, map[string]string{}},
		"open questions":   {Message{Body: "a few things", OpenQuestionsJSON: `[{"question":"Scope?","options":["a","b"]}]`}, map[string]string{}},
	} {
		message := test.message
		message.RoomID, message.AuthorKind, message.AuthorID = room.ID, "agent", one.ID
		stored, err := s.InsertMessage(ctx, message)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := wakeReasons(t, s, stored.ID); !sameWakes(got, test.want) {
			t.Errorf("%s woke %v, want %v", name, got, test.want)
		}
	}
}

// A person's message is unchanged: unaddressed it wakes every agent as a person's message to the room, and a mention narrows it.
func TestBroadcastLeavesAPersonsMessageAlone(t *testing.T) {
	t.Parallel()
	s, room, _, _, _ := broadcastFixture(t)
	ctx := context.Background()
	all, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "please work"})
	if err != nil {
		t.Fatal(err)
	}
	if got := wakeReasons(t, s, all.ID); !sameWakes(got, map[string]string{"one": "human_message", "lead": "human_message", "three": "human_message"}) {
		t.Fatalf("a person's unaddressed message woke %v", got)
	}
	one, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one please work"})
	if err != nil {
		t.Fatal(err)
	}
	if got := wakeReasons(t, s, one.ID); !sameWakes(got, map[string]string{"one": "mention"}) {
		t.Fatalf("a person's mention woke %v", got)
	}
}

// A lead that is in the room only because leads are put into every project room is not woken by a broadcast, as it is not by a person's
// unaddressed message; it is still addressable, and the others are woken.
func TestBroadcastDoesNotWakeAnAutoLead(t *testing.T) {
	t.Parallel()
	s, room, one, lead, _ := broadcastFixture(t)
	if _, err := s.db.Exec(`UPDATE room_member SET auto=1 WHERE room_id=? AND member_kind='agent' AND member_id=?`, room.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	message, err := s.InsertMessage(context.Background(), Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if got := wakeReasons(t, s, message.ID); !sameWakes(got, map[string]string{"three": "broadcast"}) {
		t.Fatalf("a broadcast woke %v, want only three", got)
	}
	var skipped int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='message_not_woken' AND agent_id=? AND payload_json LIKE '%auto_member%'`, lead.ID).Scan(&skipped); err != nil || skipped != 1 {
		t.Fatalf("the skip is recorded: %d, %v", skipped, err)
	}
}

// The agent is told a broadcast was not addressed to it: the delivered note is marked fyi and says so in its why, while a note that was for
// the agent is not marked.
func TestBroadcastNoteIsMarkedFYIWhenDelivered(t *testing.T) {
	t.Parallel()
	s, room, one, lead, three := broadcastFixture(t)
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "status: the build is green"}); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, lead.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 1 {
		t.Fatalf("delivery ready=%t notes=%d err=%v", ready, len(delivery.Notes), err)
	}
	note := delivery.Notes[0]
	if !note.FYI || !strings.Contains(note.Why, "not addressed to you") {
		t.Fatalf("a broadcast note must say it was not addressed to the agent: fyi=%t why=%q", note.FYI, note.Why)
	}
	// What is for the agent is not an FYI: a mention.
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "@three your turn"}); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err = s.PrepareDispatch(ctx, three.ID, "")
	if err != nil || !ready {
		t.Fatalf("delivery ready=%t err=%v", ready, err)
	}
	var fyi, addressed int
	for _, n := range delivery.Notes {
		if n.FYI {
			fyi++
		} else {
			addressed++
		}
	}
	if fyi != 1 || addressed != 1 {
		t.Fatalf("three has one broadcast and one mention: fyi=%d addressed=%d notes=%+v", fyi, addressed, delivery.Notes)
	}
}

// Broadcasts count against the hop guard with replies: two agents that broadcast to each other wake one another only ReplyWakeHops times in a row, the
// next broadcast is stored and wakes nobody (said once), and a person's message starts the count again.
func TestBroadcastsAreBoundedByTheHopGuard(t *testing.T) {
	t.Parallel()
	s, _, room, worker, lead := wakeFixture(t)
	ctx := context.Background()
	say := func(kind, author, body, replyTo string) Message {
		t.Helper()
		message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: kind, AuthorID: author, Body: body, ReplyTo: replyTo})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	woken := func(agent string, message Message) bool {
		_, err := s.NoteForAgentMessage(ctx, agent, message.ID)
		return err == nil
	}
	count := func(query string) int {
		var n int
		if err := s.db.QueryRow(query, room.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	guards := func() int { return count(`SELECT COUNT(*) FROM event WHERE kind='loop_guard' AND room_id=?`) }
	notices := func() int {
		return count(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body LIKE 'Agents stopped answering each other%'`)
	}
	pair := []Agent{worker, lead}
	// A person starts it, then the two broadcast to each other, each woken by the other's last message. (A turn woken by a person would be answering
	// that person, so this is the agents' own exchange.)
	say("user", "owner", "kick off", "")
	for hop := 0; hop < ReplyWakeHops; hop++ {
		author, other := pair[hop%2], pair[(hop+1)%2]
		message := say("agent", author.ID, fmt.Sprintf("status %d", hop), "")
		if !woken(other.ID, message) {
			t.Fatalf("broadcast %d should wake the other agent", hop)
		}
	}
	if guards() != 0 {
		t.Fatal("no guard before the limit")
	}
	author, other := pair[ReplyWakeHops%2], pair[(ReplyWakeHops+1)%2]
	blocked := say("agent", author.ID, "one too many", "")
	if woken(other.ID, blocked) {
		t.Fatal("a broadcast past the limit must not wake anybody")
	}
	if guards() != 1 || notices() != 1 {
		t.Fatalf("guard events=%d notices=%d, want one each", guards(), notices())
	}
	var kind string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.kind') FROM event WHERE kind='loop_guard' AND room_id=?`, room.ID).Scan(&kind); err != nil || kind != "broadcast" {
		t.Fatalf("the guard event says what it stopped: %q %v", kind, err)
	}
	if again := say("agent", other.ID, "still going", ""); woken(author.ID, again) || guards() != 1 || notices() != 1 {
		t.Fatalf("past the limit it stays quiet: guards=%d notices=%d", guards(), notices())
	}
	// A mention is a different thing and always works.
	if mention := say("agent", other.ID, "@"+author.Slug+" are you there", ""); !woken(author.ID, mention) {
		t.Fatal("a mention still wakes")
	}
	// A person's message starts the count again.
	say("user", "owner", "carry on", "")
	if message := say("agent", author.ID, "back again", ""); !woken(other.ID, message) {
		t.Fatal("after a person's message the agents may broadcast to each other again")
	}
}

// Replies and broadcasts share the limit: the count is of automatic wakes, whichever way they came.
func TestBroadcastsAndRepliesCountTogether(t *testing.T) {
	t.Parallel()
	s, _, room, worker, lead := wakeFixture(t)
	ctx := context.Background()
	say := func(author Agent, body, replyTo string) Message {
		t.Helper()
		message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: author.ID, Body: body, ReplyTo: replyTo})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	woken := func(agent Agent, message Message) bool {
		_, err := s.NoteForAgentMessage(ctx, agent.ID, message.ID)
		return err == nil
	}
	last := say(worker, "start", "") // a broadcast: wakes the lead
	if !woken(lead, last) {
		t.Fatal("the first broadcast wakes")
	}
	pair := []Agent{lead, worker}
	for hop := 1; hop < ReplyWakeHops; hop++ { // replies and broadcasts alternate, each one a wake, up to the limit
		author, other := pair[(hop-1)%2], pair[hop%2]
		var message Message
		if hop%2 == 1 {
			message = say(author, fmt.Sprintf("reply %d", hop), last.ID)
		} else {
			message = say(author, fmt.Sprintf("status %d", hop), "")
		}
		if !woken(other, message) {
			t.Fatalf("wake %d should wake (replies and broadcasts share %d)", hop, ReplyWakeHops)
		}
		last = message
	}
	author, other := pair[(ReplyWakeHops-1)%2], pair[ReplyWakeHops%2]
	if message := say(author, "past the limit", ""); woken(other, message) {
		t.Fatal("the wake past the shared limit must not happen")
	}
}
