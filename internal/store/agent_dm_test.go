package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dmFixture is two projects, each with a lead and a worker, and a handful of people:
//
//	owner   the instance owner (first account), a member of nothing
//	alice   owner of the first project only
//	bob     operator of the second project only
//	vera    viewer of the second project only
//	carol   a member of neither
type dmFixture struct {
	s                 *Store
	p1, p2            Project
	lead1, worker1    Agent
	lead2, worker2    Agent
	task1, task2      Room
	owner, alice, bob string
	vera, carol       string
}

func newDMFixture(t *testing.T) dmFixture {
	t.Helper()
	s, p1, task1, worker1, lead1 := wakeFixture(t)
	ctx := context.Background()
	p2, err := s.CreateProject(ctx, Project{Name: "other", Folder: "/tmp/other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_other',?,'mch_wake','/tmp/other')`, p2.ID); err != nil {
		t.Fatal(err)
	}
	makeAgent := func(slug, role string) Agent {
		agent, err := s.CreateAgent(ctx, Agent{Slug: slug, ProjectID: p2.ID, RoleID: role, Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_other", KeyHash: "key-" + slug, CreatedBy: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?, ?,0,'fresh_with_history',?)`, "ses_"+slug+"2", agent.ID, timestamp()); err != nil {
			t.Fatal(err)
		}
		return agent
	}
	f := dmFixture{s: s, p1: p1, p2: p2, task1: task1, worker1: worker1, lead1: lead1,
		lead2: makeAgent("boss", "role_lead"), worker2: makeAgent("peer", "role_worker"),
		owner: "usr_owner", alice: "usr_alice", bob: "usr_bob", vera: "usr_vera", carol: "usr_carol"}
	f.task2, err = s.CreateRoom(ctx, Room{ProjectID: p2.ID, Kind: "task", Title: "work", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []Agent{f.lead2, f.worker2} {
		if err := s.AddMember(ctx, f.task2.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The owner is created first: the first account is the instance owner.
	for _, user := range []string{f.owner, f.alice, f.bob, f.vera, f.carol} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}',?)`, user, user, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range [][3]string{{p1.ID, f.alice, "owner"}, {p2.ID, f.bob, "operator"}, {p2.ID, f.vera, "viewer"}} {
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, m[0], m[1], m[2]); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f dmFixture) open(t *testing.T, a, b Agent) Room {
	t.Helper()
	var room Room
	if err := f.s.Tx(context.Background(), func(tx *Tx) (err error) {
		room, _, err = tx.OpenAgentDM(a.ID, b.ID, a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return room
}

func (f dmFixture) join(t *testing.T, roomID string, users ...string) {
	t.Helper()
	for _, user := range users {
		if err := f.s.Tx(context.Background(), func(tx *Tx) error { return tx.AddMember(roomID, "user", user) }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenAgentDMIsOnePerPairAcrossProjects(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	var first, second Room
	var created, createdAgain bool
	if err := f.s.Tx(ctx, func(tx *Tx) (err error) {
		first, created, err = tx.OpenAgentDM(f.lead1.ID, f.worker2.ID, f.lead1.ID)
		return err
	}); err != nil || !created {
		t.Fatalf("first=%+v created=%t err=%v", first, created, err)
	}
	// The other side opening it finds the same room.
	if err := f.s.Tx(ctx, func(tx *Tx) (err error) {
		second, createdAgain, err = tx.OpenAgentDM(f.worker2.ID, f.lead1.ID, f.worker2.ID)
		return err
	}); err != nil || createdAgain || second.ID != first.ID {
		t.Fatalf("second=%+v created=%t err=%v", second, createdAgain, err)
	}
	if first.ProjectID != AgentDMProjectID || first.Kind != "dm" || !IsAgentDM(first) {
		t.Fatalf("room=%+v", first)
	}
	var members int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND left_at IS NULL`, first.ID).Scan(&members); err != nil || members != 2 {
		t.Fatalf("agent members=%d err=%v", members, err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, _, err := tx.OpenAgentDM(f.lead1.ID, f.lead1.ID, f.lead1.ID)
		return err
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a chat with oneself: %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET revoked_at=? WHERE id=?`, timestamp(), f.worker1.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, _, err := tx.OpenAgentDM(f.lead1.ID, f.worker1.ID, f.lead1.ID)
		return err
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a revoked agent: %v", err)
	}
}

func TestAgentDMSystemProjectNeverListed(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	var system int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM project WHERE id=? AND system=1`, AgentDMProjectID).Scan(&system); err != nil || system != 1 {
		t.Fatalf("system project rows=%d err=%v", system, err)
	}
	for _, user := range []string{f.owner, f.alice, f.bob} {
		projects, err := f.s.ProjectsForUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		for _, project := range projects {
			if project.ID == AgentDMProjectID {
				t.Fatalf("%s sees the system project", user)
			}
		}
	}
	all, err := f.s.AllProjects(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range all {
		if project.ID == AgentDMProjectID {
			t.Fatal("the owner's project list names the system project")
		}
	}
}

func TestAgentDMAccessFollowsMembershipInAnAgentsProject(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.alice, f.bob, f.vera, f.owner)
	for _, user := range []string{f.alice, f.bob, f.vera, f.owner} {
		if err := f.s.UserRoomAccess(ctx, user, room.ID); err != nil {
			t.Fatalf("%s: %v", user, err)
		}
	}
	// Never joined: a project member who is not in the chat does not read it.
	if err := f.s.UserRoomAccess(ctx, f.carol, room.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("carol: %v", err)
	}
	writers := map[string]bool{f.alice: true, f.bob: true, f.vera: false, f.owner: true, f.carol: false}
	for user, want := range writers {
		got, err := f.s.AgentDMWriteAllowed(ctx, user, room.ID)
		if err != nil || got != want {
			t.Fatalf("write %s = %t (%v), want %t", user, got, err, want)
		}
	}
	// Losing every involved project takes the chat away without anyone editing it.
	if _, err := f.s.db.Exec(`DELETE FROM membership WHERE user_id=?`, f.vera); err != nil {
		t.Fatal(err)
	}
	if err := f.s.UserRoomAccess(ctx, f.vera, room.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("vera after removal: %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE user SET disabled_at=? WHERE id=?`, timestamp(), f.alice); err != nil {
		t.Fatal(err)
	}
	if err := f.s.UserRoomAccess(ctx, f.alice, room.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a disabled account: %v", err)
	}
	// Only the chats a person has joined and still has a footing in are listed.
	list, err := f.s.AgentDMsForUser(ctx, f.bob)
	if err != nil || len(list) != 1 || list[0].ID != room.ID {
		t.Fatalf("bob list=%+v err=%v", list, err)
	}
	list, err = f.s.AgentDMsForUser(ctx, f.carol)
	if err != nil || len(list) != 0 {
		t.Fatalf("carol list=%+v err=%v", list, err)
	}
}

func TestAgentDMEventsReachOnlyItsPeople(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.alice)
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: f.alice, Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	_, max, err := f.s.EventBounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roomEvents := func(user string) (all, others int) {
		events, err := f.s.EventsAfter(ctx, user, 0, max)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.RoomID == room.ID {
				all++
				if e.Kind != "room_created" {
					others++
				}
			}
		}
		return all, others
	}
	if all, _ := roomEvents(f.alice); all < 2 {
		t.Fatal("a member of the chat sees none of its events")
	}
	// bob has a footing in the peer's project, so he learns that the chat exists (to be offered it) and nothing of what happens in it;
	// carol has none, and learns nothing.
	if all, others := roomEvents(f.bob); all != 1 || others != 0 {
		t.Fatalf("bob sees %d events, %d beyond its creation", all, others)
	}
	if all, _ := roomEvents(f.carol); all != 0 {
		t.Fatalf("carol sees %d events of a chat she has no footing in", all)
	}
}

// A turn in a chat between agents runs under the agent's own project: its pause holds the note, the other agent's project's does not.
func TestAgentDMDispatchFollowsTheAgentsOwnProject(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.bob)
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: f.bob, Body: "@peer a question from the other project"}); err != nil {
		t.Fatal(err)
	}
	setGate := func(project, gate string) {
		t.Helper()
		if _, err := f.s.db.Exec(`UPDATE project SET gate=? WHERE id=?`, gate, project); err != nil {
			t.Fatal(err)
		}
	}
	setGate(f.p2.ID, "paused")
	if _, ready, err := f.s.PrepareDispatch(ctx, f.worker2.ID, ""); err != nil || ready {
		t.Fatalf("the agent's own project is paused: ready=%t err=%v", ready, err)
	}
	setGate(f.p2.ID, "open")
	setGate(f.p1.ID, "paused")
	delivery, ready, err := f.s.PrepareDispatch(ctx, f.worker2.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 1 || delivery.Notes[0].RoomID != room.ID {
		t.Fatalf("the peer's project is paused but not this agent's: delivery=%+v ready=%t err=%v", delivery, ready, err)
	}
}

func TestAgentDMTurnsCountAgainstTheAgentsProject(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	if _, err := f.s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,started_at) VALUES ('trn_dm',?,'ses_peer2',0,?,1,'normal','exited',?)`, f.worker2.ID, room.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	p2, err := f.s.UsageLimit(ctx, f.p2.ID)
	if err != nil || p2.DailyTurns != 1 {
		t.Fatalf("the agent's project: %+v err=%v", p2, err)
	}
	p1, err := f.s.UsageLimit(ctx, f.p1.ID)
	if err != nil || p1.DailyTurns != 0 {
		t.Fatalf("the other project: %+v err=%v", p1, err)
	}
}

func TestAgentDMVisibleToItsAgentsOnly(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	msg, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: f.lead1.ID, Body: "private between us"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.No == 0 {
		t.Fatal("the message carries no number")
	}
	// Its members, from either project, read it by number and from history.
	for _, agent := range []Agent{f.lead1, f.worker2} {
		got, err := f.s.AgentMessageByNo(ctx, agent.ID, agent.ProjectID, msg.No)
		if err != nil || !got.Available || got.Body != "private between us" {
			t.Fatalf("%s by number: %+v err=%v", agent.Slug, got, err)
		}
		page, err := f.s.AgentRoomHistory(ctx, agent.ID, agent.ProjectID, room.ID, 0, 10)
		if err != nil || len(page.Messages) != 1 {
			t.Fatalf("%s history: %+v err=%v", agent.Slug, page, err)
		}
		rooms, err := f.s.RoomsForAgent(ctx, agent.ID, agent.ProjectID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range rooms {
			if r.ID == room.ID {
				found = true
				// The room list says how many of its messages are from people or agents (not notices): an agent's list must not claim none.
				if r.UserMessageCount < 1 || r.UserMessageCount > r.MessageCount {
					t.Fatalf("%s: the chat has %d messages, %d not notices", agent.Slug, r.MessageCount, r.UserMessageCount)
				}
			}
		}
		if !found {
			t.Fatalf("%s does not list the chat", agent.Slug)
		}
	}
	// Anyone else in either project does not.
	for _, agent := range []Agent{f.worker1, f.lead2} {
		got, err := f.s.AgentMessageByNo(ctx, agent.ID, agent.ProjectID, msg.No)
		if err != nil || got.Available {
			t.Fatalf("%s by number: %+v err=%v", agent.Slug, got, err)
		}
		if _, err := f.s.AgentRoomHistory(ctx, agent.ID, agent.ProjectID, room.ID, 0, 10); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s history: %v", agent.Slug, err)
		}
	}
}

func (f dmFixture) answer(t *testing.T, agent Agent, roomID, raw string) AnswerReceipt {
	t.Helper()
	ctx := context.Background()
	// One turn at a time per agent: the earlier one is over.
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE agent_id=? AND exec='pending'`, agent.ID); err != nil {
		t.Fatal(err)
	}
	var session string
	if err := f.s.db.QueryRow(`SELECT id FROM session WHERE agent_id=?`, agent.ID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.RecordTurn(ctx, Turn{AgentID: agent.ID, SessionID: session, Generation: 0, RoomID: roomID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := applyCandidate(t, f.s, turn, agent.ID, 0, raw)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

// A lead woken in a chat with an agent of another project has to tell its own people what came of it, so that turn may also post in the
// rooms of the lead's own project. A final answer may also go into any chat the agent is a member of from a turn woken in any room; a chat it
// is not in, and another project's room, stay refused.
func TestAgentDMTurnMayReportIntoItsOwnProjectOnly(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	room := f.open(t, f.lead1, f.worker2)
	second := f.open(t, f.worker2, f.worker1)
	task2 := f.task2
	post := func(rooms ...string) string {
		items := ""
		for i, id := range rooms {
			if i > 0 {
				items += ","
			}
			items += `{"room":"` + id + `","body":"hello"}`
		}
		return `{"messages":[` + items + `]}`
	}
	for name, tc := range map[string]struct {
		from  Room
		agent Agent
		to    []string
		want  string
	}{
		"reply in the chat and report to its own project": {room, f.worker2, []string{room.ID, task2.ID}, "committed"},
		"only the chat":                       {room, f.worker2, []string{room.ID}, "committed"},
		"a lead reports to its own project":   {room, f.lead1, []string{f.task1.ID}, "committed"},
		"another chat the agent is in":        {room, f.worker2, []string{second.ID}, "committed"},
		"the other agent's project room":      {room, f.lead1, []string{task2.ID}, "held"},
		"a project room into a chat it is in": {task2, f.worker2, []string{room.ID}, "committed"},
		"a chat the agent is not in":          {f.task1, f.worker1, []string{room.ID}, "held"},
	} {
		if got := f.answer(t, tc.agent, tc.from.ID, post(tc.to...)); got.Result != tc.want {
			t.Errorf("%s: result=%q want %q (%+v)", name, got.Result, tc.want, got)
		}
	}
}

// Project memory is the agent's own project's: a chat with an agent of another project shows neither side's project facts to the other,
// and what is said in the chat never reaches recall or the memory feed.
func TestAgentDMKeepsProjectMemoryApart(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	for id, project := range map[string]string{"knw_one": f.p1.ID, "knw_two": f.p2.ID} {
		body := "fact of " + id
		if _, err := f.s.db.Exec(`INSERT INTO project_memory(id,project_id,body,rev,author_kind,author_id,created_at,updated_at) VALUES (?,?,?,1,'user','usr_alice',?,?)`, id, project, body, timestamp(), timestamp()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec(`UPDATE settings_project SET memory_enabled=1 WHERE project_id=?`, project); err != nil {
			t.Fatal(err)
		}
	}
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.bob)
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: f.bob, Body: "@peer the zebra codeword stays here"}); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := f.s.PrepareDispatch(ctx, f.worker2.ID, "")
	if err != nil || !ready {
		t.Fatalf("delivery=%+v ready=%t err=%v", delivery, ready, err)
	}
	snapshot := string(delivery.Context)
	if !strings.Contains(snapshot, "fact of knw_two") || strings.Contains(snapshot, "fact of knw_one") {
		t.Fatalf("the agent's memory snapshot is not its own project's: %s", snapshot)
	}
	hits, err := f.s.RecallKnowledge(ctx, f.worker2.ID, "zebra codeword", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("a chat message reached recall: %+v err=%v", hits, err)
	}
}

func TestAgentDMArchivedOnlyWhenEveryAgentProjectIs(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	f.join(t, room.ID, f.alice)
	archive := func(project string) {
		t.Helper()
		if _, err := f.s.db.Exec(`UPDATE project SET archived_at=? WHERE id=?`, timestamp(), project); err != nil {
			t.Fatal(err)
		}
	}
	archived := func(user string) bool {
		t.Helper()
		got, err := f.s.ArchivedScope(ctx, user, "room", room.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if archived(f.alice) {
		t.Fatal("a live chat reads as archived")
	}
	archive(f.p2.ID)
	if archived(f.alice) {
		t.Fatal("one live agent project is enough to keep the chat writable")
	}
	archive(f.p1.ID)
	if !archived(f.alice) {
		t.Fatal("every agent project is archived, the chat should be read-only")
	}
	if archived(f.carol) {
		t.Fatal("someone with no footing in the chat is told something about it")
	}
}

func TestDeletingAProjectTakesItsAgentsOutOfTheirChats(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	third := f.open(t, f.lead1, f.worker1)
	f.join(t, room.ID, f.alice)
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: f.worker2.ID, Body: "kept after the project goes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE project SET archived_at=? WHERE id=?`, timestamp(), f.p2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE project_id=?`, f.p2.ID); err != nil {
		t.Fatal(err)
	}
	ackProjectStops(t, f.s, f.p2.ID)
	if err := f.s.DeleteProject(ctx, f.p2.ID, f.p2.Name, f.owner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := f.s.RoomByID(ctx, room.ID)
	if err != nil || got.Lifecycle != "closed" {
		t.Fatalf("the chat after the delete: %+v err=%v", got, err)
	}
	var left, kept, notes int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND left_at IS NULL`, room.ID).Scan(&left); err != nil || left != 1 {
		t.Fatalf("agents still in the chat=%d err=%v", left, err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND body='kept after the project goes'`, room.ID).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("the chat's messages=%d err=%v", kept, err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system'`, room.ID).Scan(&notes); err != nil || notes == 0 {
		t.Fatalf("no note about the removal err=%v", err)
	}
	// A chat that never involved the project is untouched.
	untouched, err := f.s.RoomByID(ctx, third.ID)
	if err != nil || untouched.Lifecycle != "open" {
		t.Fatalf("an unrelated chat: %+v err=%v", untouched, err)
	}
	var violations int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations: %d %v", violations, err)
	}
}

// The reserved project for chats between agents must never turn up where projects are enumerated: a query over `project` either names one
// (by id, or through a membership or an agent) or filters system=0. This catches a new "every project" query that forgot the filter.
func TestEveryProjectEnumerationSkipsTheReservedProject(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	from := regexp.MustCompile(`(?:FROM|JOIN) project\b`)
	named := regexp.MustCompile(`system=0|WHERE id=\?|WHERE p\.id=\?|WHERE id IN|ON [a-z]+\.id=[a-z]+\.project_id|ON p\.id=\?|JOIN membership|ON [a-z]+\.project_id=p\.id|FROM project p JOIN machine m ON m\.id=\?`)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "migrations.go" {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, loc := range from.FindAllStringIndex(text, -1) {
			// The statement runs to the end of its string literal.
			end := len(text)
			for _, quote := range []string{"`", "\""} {
				if i := strings.Index(text[loc[1]:], quote); i >= 0 && loc[1]+i < end {
					end = loc[1] + i
				}
			}
			start := strings.LastIndexAny(text[:loc[0]], "`\"") + 1
			if !named.MatchString(text[start:end]) {
				line := strings.Count(text[:loc[0]], "\n") + 1
				t.Errorf("%s:%d: a query over project neither names one nor filters system=0:\n%s", name, line, strings.TrimSpace(text[start:end]))
			}
		}
	}
}

// An agent's question in a chat between agents is filed under the asking agent's project (the chat has none, so nobody could see it
// otherwise) and follows that project's roles: its owners and operators in the chat see and answer it, a viewer of it or anyone from the
// other project does not.
func TestAgentDMQuestionReachesTheAskingAgentsProject(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	ask := `{"messages":[{"room":"` + room.ID + `","body":"Which one?","ask":{"kind":"question","text":"Which one?","options":["A","B"],"free_text":true}}]}`
	attention := `{"messages":[{"room":"` + room.ID + `","body":"May I proceed?","needs_attention":true,"proposed_answers":["Yes","No"]}]}`
	if got := f.answer(t, f.worker2, room.ID, ask); got.Result == "committed" {
		t.Fatalf("a question with nobody in the chat committed: %+v", got)
	}
	f.join(t, room.ID, f.alice, f.vera) // owner of the other project, viewer of the asking agent's: neither can answer
	if got := f.answer(t, f.worker2, room.ID, ask); got.Result == "committed" {
		t.Fatalf("a question nobody in the chat could answer committed: %+v", got)
	}
	f.join(t, room.ID, f.bob) // operator of the asking agent's project
	for _, request := range []struct{ name, raw string }{{"question", ask}, {"needs attention", attention}} {
		if got := f.answer(t, f.worker2, room.ID, request.raw); got.Result != "committed" {
			t.Fatalf("%s someone in the chat can answer: %+v", request.name, got)
		}
	}
	// (The two answers refused above left delivery notices of their own; only the questions matter here.)
	items := func(user string) []InboxEntry {
		t.Helper()
		list, err := f.s.InboxForUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		questions := []InboxEntry{}
		for _, item := range list {
			if item.Kind == "question" {
				questions = append(questions, item)
			}
		}
		return questions
	}
	got := items(f.bob)
	if len(got) != 1 || got[0].Question != "May I proceed?" {
		t.Fatalf("bob sees %+v, want only the newest request for attention", got)
	}
	// The attention item is in a chat between agents, not a home room: the row comes back for the chat's controls, flagged, and no count has it.
	if !got[0].InboxHidden {
		t.Fatalf("the agent-chat attention item is not flagged inbox_hidden: %+v", got[0])
	}
	if open, err := f.s.openAttentionByRoom(ctx, f.bob); err != nil || open[room.ID] != 0 {
		t.Fatalf("open attention count=%v err=%v, want the hidden item left out", open, err)
	}
	if unseen, err := f.s.attentionByRoom(ctx, f.bob); err != nil || unseen[room.ID] != 0 {
		t.Fatalf("unseen attention count=%v err=%v, want the hidden item left out", unseen, err)
	}
	var earlierState string
	if err := f.s.db.QueryRow(`SELECT i.state FROM inbox_item i JOIN message m ON m.id=i.message_id WHERE m.room_id=? AND m.body='Which one?'`, room.ID).Scan(&earlierState); err != nil || earlierState != "superseded" {
		t.Fatalf("earlier question state=%q err=%v", earlierState, err)
	}
	for _, item := range got {
		if item.ProjectID != f.p2.ID || item.RoomID != room.ID || !item.CanDecide {
			t.Fatalf("item %+v is not the asking agent's project's, in the chat, for bob to decide", item)
		}
	}
	for _, user := range []string{f.alice, f.vera, f.carol, f.owner} {
		if n := len(items(user)); n != 0 {
			t.Fatalf("%s sees %d items of a project they do not run", user, n)
		}
	}
	for _, user := range []string{f.alice, f.vera} {
		if _, _, err := f.s.AnswerInbox(ctx, got[0].ID, user, "k-"+user, "A", int(got[0].Rev)); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s answered: %v", user, err)
		}
	}
	message, _, err := f.s.AnswerInbox(ctx, got[0].ID, f.bob, "k-bob", "A", int(got[0].Rev))
	if err != nil {
		t.Fatalf("bob's answer: %v", err)
	}
	if woken := f.notesFor(t, message.ID); !sameWakes(woken, map[string]string{"peer": "inbox_answer"}) {
		t.Fatalf("the answer woke %v, want the asking agent", woken)
	}
}

// The reserved project is not a project anyone can archive, rename, delete or join, by any route or by hand.
func TestAgentDMReservedProjectCannotBeChanged(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	for name, statement := range map[string]string{
		"rename":   `UPDATE project SET name='x' WHERE id='` + AgentDMProjectID + `'`,
		"archive":  `UPDATE project SET archived_at='2026-01-01T00:00:00.000Z' WHERE id='` + AgentDMProjectID + `'`,
		"delete":   `DELETE FROM project WHERE id='` + AgentDMProjectID + `'`,
		"a member": `INSERT INTO membership(project_id,user_id,role) VALUES ('` + AgentDMProjectID + `','` + f.alice + `','owner')`,
	} {
		if _, err := f.s.db.Exec(statement); err == nil {
			t.Errorf("%s of the reserved project was allowed", name)
		}
	}
	if _, _, err := f.s.ArchiveProject(ctx, AgentDMProjectID, f.owner); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("archive: %v", err)
	}
	if err := f.s.ReopenProject(ctx, AgentDMProjectID, f.owner); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("reopen: %v", err)
	}
	if err := f.s.DeleteProject(ctx, AgentDMProjectID, "Agent conversations", f.owner); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("delete: %v", err)
	}
	newName := "renamed"
	if _, err := f.s.UpdateProject(ctx, AgentDMProjectID, &newName, nil, f.owner); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("rename: %v", err)
	}
	// An ordinary project is unaffected.
	if _, err := f.s.UpdateProject(ctx, f.p1.ID, &newName, nil, f.owner); err != nil {
		t.Errorf("rename of an ordinary project: %v", err)
	}
}

