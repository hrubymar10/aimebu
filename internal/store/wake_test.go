package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func wakeFixture(t *testing.T) (*Store, Project, Room, Agent, Agent) {
	t.Helper()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	project, err := s.CreateProject(ctx, Project{Name: "wake", Folder: "/tmp/wake"})
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "task", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_worker','worker','Worker','work','worker')`,
		`INSERT OR REPLACE INTO role(id,key,label,body,role_key,cap_is_lead) VALUES ('role_lead','lead','Lead','lead','lead',1)`,
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_wake','host','hash',1,1)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_wake',?,'mch_wake','/tmp/wake')`, project.ID); err != nil {
		t.Fatal(err)
	}
	makeAgent := func(slug, role string) Agent {
		agent, err := s.CreateAgent(ctx, Agent{Slug: slug, ProjectID: project.ID, RoleID: role, Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_wake", KeyHash: "key", CreatedBy: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, room.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?, ?,0,'fresh_with_history',?)`, "ses_"+slug, agent.ID, timestamp()); err != nil {
			t.Fatal(err)
		}
		return agent
	}
	return s, project, room, makeAgent("one", "role_worker"), makeAgent("lead", "role_lead")
}

func TestInboxAnswerWakesOnlyRequesterAndDedupes(t *testing.T) {
	t.Parallel()
	s, project, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_wake','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_wake','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	ask, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "Which path?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,agent_id,requester_kind,requester_id,kind,created_at) VALUES ('inb_wake',?,?,?,?,'agent',?,'question','now')`, project.ID, room.ID, ask.ID, one.ID, one.ID); err != nil {
		t.Fatal(err)
	}
	answer, reused, err := s.AnswerInbox(ctx, "inb_wake", "usr_wake", "request-1", "Use A", 0)
	if err != nil || reused {
		t.Fatalf("answer=%+v reused=%t %v", answer, reused, err)
	}
	if _, err := s.NoteForAgentMessage(ctx, one.ID, answer.ID); err != nil {
		t.Fatalf("requester not woken: %v", err)
	}
	if _, err := s.NoteForAgentMessage(ctx, lead.ID, answer.ID); err == nil {
		t.Fatal("nonrequester woken")
	}
	replay, reused, err := s.AnswerInbox(ctx, "inb_wake", "usr_wake", "request-1", "Use A", 0)
	if err != nil || !reused || replay.ID != answer.ID {
		t.Fatalf("replay=%+v reused=%t %v", replay, reused, err)
	}
	if _, _, err := s.AnswerInbox(ctx, "inb_wake", "usr_wake", "request-1", "Use B", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay=%v", err)
	}
}

func TestMessageWakeTargetsAndHandoff(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	tests := []struct {
		name      string
		message   Message
		one, lead bool
	}{
		{"human all", Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "please work"}, true, true},
		{"human one", Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one please work"}, true, false},
		{"full mention", Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one@wake please work"}, true, false},
		{"uppercase full ID", Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@" + strings.ToUpper(one.ID) + " please work"}, true, false},
		{"escaped", Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "`@one` and \\@lead are examples"}, true, true},
		{"agent unaddressed is a broadcast", Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "status"}, false, true},
		{"role mention", Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "@lead review"}, false, true},
		{"quoted double backticks name nobody, so it is a broadcast", Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "``@lead `still code``"}, false, true},
		{"mention of nobody is still a mention", Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "@ghost status"}, false, false},
		{"address after double backticks", Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "``@lead `still code`` @lead review"}, false, true},
		{"unmatched single before double", Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "` unmatched `` @lead review"}, false, true},
		{"handoff", Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "finished", Handoff: true}, false, true},
		{"self", Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "@one note"}, false, false},
		{"system routine", Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: "membership changed"}, false, false},
		{"system targeted", Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: "recovery action", OnlyTargets: []string{one.ID}}, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := s.InsertMessage(ctx, test.message)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				agent Agent
				want  bool
			}{{one, test.one}, {lead, test.lead}} {
				_, err := s.NoteForAgentMessage(ctx, item.agent.ID, message.ID)
				if (err == nil) != item.want {
					t.Fatalf("%s woke=%t want=%t error=%v", item.agent.Slug, err == nil, item.want, err)
				}
			}
			if test.name == "human one" {
				var targets []string
				if err := json.Unmarshal([]byte(message.TargetsJSON), &targets); err != nil || len(targets) != 1 || targets[0] != one.ID {
					t.Fatalf("targets=%v %v", targets, err)
				}
			}
		})
	}
}

