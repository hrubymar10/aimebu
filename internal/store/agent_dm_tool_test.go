package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// leadTurn gives the lead one running turn that the given people's messages caused (none: an agent or a schedule woke it).
func (f dmFixture) leadTurn(t *testing.T, lead Agent, people ...string) Turn {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE agent_id=? AND exec IN ('pending','running')`, lead.ID); err != nil {
		t.Fatal(err)
	}
	var session string
	if err := f.s.db.QueryRow(`SELECT id FROM session WHERE agent_id=?`, lead.ID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	home := f.task1
	if lead.ProjectID == f.p2.ID {
		home = f.task2
	}
	turn, err := f.s.RecordTurn(ctx, Turn{AgentID: lead.ID, SessionID: session, Generation: 0, RoomID: home.ID, Attempt: 1, Mode: "normal", Exec: "running"})
	if err != nil {
		t.Fatal(err)
	}
	for _, person := range people {
		msg, err := f.s.InsertMessage(ctx, Message{RoomID: home.ID, AuthorKind: "user", AuthorID: person, Body: "please talk to the other project"})
		if err != nil {
			t.Fatal(err)
		}
		var note string
		if err := f.s.db.QueryRow(`SELECT id FROM note WHERE agent_id=? AND message_id=?`, lead.ID, msg.ID).Scan(&note); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note); err != nil {
			t.Fatal(err)
		}
	}
	return turn
}

func (f dmFixture) dm(t *testing.T, from, to Agent, body string) (AgentDMResult, error) {
	t.Helper()
	var result AgentDMResult
	err := f.s.Tx(context.Background(), func(tx *Tx) (err error) {
		result, err = tx.DMAgentAsTool(from.ID, to.ID, body)
		return err
	})
	return result, err
}

// The people who caused the lead's turn decide, and every one of them has to reach the target's project.
func TestDMAgentAuthorizationFollowsWhoWokeTheTurn(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	if _, err := f.s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, f.p1.ID, f.bob); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		people []string
		want   error
	}{
		"owner of only the lead's project":    {[]string{f.alice}, ErrForbidden},
		"operator of the target's project":    {[]string{f.bob}, nil},
		"viewer of the target's project":      {[]string{f.vera}, ErrForbidden},
		"the instance owner":                  {[]string{f.owner}, nil},
		"one who reaches and one who doesn't": {[]string{f.bob, f.alice}, ErrForbidden},
		"two who reach":                       {[]string{f.bob, f.owner}, nil},
		"a stranger":                          {[]string{f.carol}, ErrForbidden},
		// No person: an owner of the lead's own project stands in; bob owns it here and operates the target's project.
		"woken by an agent or a timer": {nil, nil},
	} {
		f.leadTurn(t, f.lead1, tc.people...)
		if _, err := f.dm(t, f.lead1, f.lead2, "hello"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", name, err, tc.want)
		}
	}
	// Without a person, the lead's project's owners are the ones: alice owns it and has no reach into the target's project.
	if _, err := f.s.db.Exec(`DELETE FROM membership WHERE project_id=? AND user_id=?`, f.p1.ID, f.bob); err != nil {
		t.Fatal(err)
	}
	f.leadTurn(t, f.lead1)
	if _, err := f.dm(t, f.lead1, f.lead2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a timer-woken lead whose owner has no reach: %v", err)
	}
	// An owner who is also the instance owner reaches every project.
	if _, err := f.s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, f.p1.ID, f.owner); err != nil {
		t.Fatal(err)
	}
	f.leadTurn(t, f.lead1)
	if _, err := f.dm(t, f.lead1, f.lead2, "hello"); err != nil {
		t.Fatalf("a timer-woken lead whose owner is the instance owner: %v", err)
	}
	// A retry or a correction continues the turn that a person caused.
	parent := f.leadTurn(t, f.lead1, f.carol)
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,parent_turn_id,attempt,mode,exec) VALUES ('trn_fix',?,'ses_lead',0,?,?,1,'correction','running')`, f.lead1.ID, f.task1.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.lead2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a correction of a turn a stranger caused: %v", err)
	}
	// Outside a turn there is nobody to ask.
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='exited' WHERE agent_id=?`, f.lead1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.lead2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a call outside any turn: %v", err)
	}
}

func TestDMAgentOnlyAnActiveLeadMayStartAChat(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	if _, err := f.dm(t, f.worker1, f.worker2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a worker: %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET desired='not_accepting' WHERE id=?`, f.lead1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.lead2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a lead that is not accepting work: %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET desired='active' WHERE id=?`, f.lead1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.lead1, "hello"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a chat with oneself: %v", err)
	}
	if _, err := f.dm(t, f.lead1, Agent{ID: "agt_nobody"}, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent that does not exist answers like one out of reach: %v", err)
	}
}

func TestDMAgentTargetMustBeLive(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	for name, tc := range map[string]struct {
		query string
		want  error
	}{
		"stopped":       {`UPDATE agent SET desired='stopped' WHERE id=?`, ErrAgentUnavailable},
		"not accepting": {`UPDATE agent SET desired='not_accepting' WHERE id=?`, ErrAgentUnavailable},
		"revoked":       {`UPDATE agent SET revoked_at='2026-01-01T00:00:00.000Z' WHERE id=?`, ErrAgentUnavailable},
		"archived":      {`UPDATE project SET archived_at='2026-01-01T00:00:00.000Z' WHERE id=(SELECT project_id FROM agent WHERE id=?)`, ErrProjectArchived},
	} {
		if _, err := f.s.db.Exec(tc.query, f.lead2.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.dm(t, f.lead1, f.lead2, "hello"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", name, err, tc.want)
		}
		for _, undo := range []string{`UPDATE agent SET desired='active',revoked_at=NULL WHERE id=?`, `UPDATE project SET archived_at=NULL WHERE id=(SELECT project_id FROM agent WHERE id=?)`} {
			if _, err := f.s.db.Exec(undo, f.lead2.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The lead's own archived project cannot start anything either.
	if _, err := f.s.db.Exec(`UPDATE project SET archived_at='2026-01-01T00:00:00.000Z' WHERE id=?`, f.p1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.lead2, "hello"); !errors.Is(err, ErrProjectArchived) {
		t.Fatalf("an archived lead project: %v", err)
	}
}

func TestDMAgentOpensTheChatAndWakesTheTarget(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	first, err := f.dm(t, f.lead1, f.lead2, "can you review the API shape?")
	if err != nil || !first.Created || first.No == 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if got := f.notesFor(t, first.MessageID); !sameWakes(got, map[string]string{"boss": "agent_dm"}) {
		t.Fatalf("wakes=%v", got)
	}
	second, err := f.dm(t, f.lead1, f.lead2, "and the error codes")
	if err != nil || second.Created || second.RoomID != first.RoomID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	// The target is woken under its own project and sees the chat.
	delivery, ready, err := f.s.PrepareDispatch(context.Background(), f.lead2.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 2 || delivery.Notes[0].RoomID != first.RoomID {
		t.Fatalf("delivery=%+v ready=%t err=%v", delivery, ready, err)
	}
	// The other lead can open its own chat with the same agent: a pair each.
	var other AgentDMResult
	f.leadTurn(t, f.lead2, f.owner)
	if other, err = f.dm(t, f.lead2, f.worker2, "hi"); err != nil || other.RoomID == first.RoomID {
		t.Fatalf("other=%+v err=%v", other, err)
	}
}

func TestDMAgentLimits(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	chat, err := f.dm(t, f.lead1, f.lead2, "hello")
	if err != nil {
		t.Fatal(err)
	}
	// A chat that used its hour of turns refuses a message the lead would send into it.
	for i := 0; i < AgentDMTurnsPerHour; i++ {
		if _, err := f.s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,started_at) VALUES (?,?,'ses_boss2',0,?,1,'normal','exited',?)`,
			"trn_cap_"+string(rune('a'+i)), f.lead2.ID, chat.RoomID, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.dm(t, f.lead1, f.lead2, "more"); !errors.Is(err, ErrDMTurnCap) {
		t.Fatalf("turn limit: %v", err)
	}
	// A lead may open only so many new chats an hour; the ones it already has still work.
	now := time.Now().UTC().Format(timestampLayout)
	for i := 0; i < AgentDMNewChatsPerHour; i++ {
		if _, err := f.s.db.Exec(`INSERT INTO room(id,project_id,depth,kind,dm_key,title,created_by,created_at) VALUES (?,?,0,'dm',?,'x',?,?)`,
			"room_fan_"+string(rune('a'+i)), AgentDMProjectID, "agents:fan"+string(rune('a'+i)), f.lead1.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.dm(t, f.lead1, f.worker1, "a new one"); !errors.Is(err, ErrDMFanOutCap) {
		t.Fatalf("new chat limit: %v", err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM turn WHERE id LIKE 'trn_cap_%'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.lead2, "an existing chat is not a new one"); err != nil {
		t.Fatalf("existing chat: %v", err)
	}
}

// slug@project names an agent the way people write it; the same authorization decides, and the errors tell only what the caller's people
// could see anyway.
func TestDMAgentResolvesSlugAtProject(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	if _, err := f.s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, f.p1.ID, f.bob); err != nil {
		t.Fatal(err)
	}
	resolve := func(target string) (AgentDMResult, error) {
		var result AgentDMResult
		err := f.s.Tx(context.Background(), func(tx *Tx) (err error) {
			result, err = tx.DMAgentAsTool(f.lead1.ID, target, "hello")
			return err
		})
		return result, err
	}
	f.leadTurn(t, f.lead1, f.bob)
	byID, err := resolve(f.lead2.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"boss@other", "BOSS@Other", " boss@" + f.p2.ID} {
		got, err := resolve(target)
		if err != nil || got.RoomID != byID.RoomID {
			t.Errorf("%q: %+v err=%v, want the chat %s", target, got, err, byID.RoomID)
		}
	}
	var missing *AgentNotFoundError
	if _, err := resolve("ghost@other"); !errors.As(err, &missing) || err.Error() != "no agent ghost in project other" {
		t.Errorf("a missing agent in a reachable project: %v", err)
	}
	// Nothing is confirmed about a project, or an agent in it, that the people behind the turn cannot reach.
	for _, target := range []string{"nowhere@nowhere", "ghost@nowhere", "@other", "boss@"} {
		if _, err := resolve(target); !errors.Is(err, ErrForbidden) {
			t.Errorf("%q with an unknown project: %v", target, err)
		}
	}
	f.leadTurn(t, f.lead1, f.carol)
	for _, target := range []string{"boss@other", "ghost@other", f.lead2.ID} {
		if _, err := resolve(target); !errors.Is(err, ErrForbidden) {
			t.Errorf("%q without access: %v", target, err)
		}
	}
	// Two reachable projects of the same name make a slug ambiguous: the IDs of exactly those agents are listed.
	p3, err := f.s.CreateProject(context.Background(), Project{Name: "Other", Folder: "/tmp/other3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_other3',?,'mch_wake','/tmp/other3')`, p3.ID); err != nil {
		t.Fatal(err)
	}
	twin, err := f.s.CreateAgent(context.Background(), Agent{Slug: "boss", ProjectID: p3.ID, RoleID: "role_lead", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_other3", KeyHash: "key-twin", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	f.leadTurn(t, f.lead1, f.owner)
	var ambiguous *AgentAmbiguousError
	_, ambErr := resolve("boss@other")
	if !errors.As(ambErr, &ambiguous) || len(ambiguous.IDs) != 2 {
		t.Fatalf("two projects named other: %v", ambErr)
	}
	for _, id := range []string{f.lead2.ID, twin.ID} {
		if !strings.Contains(ambErr.Error(), id) {
			t.Errorf("the ambiguity does not list %s: %v", id, ambErr)
		}
	}
	// Bob reaches only the first of them, so for him the name is no longer ambiguous and the other is not listed.
	f.leadTurn(t, f.lead1, f.bob)
	if got, err := resolve("boss@other"); err != nil || got.RoomID != byID.RoomID {
		t.Fatalf("bob reaches one project: %+v err=%v", got, err)
	}
}

// A note about a message from an agent of another project carries that agent's ID next to its name, so a reply by ID is always possible; an
// agent of the recipient's own project does not need it.
func TestNotesCarryTheIDOfAnAgentOfAnotherProject(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	sent, err := f.dm(t, f.lead1, f.lead2, "hello")
	if err != nil {
		t.Fatal(err)
	}
	notes := deliveredNotes(t, f.s, f.lead2.ID)
	if len(notes) != 1 || notes[0].From != f.lead1.ID || notes[0].FromName != f.lead1.Slug+"@wake" {
		t.Fatalf("notes=%+v", notes)
	}
	// The answer of the other project's agent, and a person's reply to it that mentions the lead: the parent names it with its ID too.
	f.join(t, sent.RoomID, f.owner)
	answer, err := f.s.InsertMessage(context.Background(), Message{RoomID: sent.RoomID, AuthorKind: "agent", AuthorID: f.lead2.ID, Body: "on it"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InsertMessage(context.Background(), Message{RoomID: sent.RoomID, AuthorKind: "user", AuthorID: f.owner, Body: "@" + f.lead1.ID + " thanks", ReplyTo: answer.ID}); err != nil {
		t.Fatal(err)
	}
	// The lead's own turn is over and the person's first message to it was read, so the chat's notes are what is delivered next.
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE agent_id=? AND exec IN ('pending','running')`, f.lead1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE note SET state='done' WHERE agent_id=? AND room_id=?`, f.lead1.ID, f.task1.ID); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, note := range deliveredNotes(t, f.s, f.lead1.ID) {
		switch note.From {
		case f.lead2.ID:
			seen++
			if note.FromName != "boss@other" {
				t.Fatalf("note=%+v", note)
			}
		case f.owner:
			seen++
			if note.FromName != "" || note.InReplyTo == nil || note.InReplyTo.AgentID != f.lead2.ID || note.InReplyTo.From != "boss@other" {
				t.Fatalf("a person's note=%+v in_reply_to=%+v", note, note.InReplyTo)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the two notes", seen)
	}
	// An agent of the recipient's own project is named as before.
	if _, err := f.s.InsertMessage(context.Background(), Message{RoomID: f.task1.ID, AuthorKind: "agent", AuthorID: f.worker1.ID, Body: "@" + f.lead1.Slug + " status?"}); err != nil {
		t.Fatal(err)
	}
	for _, note := range deliveredNotes(t, f.s, f.lead1.ID) {
		if note.From == f.worker1.ID && note.FromName != "" {
			t.Fatalf("same-project note=%+v", note)
		}
	}
}

// A lead is shown the leads of other projects only where dm_agent would let it message them.
func TestOtherLeadersFollowTheDMReachRule(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	list := func() []OtherLeader {
		got, err := f.s.OtherLeadersForTool(context.Background(), f.lead1.ID, f.p1.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	f.leadTurn(t, f.lead1, f.carol)
	if got := list(); len(got) != 0 {
		t.Fatalf("a stranger's turn sees %+v", got)
	}
	f.leadTurn(t, f.lead1, f.vera)
	if got := list(); len(got) != 0 {
		t.Fatalf("a viewer's turn sees %+v", got)
	}
	f.leadTurn(t, f.lead1, f.owner)
	got := list()
	if len(got) != 1 || got[0].ID != f.lead2.ID || got[0].Slug != "boss" || got[0].ProjectSlug != "other" {
		t.Fatalf("the owner's turn sees %+v", got)
	}
}

func (f dmFixture) extraAgent(t *testing.T, slug, role string) Agent {
	t.Helper()
	agent, err := f.s.CreateAgent(context.Background(), Agent{Slug: slug, ProjectID: f.p1.ID, RoleID: role, Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_wake", KeyHash: "key-" + slug, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// Across projects only a lead may be messaged, and only by a lead; any other agent reaches its own project's lead or dispatcher and no one else.
func TestDMAgentWhoMayMessageWhom(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	dispatcher := f.extraAgent(t, "disp", "role_dispatcher_default")
	otherWorker := f.extraAgent(t, "colleague", "role_worker")
	f.leadTurn(t, f.lead1, f.owner)
	for name, tc := range map[string]struct {
		from, to Agent
		want     error
	}{
		"lead to another project's lead":    {f.lead1, f.lead2, nil},
		"lead to another project's worker":  {f.lead1, f.worker2, ErrCrossProjectLeadOnly},
		"lead to its own worker":            {f.lead1, f.worker1, nil},
		"worker to its own lead":            {f.worker1, f.lead1, nil},
		"worker to its own dispatcher":      {f.worker1, dispatcher, nil},
		"worker to a worker":                {f.worker1, otherWorker, ErrForbidden},
		"worker to another project's lead":  {f.worker1, f.lead2, ErrForbidden},
		"worker to another project's agent": {f.worker1, f.worker2, ErrForbidden},
	} {
		if _, err := f.dm(t, tc.from, tc.to, "hello"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", name, err, tc.want)
		}
	}
	// A lead is refused nothing about an agent of a project its people cannot reach: that stays a plain 403.
	f.leadTurn(t, f.lead1, f.carol)
	if _, err := f.dm(t, f.lead1, f.worker2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a lead without reach: %v", err)
	}
}

// message_get: a person's citation reaches an agent across projects; a lead also reads what the other project's lead cited in their chat.
func TestMessageGetAcrossProjects(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	if _, err := f.s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, f.p1.ID, f.bob); err != nil {
		t.Fatal(err)
	}
	cited, err := f.s.InsertMessage(ctx, Message{RoomID: f.task1.ID, AuthorKind: "agent", AuthorID: f.worker1.ID, Body: "the design"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.s.InsertMessage(ctx, Message{RoomID: f.task1.ID, AuthorKind: "agent", AuthorID: f.worker1.ID, Body: "something else"})
	if err != nil {
		t.Fatal(err)
	}
	get := func(agent Agent, no int64) AgentMessage {
		m, err := f.s.AgentMessageByNo(ctx, agent.ID, agent.ProjectID, no)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if get(f.lead2, cited.No).Available || get(f.worker2, cited.No).Available {
		t.Fatal("another project's message is readable before anyone cited it")
	}
	f.join(t, f.task2.ID, f.bob)
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: f.task2.ID, AuthorKind: "user", AuthorID: f.bob, Body: fmt.Sprintf("see #%d", cited.No)}); err != nil {
		t.Fatal(err)
	}
	if got := get(f.worker2, cited.No); !got.Available || got.Body != "the design" || got.Project != "wake" {
		t.Fatalf("a worker, message cited by a person: %+v", got)
	}
	if get(f.worker2, other.No).Available {
		t.Fatal("an uncited message of another project is readable")
	}
	// The lead of project one cites a message to the lead of project two in their chat; a worker cannot be in that chat.
	f.leadTurn(t, f.lead1, f.owner)
	chat, err := f.dm(t, f.lead1, f.lead2, fmt.Sprintf("please read #%d", other.No))
	if err != nil {
		t.Fatal(err)
	}
	if got := get(f.lead2, other.No); !got.Available || got.Body != "something else" || got.Project != "wake" {
		t.Fatalf("a lead, message cited by the other lead in their chat: %+v", got)
	}
	third, err := f.s.CreateAgent(ctx, Agent{Slug: "third", ProjectID: f.p2.ID, RoleID: "role_lead", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_other", KeyHash: "key-third", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if get(third, other.No).Available {
		t.Fatalf("a lead outside the chat reads it (chat %s)", chat.RoomID)
	}
	if get(f.worker2, other.No).Available {
		t.Fatal("a worker reads what a lead cited to another lead")
	}
}
