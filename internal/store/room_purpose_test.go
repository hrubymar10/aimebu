package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// The migration gives every existing room a purpose: general for the flow's coordination rooms and a task room named general,
// research for a task room whose only agents besides leads are researchers, and task for everything else.
func TestRoomPurposeMigrationBackfills(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(SQLDriverName, sqliteDSN(filepath.Join(t.TempDir(), "purpose.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateUpTo(ctx, db, 125); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO project(id,name,folder,created_at) VALUES ('p1','one','/tmp/one','2026-01-01T00:00:00Z')`,
		`INSERT INTO machine(id,name,key_hash,approved) VALUES ('m1','host','hash',1)`,
	}
	for i, role := range []string{"role_leader_default", "role_researcher_default", "role_worker_default"} {
		id := []string{"lead", "res", "work"}[i]
		statements = append(statements,
			`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('c_`+id+`','p1','m1','/tmp/one/`+id+`')`,
			`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
			 VALUES ('`+id+`','`+id+`','p1','`+role+`','fake','test','test','fake','m1','c_`+id+`','hash','owner','2026-01-01T00:00:00Z')`)
	}
	rooms := []struct{ id, kind, title, key string }{
		{"home", "home", "one", ""}, {"plain", "task", "build it", ""}, {"named", "task", "general", ""}, {"routing", "task", "routing", "routing"},
		{"advice", "task", "advice", "advice"}, {"lobby", "lobby", "lobby", ""}, {"research", "task", "compare", ""}, {"mixed", "task", "mixed", ""},
		{"leadonly", "task", "leadonly", ""}, {"gone", "task", "gone", ""},
	}
	for _, room := range rooms {
		statements = append(statements, `INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at,team_flow_room_key) VALUES ('`+room.id+`','p1',0,'`+room.kind+`','`+room.title+`','owner','2026-01-01T00:00:00Z','`+room.key+`')`)
	}
	member := func(room, agent, left string) string {
		leftAt := "NULL"
		if left != "" {
			leftAt = "'" + left + "'"
		}
		return `INSERT INTO room_member(room_id,member_kind,member_id,joined_at,left_at) VALUES ('` + room + `','agent','` + agent + `','now',` + leftAt + `)`
	}
	statements = append(statements,
		member("research", "lead", ""), member("research", "res", ""),
		member("mixed", "res", ""), member("mixed", "work", ""),
		member("leadonly", "lead", ""),
		member("gone", "res", ""), member("gone", "work", "2026-01-02T00:00:00Z"))
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := migrateUpTo(ctx, db, 0); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"home": "task", "plain": "task", "named": "general", "routing": "general", "advice": "general", "lobby": "task",
		"research": "research", "mixed": "task", "leadonly": "task", "gone": "research"}
	for id, purpose := range want {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT purpose FROM room WHERE id=?`, id).Scan(&got); err != nil || got != purpose {
			t.Errorf("room %s purpose = %q, %v; want %q", id, got, err, purpose)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE room SET purpose='bogus' WHERE id='plain'`); err == nil {
		t.Error("a purpose outside task, research and general was accepted")
	}
}

func TestCreateRoomKeepsPurposeAndRefusesUnknown(t *testing.T) {
	s, _ := testStore(t, Options{})
	project, home, _ := fixture(t, s)
	ctx := context.Background()
	make := func(kind, purpose string) (Room, error) {
		return s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: home.ID, Kind: kind, Title: kind + purpose, CreatedBy: "usr_test", Purpose: purpose})
	}
	for want, ask := range map[string]string{"task": "", "research": "research", "general": "general"} {
		room, err := make("task", ask)
		if err != nil || room.Purpose != want {
			t.Fatalf("task room asked %q: %+v, %v; want purpose %q", ask, room, err, want)
		}
		stored, err := s.RoomByID(ctx, room.ID)
		if err != nil || stored.Purpose != want {
			t.Fatalf("stored purpose for %q = %q, %v", ask, stored.Purpose, err)
		}
	}
	if room, err := make("home", "research"); err != nil || room.Purpose != "task" {
		t.Fatalf("a room that is not a task room keeps the default purpose: %+v, %v", room, err)
	}
	if _, err := make("task", "urgent"); err == nil {
		t.Fatal("an unknown purpose was accepted")
	}
}

// The person's room list says whether an agent is working in the room, and how many unread messages name the person or reply to them.
func TestRoomListReportsWorkingAndAddressedUnread(t *testing.T) {
	s, room, ctx := notifyFixture(t)
	view := notifyView(t, s, ctx, room, "one")
	if view.Working || view.UnreadAddressed != 0 {
		t.Fatalf("a quiet room: %+v", view)
	}
	own, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "one", Body: "my question"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ body, reply string }{{"chatter", ""}, {"more chatter", ""}, {"@one look", ""}, {"an answer", own.ID}} {
		if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "two", Body: item.body, ReplyTo: item.reply}); err != nil {
			t.Fatal(err)
		}
	}
	if view = notifyView(t, s, ctx, room, "one"); view.UnreadCount != 4 || view.UnreadAddressed != 2 {
		t.Fatalf("unread=%d addressed=%d, want 4 and 2", view.UnreadCount, view.UnreadAddressed)
	}
	if other := notifyView(t, s, ctx, room, "two"); other.UnreadAddressed != 0 {
		t.Fatalf("someone else's addressed count = %d", other.UnreadAddressed)
	}
	state, err := s.MarkRoomRead(ctx, "one", room.ID, 0)
	if err != nil || state.UnreadAddressed != 0 || state.UnreadCount != 0 {
		t.Fatalf("after reading: %+v, %v", state, err)
	}
	for _, statement := range []string{
		`INSERT INTO machine(id,name,key_hash,approved) VALUES ('m1','host','hash',1)`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('c1',?,'m1','/tmp/test')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('a1','a1',?,'role_worker_default','fake','test','test','fake','m1','c1','hash','owner','now')`,
		`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('s1','a1',0,'fresh_with_history','now')`,
		`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,outcome,settle) VALUES ('t_done','a1','s1',0,'` + room.ID + `',1,'normal','exited','completed','settled')`,
	} {
		args := []any{}
		if len(statement) > 30 && (statement[:21] == "INSERT INTO checkout(" || statement[:18] == "INSERT INTO agent(") {
			args = append(args, room.ProjectID)
		}
		if _, err := s.db.Exec(statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if view = notifyView(t, s, ctx, room, "one"); view.Working {
		t.Fatal("a finished turn counts as working")
	}
	if _, err := s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec) VALUES ('t_run','a1','s1',0,?,2,'normal','running')`, room.ID); err != nil {
		t.Fatal(err)
	}
	if view = notifyView(t, s, ctx, room, "one"); !view.Working {
		t.Fatal("a running turn is not reported as working")
	}
}

// An agent's room list carries each room's purpose too, not the column default.
func TestRoomsForAgentReportsPurpose(t *testing.T) {
	s, _ := testStore(t, Options{})
	project, home, _ := fixture(t, s)
	ctx := context.Background()
	room, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: home.ID, Kind: "task", Title: "overview", CreatedBy: "usr_test", Purpose: "general"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "agent", "agent_example"); err != nil {
		t.Fatal(err)
	}
	rooms, err := s.RoomsForAgent(ctx, "agent_example", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rooms {
		if r.ID == room.ID {
			if r.Purpose != "general" {
				t.Fatalf("agent room list purpose = %q, want general", r.Purpose)
			}
			return
		}
	}
	t.Fatalf("room %s missing from %+v", room.ID, rooms)
}