func TestMentionWakeReasonKeepsStrongestMatch(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	for _, test := range []struct {
		body, want string
	}{
		{"@one please act; @worker and @channel coordinate", "mention"},
		{"@worker please act; @channel coordinate", "role"},
		{"@channel please act", "group"},
	} {
		message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: test.body})
		if err != nil {
			t.Fatal(err)
		}
		note, err := s.NoteForAgentMessage(ctx, one.ID, message.ID)
		if err != nil || note.WakeReason != test.want {
			t.Fatalf("body=%q wake reason=%q want=%q err=%v", test.body, note.WakeReason, test.want, err)
		}
	}
}

func TestAgentReplyWakesTheParentAuthorAndMentionsStillWork(t *testing.T) {
	t.Parallel()
	s, _, room, worker, lead := wakeFixture(t)
	ctx := context.Background()
	parent, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: worker.ID, Body: "work complete"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, kind, author, body string
		wake                     bool
	}{
		{"agent reply", "agent", lead.ID, "review pending", true},
		{"agent mention", "agent", lead.ID, "@one please revise", true},
		{"human reply", "user", "owner", "please revise", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: tc.kind,
				AuthorID: tc.author, Body: tc.body, ReplyTo: parent.ID})
			if err != nil {
				t.Fatal(err)
			}
			note, err := s.NoteForAgentMessage(ctx, worker.ID, message.ID)
			if (err == nil) != tc.wake {
				t.Fatalf("worker woke=%t want=%t: %v", err == nil, tc.wake, err)
			}
			if tc.name == "agent reply" && note.WakeReason != "reply" {
				t.Fatalf("agent reply wake reason=%q, want reply", note.WakeReason)
			}
			if tc.name == "human reply" {
				if note.WakeReason != "reply" {
					t.Fatalf("human reply wake reason=%q", note.WakeReason)
				}
				if _, err := s.NoteForAgentMessage(ctx, lead.ID, message.ID); err == nil {
					t.Fatal("human reply woke an agent other than its parent author")
				}
			}
		})
	}
}

func TestAgentReplyTargetsHumanParent(t *testing.T) {
	t.Parallel()
	s, project, room, worker, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_reply','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_reply','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "user", "usr_reply"); err != nil {
		t.Fatal(err)
	}
	parent, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_reply", Body: "How did it go?"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: worker.ID, Body: "Done.", ReplyTo: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	if err := json.Unmarshal([]byte(reply.TargetsJSON), &targets); err != nil || len(targets) != 1 || targets[0] != "usr_reply" {
		t.Fatalf("human reply targets=%v: %v", targets, err)
	}
}

