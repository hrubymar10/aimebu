package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/goccy/go-json"
)

func TestRoomViewPreferencesArePrivateAndUnhideOnAttentionOrJoin(t *testing.T) {
	s, _ := testStore(t, Options{})
	project, room, _ := fixture(t, s)
	ctx := context.Background()
	for _, user := range []string{"one", "two", "three"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, user, user); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, project.ID, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{"one", "two"} {
		if err := s.AddMember(ctx, room.ID, "user", user); err != nil {
			t.Fatal(err)
		}
	}
	trueValue := true
	if _, err := s.SetRoomViewPreference(ctx, "one", room.ID, &trueValue); err != nil {
		t.Fatal(err)
	}
	view := func(user string) Room {
		t.Helper()
		rooms, err := s.RoomsForUserProject(ctx, project.ID, user)
		if err != nil || len(rooms) != 1 {
			t.Fatalf("rooms for %s: %+v, %v", user, rooms, err)
		}
		return rooms[0]
	}
	if !view("one").Hidden || view("two").Hidden {
		t.Fatal("preferences leaked between users")
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "two", Body: "ordinary update"}); err != nil {
		t.Fatal(err)
	}
	if !view("one").Hidden {
		t.Fatal("ordinary message unhid room")
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "two", Body: "@one please review"}); err != nil {
		t.Fatal(err)
	}
	if view("one").Hidden {
		t.Fatal("addressed message did not unhide")
	}
	if _, err := s.SetRoomViewPreference(ctx, "one", room.ID, &trueValue); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "user", "three"); err != nil {
		t.Fatal(err)
	}
	if view("one").Hidden {
		t.Fatal("joining member did not unhide room")
	}
}

func migrationSQL(version int) string {
	for _, m := range migrations {
		if m.version == version {
			return m.sql
		}
	}
	return ""
}

func sidebarOrder(t *testing.T, s *Store, user string) (order []string, found bool) {
	t.Helper()
	var value string
	err := s.db.QueryRow(`SELECT value FROM user_preference WHERE user_id=? AND key='sidebar_project_order'`, user).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(value), &order); err != nil {
		t.Fatalf("stored order %q: %v", value, err)
	}
	return order, true
}

// The pin migrations in sequence: 59 turned room pins into project pins, 66 turns project pins into the start of a person's
// manual order and drops the table. The database starts at the pre-59 shape (a pinned flag on the room preference).
func TestPinMigrationsCarryEveryonesPinsIntoTheirManualOrder(t *testing.T) {
	s, _ := testStore(t, Options{})
	project, room, _ := fixture(t, s)
	ctx := context.Background()
	second, err := s.CreateProject(ctx, Project{Name: "second", Folder: "/tmp/second"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.CreateProject(ctx, Project{Name: "third", Folder: "/tmp/third"})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE IF EXISTS project_view_preference`,
		`ALTER TABLE room_view_preference ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('one','one','hash','{}','now')`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('two','two','hash','{}','now')`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('three','three','hash','{}','now')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// one pinned the first project's room, two only unhid it, three pinned a room of each of the other two projects (and has an order).
	secondRoomRow, err := s.CreateRoom(ctx, Room{ProjectID: second.ID, Kind: "home", Title: "Second home", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	thirdRoomRow, err := s.CreateRoom(ctx, Room{ProjectID: third.ID, Kind: "home", Title: "Third home", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	secondRoom, thirdRoom := secondRoomRow.ID, thirdRoomRow.ID
	for _, row := range []struct {
		user, room     string
		hidden, pinned int
	}{{"one", room.ID, 0, 1}, {"two", room.ID, 0, 0}, {"three", secondRoom, 0, 1}, {"three", thirdRoom, 0, 1}} {
		if _, err := s.db.Exec(`INSERT INTO room_view_preference(user_id,room_id,hidden,pinned) VALUES (?,?,?,?)`, row.user, row.room, row.hidden, row.pinned); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO user_preference(user_id,key,value,updated_at) VALUES ('three','sidebar_project_order','["mine"]','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(migrationSQL(59)); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room_view_preference`).Scan(&left); err != nil || left != 4 {
		t.Fatalf("room preferences after 59 (the unhide included): %d %v", left, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('room_view_preference') WHERE name='pinned'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the pinned column is still there: %d %v", left, err)
	}
	if _, err := s.db.Exec(migrationSQL(66)); err != nil {
		t.Fatal(err)
	}
	if order, found := sidebarOrder(t, s, "one"); !found || len(order) != 1 || order[0] != project.ID {
		t.Fatalf("one's order after 61: %v %t", order, found)
	}
	if _, found := sidebarOrder(t, s, "two"); found {
		t.Fatal("a person with no pin gets no order")
	}
	if order, _ := sidebarOrder(t, s, "three"); len(order) != 1 || order[0] != "mine" {
		t.Fatalf("an order the person already had must win: %v", order)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='project_view_preference'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("project_view_preference still exists: %d %v", left, err)
	}
}

// With two pinned projects the carried order lists them oldest first.
func TestPinMigrationOrdersCarriedPinsOldestFirst(t *testing.T) {
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	a, err := s.CreateProject(ctx, Project{Name: "a", Folder: "/tmp/a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateProject(ctx, Project{Name: "b", Folder: "/tmp/b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE project_view_preference (user_id TEXT NOT NULL, project_id TEXT NOT NULL, pinned INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (user_id, project_id))`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('one','one','hash','{}','now')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for id, at := range map[string]string{a.ID: "2026-01-01T00:00:00.000Z", b.ID: "2026-02-01T00:00:00.000Z"} {
		if _, err := s.db.Exec(`UPDATE project SET created_at=? WHERE id=?`, at, id); err != nil {
			t.Fatal(err)
		}
	}
	// Pinned in the opposite order of their creation: the carried order still follows creation.
	for _, id := range []string{b.ID, a.ID} {
		if _, err := s.db.Exec(`INSERT INTO project_view_preference(user_id,project_id,pinned) VALUES ('one',?,1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(migrationSQL(66)); err != nil {
		t.Fatal(err)
	}
	order, found := sidebarOrder(t, s, "one")
	if !found || len(order) != 2 || order[0] != a.ID || order[1] != b.ID {
		t.Fatalf("carried order = %v (found %t), want %v", order, found, []string{a.ID, b.ID})
	}
}
