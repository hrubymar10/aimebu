package store

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/answer"
)

func TestExtractMessageRefs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		bodies []string
		skip   map[int64]bool
		max    int
		want   []int64
	}{
		{"plain", []string{"reply to #36226 please"}, nil, 5, []int64{36226}},
		{"order and dedupe", []string{"see #3, #1 and #3", "again #7"}, nil, 5, []int64{3, 1, 7}},
		{"punctuation around", []string{"(#42) and #43. #44, #45!"}, nil, 9, []int64{42, 43, 44, 45}},
		{"start of text and after newline", []string{"#10\n#11"}, nil, 9, []int64{10, 11}},
		{"not a word, URL fragment or heading", []string{"a#7 https://x.test/p#8 &#9; ## Title #0 #01 ##12"}, nil, 9, nil},
		{"not in code", []string{"`#5` and ```\n#6\n``` but #7"}, nil, 9, []int64{7}},
		{"skips what is delivered", []string{"#1 #2 #3"}, map[int64]bool{2: true}, 9, []int64{1, 3}},
		{"bounded", []string{"#1 #2 #3 #4 #5 #6 #7"}, nil, 5, []int64{1, 2, 3, 4, 5}},
		{"too long a number is not one", []string{"#12345678901234567890"}, nil, 5, nil},
	}
	for _, c := range cases {
		if got := extractMessageRefs(c.bodies, c.skip, c.max); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A message is only ever returned to an agent from a room of its own project; another project, a missing number and a private chat the
// agent is not in all answer the same way, with nothing about the message.
func TestAgentMessageByNoIsScopedToTheProject(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	ctx := context.Background()
	f.role(t, "role_w", f.project.ID, "worker", "Worker", "w", "multi")
	worker := f.agent(t, "wanda", "role_w", &f.home)
	child, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: f.home.ID, Kind: "task", Title: "roomB", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	inChild, err := f.s.InsertMessage(ctx, Message{RoomID: child.ID, AuthorKind: "system", AuthorID: "system", Body: "in the child room"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.s.CreateProject(ctx, Project{Name: "elsewhere", Folder: "/tmp/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	otherRoom, err := f.s.CreateRoom(ctx, Room{ProjectID: other.ID, Kind: "home", Title: "Elsewhere", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.s.InsertMessage(ctx, Message{RoomID: otherRoom.ID, AuthorKind: "system", AuthorID: "system", Body: "secret of another project"})
	if err != nil {
		t.Fatal(err)
	}
	// The worker is not in the child room, yet any room of its project is readable by number.
	got, err := f.s.AgentMessageByNo(ctx, worker.ID, f.project.ID, inChild.No)
	if err != nil || !got.Available || got.Body != "in the child room" || got.Room != "roomB" || got.RoomID != child.ID {
		t.Fatalf("a room of the same project: %+v %v", got, err)
	}
	for name, no := range map[string]int64{"another project": foreign.No, "no such number": 999999} {
		got, err := f.s.AgentMessageByNo(ctx, worker.ID, f.project.ID, no)
		if err != nil || !reflect.DeepEqual(got, AgentMessage{No: no}) {
			t.Fatalf("%s: %+v %v, want only the number", name, got, err)
		}
	}
	// A private chat is readable only by a member of it.
	dm, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "dm", DMKey: "users:a:b", Title: "dm", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	private, err := f.s.InsertMessage(ctx, Message{RoomID: dm.ID, AuthorKind: "system", AuthorID: "system", Body: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.AgentMessageByNo(ctx, worker.ID, f.project.ID, private.No); got.Available {
		t.Fatalf("a private chat the agent is not in was readable: %+v", got)
	}
	// The excerpt put into a delivery is bounded.
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	big, err := f.s.InsertMessage(ctx, Message{RoomID: child.ID, AuthorKind: "system", AuthorID: "system", Body: string(long)})
	if err != nil {
		t.Fatal(err)
	}
	var refs []AgentMessage
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		var err error
		refs, err = tx.referencedMessages(worker.ID, f.project.ID, []string{"see #" + strconv.FormatInt(big.No, 10) + " and #" + strconv.FormatInt(foreign.No, 10)}, map[int64]bool{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || !refs[0].Available || len([]rune(refs[0].Body)) > refExcerptRunes+1 || refs[1].Available || refs[1].Body != "" || refs[1].Room != "" {
		t.Fatalf("references in a delivery: %+v", refs)
	}
}

// The lead is in every room of its project: rooms made later, rooms that already exist when it is spawned, but not private chats.
func TestLeadJoinsEveryProjectRoom(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	ctx := context.Background()
	member := func(room Room, agent Agent) bool {
		var n int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, room.ID, agent.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	f.role(t, "role_w", f.project.ID, "worker", "Worker", "w", "multi")
	// A lead spawned into the home room joins the rooms that already exist (f.other).
	lead := f.agent(t, "tessa", "role_lead_default", &f.home)
	if !member(f.home, lead) || !member(f.other, lead) {
		t.Fatal("a lead spawned after the rooms exist did not join them all")
	}
	// A room made afterwards, by anyone, has the lead in it, once.
	child, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: f.home.ID, Kind: "task", Title: "roomB", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if !member(child, lead) {
		t.Fatal("a new child room did not get the lead")
	}
	// A worker is not made a member of anything by this.
	worker := f.agent(t, "wanda", "role_w", &f.home)
	if member(child, worker) {
		t.Fatal("a worker joined a room it was never put in")
	}
	// Private chats are not project rooms for this.
	dm, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "dm", DMKey: "users:x:y", Title: "dm", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if member(dm, lead) {
		t.Fatal("the lead joined a private chat")
	}
	// A closed room is left alone for a lead that arrives later.
	if _, err := f.s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	later := f.agent(t, "tom", "role_lead_default", nil)
	if err := f.s.AddMember(ctx, f.home.ID, "agent", later.ID); err != nil {
		t.Fatal(err)
	}
	if !member(f.other, later) || member(child, later) {
		t.Fatal("a later lead should join open rooms only")
	}
}

// A room closed before a lead arrived has no lead; reopening it puts the project's leads in (quietly, as automatic members).
func TestReopenedRoomGetsTheLead(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	ctx := context.Background()
	child, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: f.home.ID, Kind: "task", Title: "roomC", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE room SET lifecycle='closed',lifecycle_rev=lifecycle_rev+1 WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := f.s.RoomByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	lead := f.agent(t, "tessa", "role_lead_default", &f.home)
	count := func() (n, auto int) {
		if err := f.s.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(auto),0) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, child.ID, lead.ID).Scan(&n, &auto); err != nil {
			t.Fatal(err)
		}
		return
	}
	if n, _ := count(); n != 0 {
		t.Fatal("a closed room got the lead")
	}
	if _, err := f.s.ReopenRoom(ctx, child.ID, f.alice, closed.LifecycleRev); err != nil {
		t.Fatal(err)
	}
	if n, auto := count(); n != 1 || auto != 1 {
		t.Fatalf("reopened room: lead members=%d auto=%d, want 1 automatic member", n, auto)
	}
}

// Migration 72 brings the existing rooms up to date: a lead already in a project gets every open room it is not in.
func TestLeadBackfillMigration(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	lead := f.agent(t, "tessa", "role_lead_default", &f.home)
	f.role(t, "role_w", f.project.ID, "worker", "Worker", "w", "multi")
	worker := f.agent(t, "wanda", "role_w", &f.home)
	if _, err := f.s.db.Exec(`DELETE FROM room_member WHERE room_id=? AND member_id=?`, f.other.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	var backfill string
	for _, m := range migrations {
		if m.version == 72 {
			backfill = m.sql[strings.Index(m.sql, "INSERT INTO room_member"):] // the ALTER already ran with the migrations
		}
	}
	if backfill == "" {
		t.Fatal("migration 72 is missing")
	}
	if _, err := f.s.db.Exec(backfill); err != nil {
		t.Fatal(err)
	}
	var leadIn, workerIn int
	f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_id=? AND left_at IS NULL`, f.other.ID, lead.ID).Scan(&leadIn)
	f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_id=?`, f.other.ID, worker.ID).Scan(&workerIn)
	if leadIn != 1 || workerIn != 0 {
		t.Fatalf("after the backfill: lead in the other room %d (want 1), worker %d (want 0)", leadIn, workerIn)
	}
	if _, err := f.s.db.Exec(backfill); err != nil { // idempotent
		t.Fatal(err)
	}
}

// What an agent is shown includes a visual plan and appendix pages, so a #N written only there is a reference too.
func TestReferencesAreReadFromTheDeliveredBodyWithItsPlan(t *testing.T) {
	t.Parallel()
	body := deliveredNoteBody("see the plan", `[{"type":"markdown","title":"Plan","data":{"markdown":"Decided in #41"}}]`, `[{"title":"More","body":"and also #42"}]`)
	if got := extractMessageRefs([]string{body}, nil, 5); !reflect.DeepEqual(got, []int64{41, 42}) {
		t.Fatalf("references of a body with a plan and pages: %v in %q", got, body)
	}
}

// A recovered turn re-delivers its notes; the #N in them reach its context like any other turn's.
func TestRecoveredTurnContextCarriesReferencedMessages(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, true)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE settings_project SET auto_resume_after_clean_diagnostic=1 WHERE project_id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	earlier, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "the earlier decision"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE message SET body=? WHERE id=?`, "continue with #"+strconv.FormatInt(earlier.No, 10), f.note.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	diagnostic, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready {
		t.Fatalf("diagnostic ready=%t %v", ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, diagnostic.TurnID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"messages":[],"reconciliation":{"status":"clean","findings":[]}}`)
	parsed, err := answer.Parse(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, Generation: 0, AnswerRev: 0, Digest: "clean-digest", Candidate: raw, Parsed: &parsed}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, diagnostic.TurnID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	continuation, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || len(continuation.Notes) != 1 {
		t.Fatalf("continuation=%+v ready=%t %v", continuation, ready, err)
	}
	if !strings.Contains(string(continuation.Context), `"referenced_messages"`) || !strings.Contains(string(continuation.Context), "the earlier decision") {
		t.Fatalf("a recovered turn's context lacks the referenced message: %s", continuation.Context)
	}
}

// An automatic join is quiet and does not wake the lead for what people say to the room; joining on purpose makes it an ordinary member.
func TestLeadAutoJoinIsQuietAndNotWokenByUnaddressedMessages(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	ctx := context.Background()
	f.role(t, "role_w", f.project.ID, "worker", "Worker", "w", "multi")
	lead := f.agent(t, "tessa", "role_lead_default", &f.home) // explicit in home; automatic in f.other
	worker := f.agent(t, "wanda", "role_w", &f.other)
	if _, err := f.s.db.Exec(`UPDATE room_view_preference SET hidden=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO room_view_preference(user_id,room_id,hidden) VALUES (?,?,1)`, f.alice, f.other.ID); err != nil {
		t.Fatal(err)
	}
	var joinedBefore int
	f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='room_member_joined' AND room_id=? AND json_extract(payload_json,'$.member_id')=?`, f.other.ID, lead.ID).Scan(&joinedBefore)
	if joinedBefore != 0 {
		t.Fatalf("the automatic join emitted %d join events", joinedBefore)
	}
	child, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: f.home.ID, Kind: "task", Title: "roomB", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	var hidden int
	f.s.db.QueryRow(`SELECT hidden FROM room_view_preference WHERE user_id=? AND room_id=?`, f.alice, f.other.ID).Scan(&hidden)
	if hidden != 1 {
		t.Fatal("an automatic join un-hid a room for someone")
	}
	notes := func(agent Agent, room Room) int {
		var n int
		f.s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND room_id=?`, agent.ID, room.ID).Scan(&n)
		return n
	}
	say := func(room Room, body string) {
		if _, err := f.s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: f.alice, Body: body, TargetsJSON: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	// In f.other the worker is woken by an unaddressed human message, the lead (automatic there) is not.
	say(f.other, "anyone around?")
	if notes(worker, f.other) != 1 || notes(lead, f.other) != 0 {
		t.Fatalf("unaddressed message: worker %d notes, lead %d notes (want 1 and 0)", notes(worker, f.other), notes(lead, f.other))
	}
	// Addressed to the lead, it is woken, by name or by role.
	say(f.other, "@tessa please look")
	say(f.other, "@lead status?")
	if got := notes(lead, f.other); got != 2 {
		t.Fatalf("addressed messages woke the lead %d times, want 2", got)
	}
	// Where the lead was put on purpose it is woken as before.
	say(f.home, "hello home")
	if notes(lead, f.home) != 1 {
		t.Fatalf("an explicit lead member was not woken by an unaddressed message: %d", notes(lead, f.home))
	}
	// Joining on purpose a room it is in only automatically makes it an ordinary member.
	if err := f.s.AddMember(ctx, child.ID, "agent", lead.ID); err != nil {
		t.Fatal(err)
	}
	say(child, "anyone in roomB?")
	if notes(lead, child) != 1 {
		t.Fatalf("a lead joined on purpose was not woken: %d", notes(lead, child))
	}
	var auto int
	f.s.db.QueryRow(`SELECT auto FROM room_member WHERE room_id=? AND member_id=? AND left_at IS NULL`, f.other.ID, lead.ID).Scan(&auto)
	if auto != 1 {
		t.Fatalf("the lead's membership of the other room is not marked automatic: %d", auto)
	}
}