// An approval is seen and answered by owners only, so one in a chat needs an owner of the asking agent's project in it; an operator
// alone is not enough (the agent would wait for an answer nobody could give).
func TestAgentDMApprovalNeedsAnOwnerOfTheAskingAgentsProject(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	approval := `{"messages":[{"room":"` + room.ID + `","body":"Ship it?","ask":{"kind":"approval","text":"Ship it?","options":["Approve","Deny"]}}]}`
	f.join(t, room.ID, f.bob) // operator of the asking agent's project
	if got := f.answer(t, f.worker2, room.ID, approval); got.Result == "committed" {
		t.Fatalf("an approval only an operator was there for committed: %+v", got)
	}
	if _, err := f.s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, f.p2.ID, f.carol); err != nil {
		t.Fatal(err)
	}
	f.join(t, room.ID, f.carol)
	if got := f.answer(t, f.worker2, room.ID, approval); got.Result != "committed" {
		t.Fatalf("an approval an owner can answer: %+v", got)
	}
	approvals := func(user string) []InboxEntry {
		t.Helper()
		list, err := f.s.InboxForUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		out := []InboxEntry{}
		for _, item := range list {
			if item.Kind == "approval" {
				out = append(out, item)
			}
		}
		return out
	}
	if got := approvals(f.bob); len(got) != 0 {
		t.Fatalf("an operator sees %d approvals", len(got))
	}
	mine := approvals(f.carol)
	if len(mine) != 1 || !mine[0].CanDecide {
		t.Fatalf("the owner's approvals: %+v", mine)
	}
	if _, _, err := f.s.AnswerInbox(ctx, mine[0].ID, f.bob, "k-bob", "Approve", int(mine[0].Rev)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an operator answered an approval: %v", err)
	}
	if _, _, err := f.s.AnswerInbox(ctx, mine[0].ID, f.carol, "k-carol", "Approve", int(mine[0].Rev)); err != nil {
		t.Fatalf("the owner's answer: %v", err)
	}
}
