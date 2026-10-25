package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// notesFor lists, by agent slug, the wake reasons of the notes a message left.
func (f dmFixture) notesFor(t *testing.T, messageID string) map[string]string {
	t.Helper()
	rows, err := f.s.db.Query(`SELECT a.slug,n.wake_reason FROM note n JOIN agent a ON a.id=n.agent_id WHERE n.message_id=?`, messageID)
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

func sameWakes(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// What one agent says wakes the other agent of the pair and nobody else, whatever the text addresses.
func TestAgentDMAgentMessageWakesOnlyThePeer(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.alice)
	for name, tc := range map[string]struct {
		from Agent
		body string
		want map[string]string
	}{
		"plain":                 {f.lead1, "a question", map[string]string{"peer": "agent_dm"}},
		"answer":                {f.worker2, "the answer", map[string]string{"lead": "agent_dm"}},
		"names a third agent":   {f.worker2, "@boss please look", map[string]string{"lead": "agent_dm"}},
		"names the sender":      {f.worker2, "@peer note to self", map[string]string{"lead": "agent_dm"}},
		"group tags":            {f.lead1, "@all @here @ais @humans", map[string]string{"peer": "agent_dm"}},
		"addresses the watcher": {f.worker2, "@usr_alice look", map[string]string{"lead": "agent_dm"}},
	} {
		msg, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: tc.from.ID, Body: tc.body})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := f.notesFor(t, msg.ID); !sameWakes(got, tc.want) {
			t.Errorf("%s: wakes=%v want %v", name, got, tc.want)
		}
		var targets string
		if err := f.s.db.QueryRow(`SELECT targets_json FROM message WHERE id=?`, msg.ID).Scan(&targets); err != nil || strings.Contains(targets, f.alice) {
			t.Errorf("%s: the watching person was addressed: %s %v", name, targets, err)
		}
	}
}

