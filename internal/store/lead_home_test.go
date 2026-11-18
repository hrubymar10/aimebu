package store

import (
	"context"
	"testing"
)

func leadAuto(t *testing.T, s *Store, roomID, agentID string) int {
	t.Helper()
	var auto int
	if err := s.db.QueryRow(`SELECT auto FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, roomID, agentID).Scan(&auto); err != nil {
		t.Fatal(err)
	}
	return auto
}

// The project lead is a full member of the home room, so an unaddressed human message there wakes it; in another project room it stays an
// auto member (it reads the room, but the unaddressed message is the worker's).
func TestLeadIsAFullMemberOfHomeAndWakesThere(t *testing.T) {
	t.Parallel()
	s, project, task, _, lead := wakeFixture(t)
	ctx := context.Background()
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: project.Name, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	// A lead already in the project when the home room is made, and one made lead later, both join home quietly but without the auto mark.
	if got := leadAuto(t, s, home.ID, lead.ID); got != 0 {
		t.Fatalf("lead's home membership auto=%d, want 0", got)
	}
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "other", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if got := leadAuto(t, s, other.ID, lead.ID); got != 1 {
		t.Fatalf("lead's task room membership auto=%d, want 1", got)
	}
	_ = task
	message, err := s.InsertMessage(ctx, Message{RoomID: home.ID, AuthorKind: "user", AuthorID: "owner", Body: "anyone there?"})
	if err != nil {
		t.Fatal(err)
	}
	if got := wakeReasons(t, s, message.ID); !sameWakes(got, map[string]string{"lead": "human_message"}) {
		t.Fatalf("an unaddressed message in home woke %v, want the lead", got)
	}
	message, err = s.InsertMessage(ctx, Message{RoomID: other.ID, AuthorKind: "user", AuthorID: "owner", Body: "anyone there?"})
	if err != nil {
		t.Fatal(err)
	}
	if got := wakeReasons(t, s, message.ID); len(got) != 0 {
		t.Fatalf("an unaddressed message in a task room woke %v, want nobody (the lead is an auto member)", got)
	}
}

// A lead made lead after the home room exists also joins it as a full member.
func TestLateLeadJoinsHomeAsFullMember(t *testing.T) {
	t.Parallel()
	s, project, _, one, _ := wakeFixture(t)
	ctx := context.Background()
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: project.Name, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET role_id='role_lead' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.joinLeadToProjectRooms(one.ID) }); err != nil {
		t.Fatal(err)
	}
	if got := leadAuto(t, s, home.ID, one.ID); got != 0 {
		t.Fatalf("late lead's home membership auto=%d, want 0", got)
	}
}

// The migration turns an existing active lead's auto home membership into an ordinary one and leaves everything else alone.
func TestMigrationMakesTheLeadAFullMemberOfHome(t *testing.T) {
	t.Parallel()
	s, project, task, one, lead := wakeFixture(t)
	ctx := context.Background()
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: project.Name, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, home.ID, "agent", one.ID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE room_member SET auto=1 WHERE member_kind='agent' AND member_id IN ('` + lead.ID + `','` + one.ID + `')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(leadMigrationSQL(t, 116)); err != nil {
		t.Fatal(err)
	}
	if got := leadAuto(t, s, home.ID, lead.ID); got != 0 {
		t.Fatalf("lead in home auto=%d, want 0", got)
	}
	if got := leadAuto(t, s, task.ID, lead.ID); got != 1 {
		t.Fatalf("lead in a task room auto=%d, want 1", got)
	}
	if got := leadAuto(t, s, home.ID, one.ID); got != 1 {
		t.Fatalf("a worker's auto mark in home changed to %d", got)
	}
}

func leadMigrationSQL(t *testing.T, version int) string {
	t.Helper()
	for _, m := range migrations {
		if m.version == version {
			return m.sql
		}
	}
	t.Fatalf("no migration %d", version)
	return ""
}

// A revoked lead, a lead that left home, and a closed home are the cases the migration must not turn into live wakes.
func TestMigrationLeavesRevokedAndLeftLeadsAlone(t *testing.T) {
	t.Parallel()
	s, project, _, _, lead := wakeFixture(t)
	ctx := context.Background()
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: project.Name, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room_member SET auto=1 WHERE room_id=? AND member_id=?`, home.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET revoked_at=? WHERE id=?`, timestamp(), lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(leadMigrationSQL(t, 116)); err != nil {
		t.Fatal(err)
	}
	if got := leadAuto(t, s, home.ID, lead.ID); got != 1 {
		t.Fatalf("revoked lead auto=%d, want 1", got)
	}
	if _, err := s.db.Exec(`UPDATE agent SET revoked_at=NULL WHERE id=?`, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room_member SET left_at=? WHERE room_id=? AND member_id=?`, timestamp(), home.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(leadMigrationSQL(t, 116)); err != nil {
		t.Fatal(err)
	}
	var auto int
	if err := s.db.QueryRow(`SELECT auto FROM room_member WHERE room_id=? AND member_id=?`, home.ID, lead.ID).Scan(&auto); err != nil || auto != 1 {
		t.Fatalf("lead that left auto=%d err=%v", auto, err)
	}
}