func TestWakeQueuePausesStopsCoalescesAndDrops(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	busy, err := s.RecordTurn(ctx, Turn{AgentID: one.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET gate='paused' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	var first Message
	for i := 0; i < 5; i++ {
		message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: fmt.Sprintf("task %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = message
		}
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || ready {
		t.Fatalf("paused ready=%t %v", ready, err)
	}
	if _, err := s.db.Exec(`UPDATE room SET gate='open' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',desired_rev=1 WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || ready {
		t.Fatalf("stopped ready=%t %v", ready, err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='active',desired_rev=2 WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || ready {
		t.Fatalf("busy ready=%t %v", ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, busy.ID); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 5 || delivery.Notes[0].MessageID != first.ID {
		t.Fatalf("coalesced=%+v ready=%t %v", delivery, ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, delivery.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, room.ID, "agent", one.ID); err != nil {
		t.Fatal(err)
	}
	var dropped int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND state='dropped'`, one.ID).Scan(&dropped); err != nil || dropped != 5 {
		t.Fatalf("dropped=%d %v", dropped, err)
	}
}

func TestDispatchRoundsRoomsAndRespectsSwitchGate(t *testing.T) {
	t.Parallel()
	s, project, first, one, _ := wakeFixture(t)
	ctx := context.Background()
	second, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "second", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, second.ID, "agent", one.ID); err != nil {
		t.Fatal(err)
	}
	for _, room := range []Room{first, second} {
		if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "work"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO dispatch_switch_gate(machine_id,tool,gate) VALUES ('mch_wake','fake','paused')`); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || ready {
		t.Fatalf("switch gate ready=%t %v", ready, err)
	}
	if _, err := s.db.Exec(`UPDATE dispatch_switch_gate SET gate='open' WHERE machine_id='mch_wake' AND tool='fake'`); err != nil {
		t.Fatal(err)
	}
	a, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready || len(a.Notes) != 1 || a.Notes[0].RoomID != first.ID {
		t.Fatalf("first room=%+v ready=%t %v", a, ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, a.TurnID); err != nil {
		t.Fatal(err)
	}
	b, ready, err := s.PrepareDispatch(ctx, one.ID, first.ID)
	if err != nil || !ready || len(b.Notes) != 1 || b.Notes[0].RoomID != second.ID {
		t.Fatalf("second room=%+v ready=%t %v", b, ready, err)
	}
}

func TestProjectHumansAreAddressableInOrdinaryRoomsOnly(t *testing.T) {
	t.Parallel()
	s, project, room, _, _ := wakeFixture(t)
	ctx := context.Background()
	for _, user := range []struct{ id, login string }{{"usr_alice", "alice"}, {"usr_bob", "bob"}, {"usr_carol", "carol"}, {"usr_gone", "gone"}, {"usr_one", "one"}} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, user.id, user.login); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, project.ID, user.id); err != nil {
			t.Fatal(err)
		}
	}
	// outsider is not in the project at all; gone is a disabled project member.
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_out','outsider','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE user SET disabled_at='now' WHERE id='usr_gone'`); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "user", "usr_bob"); err != nil {
		t.Fatal(err)
	}
	targetsOf := func(roomID, author, body string) map[string]bool {
		t.Helper()
		message, err := s.InsertMessage(ctx, Message{RoomID: roomID, AuthorKind: "user", AuthorID: author, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		if err := json.Unmarshal([]byte(message.TargetsJSON), &ids); err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, id := range ids {
			got[id] = true
		}
		return got
	}

	// A project human with no room_member row is addressed by login.
	if got := targetsOf(room.ID, "usr_bob", "@alice please look"); !got["usr_alice"] {
		t.Fatalf("project human not addressed: %v", got)
	}
	// Non-members of the project and disabled users are not.
	if got := targetsOf(room.ID, "usr_bob", "@outsider @gone hi"); got["usr_out"] || got["usr_gone"] {
		t.Fatalf("outsider or disabled user addressed: %v", got)
	}
	// Group tags keep meaning the room's explicit members.
	if got := targetsOf(room.ID, "usr_alice", "@humans @channel sync"); got["usr_carol"] || !got["usr_bob"] {
		t.Fatalf("group tag reached implicit humans or missed a member: %v", got)
	}
	// A login equal to an agent slug stays ambiguous.
	if got := targetsOf(room.ID, "usr_bob", "@one are you there"); len(got) != 0 {
		t.Fatalf("ambiguous slug/login addressed someone: %v", got)
	}
	// Logins listed for the client match what the server resolves.
	logins, err := s.RoomUserLogins(ctx, room.ID)
	if err != nil || strings.Join(logins, ",") != "alice,bob,carol,one" {
		t.Fatalf("logins=%v err=%v", logins, err)
	}

	// A DM stays members-only.
	dm, err := s.OpenUserDM(ctx, project.ID, "usr_alice", "usr_bob")
	if err != nil {
		t.Fatal(err)
	}
	if got := targetsOf(dm.ID, "usr_alice", "@carol join us"); got["usr_carol"] {
		t.Fatalf("project human addressed in a DM: %v", got)
	}
	logins, err = s.RoomUserLogins(ctx, dm.ID)
	if err != nil || strings.Join(logins, ",") != "alice,bob" {
		t.Fatalf("dm logins=%v err=%v", logins, err)
	}
}

func TestRoomMemberCountSkipsDisabledUsers(t *testing.T) {
	t.Parallel()
	s, project, room, _, _ := wakeFixture(t)
	ctx := context.Background()
	for _, id := range []string{"usr_a", "usr_b"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, project.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	dm, err := s.OpenUserDM(ctx, project.ID, "usr_a", "usr_b")
	if err != nil {
		t.Fatal(err)
	}
	counts := func() (ordinary, private int) {
		t.Helper()
		rooms, err := s.RoomsForUserProject(ctx, project.ID, "usr_a")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rooms {
			switch r.ID {
			case room.ID:
				ordinary = r.MemberCount
			case dm.ID:
				private = r.MemberCount
			}
		}
		return ordinary, private
	}
	// Two project humans plus the room's two agents; the DM has its two people.
	if ordinary, private := counts(); ordinary != 4 || private != 2 {
		t.Fatalf("before: ordinary=%d private=%d", ordinary, private)
	}
	if _, err := s.db.Exec(`UPDATE user SET disabled_at='now' WHERE id='usr_b'`); err != nil {
		t.Fatal(err)
	}
	if ordinary, private := counts(); ordinary != 3 || private != 1 {
		t.Fatalf("after disabling: ordinary=%d private=%d, want 3 and 1", ordinary, private)
	}
	all, err := s.RoomsForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.ID == room.ID && r.MemberCount != 3 {
			t.Fatalf("RoomsForProject count=%d, want 3", r.MemberCount)
		}
	}
}

// Agents answering each other by reply wake one another, but only ReplyWakeHops times in a row: the next reply is threaded and wakes nobody,
// once says so, and a person's message starts the count again.
func TestAgentReplyWakesAreBoundedByTheHopGuard(t *testing.T) {
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
	guards := func() int {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='loop_guard' AND room_id=?`, room.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	notices := func() int {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body LIKE 'Agents stopped answering each other%'`, room.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	// A person starts it; the worker answers (waking the person is not an agent wake), then the two answer each other.
	start := say("user", "owner", "kick off", "")
	previous := say("agent", worker.ID, "on it", start.ID)
	agents := []string{lead.ID, worker.ID}
	for hop := 1; hop <= ReplyWakeHops; hop++ {
		author, parentAuthor := agents[(hop-1)%2], agents[hop%2]
		next := say("agent", author, fmt.Sprintf("reply %d", hop), previous.ID)
		if !woken(parentAuthor, next) {
			t.Fatalf("reply %d should wake the parent's author", hop)
		}
		previous = next
	}
	if guards() != 0 {
		t.Fatal("no guard before the limit")
	}
	// The next reply is one too many: threaded, not woken, and announced once.
	author, parentAuthor := agents[ReplyWakeHops%2], agents[(ReplyWakeHops+1)%2]
	blocked := say("agent", author, "one too many", previous.ID)
	if woken(parentAuthor, blocked) {
		t.Fatal("a reply past the limit must not wake")
	}
	if guards() != 1 || notices() != 1 {
		t.Fatalf("guard events=%d notices=%d, want one each", guards(), notices())
	}
	// Going on past the limit says nothing more and still wakes nobody.
	again := say("agent", parentAuthor, "still going", blocked.ID)
	if woken(author, again) || guards() != 1 || notices() != 1 {
		t.Fatalf("past the limit: woken=%v guards=%d notices=%d", woken(author, again), guards(), notices())
	}
	// A mention is a different thing and always works.
	mention := say("agent", lead.ID, "@one are you there", again.ID)
	if !woken(worker.ID, mention) {
		t.Fatal("a mention still wakes")
	}
	// A person replying into the chain starts the count again.
	human := say("user", "owner", "carry on", again.ID)
	next := say("agent", worker.ID, "thanks", human.ID)
	back := say("agent", lead.ID, "you're welcome", next.ID)
	if !woken(worker.ID, back) {
		t.Fatal("after a person's message the agents may answer each other again")
	}
}

// Two agents that answer every note they are woken by with a reply would go on for ever; the guard stops the exchange after
// ReplyWakeHops automatic replies. This plays that out until nothing is left to answer.
func TestTwoRepliersStopAfterTheHopLimit(t *testing.T) {
	t.Parallel()
	s, _, room, worker, lead := wakeFixture(t)
	ctx := context.Background()
	first, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: worker.ID, Body: "@lead status?"})
	if err != nil {
		t.Fatal(err)
	}
	// The lead replying to it is what sets the exchange going. (The question names the lead, so it is a mention and not a broadcast, which would
	// count against the limit like a reply.)
	opening, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "looking", ReplyTo: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	pending := []Message{opening}
	sent := map[string]bool{first.ID: true, opening.ID: true}
	agentMessages := 2
	for round := 0; round < 50 && len(pending) > 0; round++ {
		var next []Message
		for _, message := range pending {
			for _, agent := range []string{worker.ID, lead.ID} {
				if _, err := s.NoteForAgentMessage(ctx, agent, message.ID); err != nil {
					continue // not woken by this one
				}
				reply, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: agent, Body: "answering", ReplyTo: message.ID})
				if err != nil {
					t.Fatal(err)
				}
				agentMessages++
				if !sent[reply.ID] {
					sent[reply.ID] = true
					next = append(next, reply)
				}
			}
		}
		pending = next
	}
	if len(pending) != 0 {
		t.Fatal("the exchange never stopped")
	}
	// The message that was replied to, ReplyWakeHops replies that each wake the other agent (the lead's opening one included), and the one
	// reply that is threaded but wakes nobody.
	if want := 1 + ReplyWakeHops + 1; agentMessages != want {
		t.Fatalf("agent messages=%d, want %d", agentMessages, want)
	}
	var paused int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM agent WHERE desired='not_accepting'`).Scan(&paused); err != nil || paused != 0 {
		t.Fatalf("the hop guard should stop it long before the turn-budget pause: paused=%d %v", paused, err)
	}
}

// The card of an agent whose running turn began with another agent's reply says so; a turn begun by anything else does not.
func TestAgentCardTagsATurnWokenByAReply(t *testing.T) {
	t.Parallel()
	s, _, room, worker, lead := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET observed='in_turn' WHERE id IN (?,?)`, worker.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	asked, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: worker.ID, Body: "status?"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "fine", ReplyTo: asked.ID})
	if err != nil {
		t.Fatal(err)
	}
	note, err := s.NoteForAgentMessage(ctx, worker.ID, reply.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := s.db.QueryRow(`SELECT id FROM session WHERE agent_id=? LIMIT 1`, worker.ID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	turn, err := s.RecordTurn(ctx, Turn{AgentID: worker.ID, SessionID: sessionID, Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.ID); err != nil {
		t.Fatal(err)
	}
	cards, err := s.AgentCardsForRoom(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, card := range cards {
		got[card.ID] = card.WokenBy
	}
	if got[worker.ID] != "reply" || got[lead.ID] != "" {
		t.Fatalf("woken_by=%v", got)
	}
	single, err := s.AgentCardByID(ctx, worker.ID)
	if err != nil || single.WokenBy != "reply" {
		t.Fatalf("single card: %q %v", single.WokenBy, err)
	}
}