// A person's message in the chat wakes the agents whose project that person reaches: all of them by default, the one named otherwise
// (also project-qualified, as the room has no project of its own), and never an agent of a project they only read or do not belong to.
func TestAgentDMPersonMessageWakesOnlyWhatTheyReach(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.alice, f.bob, f.vera, f.owner)
	for name, tc := range map[string]struct {
		author string
		body   string
		want   map[string]string
	}{
		"owner of one project, unaddressed":     {f.alice, "what do you two think", map[string]string{"lead": "human_message"}},
		"owner of one project, names the other": {f.alice, "@peer what do you think", map[string]string{}},
		"owner of one project, names a role":    {f.alice, "@worker and @lead", map[string]string{"lead": "mention"}},
		"qualified, own project":                {f.alice, "@lead@wake please", map[string]string{"lead": "mention"}},
		"qualified, the other project":          {f.alice, "@peer@other please", map[string]string{}},
		"operator of the other, unaddressed":    {f.bob, "what do you two think", map[string]string{"peer": "human_message"}},
		"operator of the other, names the lead": {f.bob, "@lead please", map[string]string{}},
		"a viewer reaches neither":              {f.vera, "anyone?", map[string]string{}},
		"the instance owner, unaddressed":       {f.owner, "both of you", map[string]string{"lead": "human_message", "peer": "human_message"}},
		"the instance owner, names one":         {f.owner, "@peer only you", map[string]string{"peer": "mention"}},
		"the instance owner, qualified":         {f.owner, "@lead@wake and @peer@other", map[string]string{"lead": "mention", "peer": "mention"}},
		"the instance owner, names nobody":      {f.owner, "@peer@wake", map[string]string{"lead": "human_message", "peer": "human_message"}}, // names nobody, so it is unaddressed
		"another person is named":               {f.owner, "@usr_bob did you see this", map[string]string{}},
	} {
		msg, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: tc.author, Body: tc.body})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := f.notesFor(t, msg.ID); !sameWakes(got, tc.want) {
			t.Errorf("%s: wakes=%v want %v", name, got, tc.want)
		}
	}
	// The ones left out are recorded as such, and the message stays in the chat.
	var skipped int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='message_not_woken' AND room_id=? AND json_extract(payload_json,'$.reason')='no_reach'`, room.ID).Scan(&skipped); err != nil || skipped == 0 {
		t.Fatalf("no_reach skips=%d err=%v", skipped, err)
	}
}

// An hour of turns is the limit: past it an agent's message is kept but wakes nobody (and says so once), a person's still wakes.
func TestAgentDMTurnLimit(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.alice)
	addTurns := func(n int, startedAt string) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := f.s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,started_at) VALUES (?,?,'ses_peer2',0,?,1,'normal','exited',?)`,
				"trn_"+startedAt+"_"+time.Now().Format("150405.000000000")+string(rune('a'+i%26))+string(rune('a'+i/26)), f.worker2.ID, room.ID, startedAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Turns older than an hour do not count.
	addTurns(AgentDMTurnsPerHour, time.Now().UTC().Add(-2*time.Hour).Format(timestampLayout))
	msg, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: f.lead1.ID, Body: "still allowed"})
	if err != nil || !sameWakes(f.notesFor(t, msg.ID), map[string]string{"peer": "agent_dm"}) {
		t.Fatalf("old turns held the chat: %v %v", f.notesFor(t, msg.ID), err)
	}
	addTurns(AgentDMTurnsPerHour, timestamp())
	capped, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: f.lead1.ID, Body: "over the limit"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.notesFor(t, capped.ID); len(got) != 0 {
		t.Fatalf("the limit did not hold: %v", got)
	}
	var skipped int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='message_not_woken' AND room_id=? AND json_extract(payload_json,'$.reason')='dm_turn_cap'`, room.ID).Scan(&skipped); err != nil || skipped != 1 {
		t.Fatalf("skip events=%d err=%v", skipped, err)
	}
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: f.worker2.ID, Body: "again"}); err != nil {
		t.Fatal(err)
	}
	var notices int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body=?`, room.ID, agentChatCapNotice).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("notices=%d err=%v", notices, err)
	}
	f.join(t, room.ID, f.owner)
	person, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: f.owner, Body: "@peer carry on"})
	if err != nil || !sameWakes(f.notesFor(t, person.ID), map[string]string{"peer": "mention"}) {
		t.Fatalf("a person's message at the limit: %v %v", f.notesFor(t, person.ID), err)
	}
}

// Whatever a person writes, however it addresses an agent (a reply, a name, a group tag, a handoff, an Inbox answer), an agent of a project they
// do not reach stays asleep: the check sits where notes are made, so no route around it exists.
func TestAgentDMNoReachHoldsOnEveryWakePath(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.bob) // operator of the second project only: reaches the peer, not the lead
	theirs, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: f.lead1.ID, Body: "a question from the first project"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		message Message
		want    map[string]string
	}{
		"a reply that also names the agent": {Message{Body: "@lead here is the answer", ReplyTo: theirs.ID}, map[string]string{}},
		"a plain reply":                     {Message{Body: "here is the answer", ReplyTo: theirs.ID}, map[string]string{}},
		"group tags":                        {Message{Body: "@all @ais @here @channel"}, map[string]string{"peer": "group"}},
		"a handoff to the lead":             {Message{Body: "done, over to you", Handoff: true}, map[string]string{}},
		"an Inbox answer to the lead":       {Message{Body: "A", OnlyTargets: []string{f.lead1.ID}}, map[string]string{}},
		"an Inbox answer to the peer":       {Message{Body: "A", OnlyTargets: []string{f.worker2.ID}}, map[string]string{"peer": "inbox_answer"}},
	} {
		tc.message.RoomID, tc.message.AuthorKind, tc.message.AuthorID = room.ID, "user", f.bob
		msg, err := f.s.InsertMessage(ctx, tc.message)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := f.notesFor(t, msg.ID); !sameWakes(got, tc.want) {
			t.Errorf("%s: wakes=%v want %v", name, got, tc.want)
		}
	}
	var queued int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND state='queued'`, f.lead1.ID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("the first project's lead has %d queued notes after a person who does not reach it wrote (%v)", queued, err)
	}
}
