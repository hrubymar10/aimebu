package store

import (
	"context"
	"testing"
)

// last_activity_at (the newest message's time, else the room's creation) orders the room list.
func TestRoomListReportsLastActivity(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	ctx := context.Background()
	rooms, err := f.s.RoomsForUserProject(ctx, f.project.ID, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Room{}
	for _, room := range rooms {
		byID[room.ID] = room
		if room.LastActivityAt == "" {
			t.Fatalf("room %s has no activity time", room.ID)
		}
	}
	before := byID[f.other.ID].LastActivityAt
	if _, err := f.s.db.Exec(`UPDATE message SET created_at='2999-01-01T00:00:00.000Z' WHERE room_id=?`, f.home.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: f.other.ID, AuthorKind: "user", AuthorID: f.alice, Body: "later"}); err != nil {
		t.Fatal(err)
	}
	rooms, err = f.s.RoomsForUserProject(ctx, f.project.ID, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range rooms {
		switch room.ID {
		case f.home.ID:
			if room.LastActivityAt != "2999-01-01T00:00:00.000Z" {
				t.Fatalf("home activity = %q", room.LastActivityAt)
			}
		case f.other.ID:
			if room.LastActivityAt <= before || room.LastMessageBody != "later" {
				t.Fatalf("other activity %q (was %q), body %q", room.LastActivityAt, before, room.LastMessageBody)
			}
		}
	}
}

// The sidebar's one-line preview is the newest message somebody wrote: a system notice (a join, a role change, "stopped") is not shown there, though it
// still counts as activity. A room with only notices has no preview.
func TestRoomPreviewSkipsSystemMessages(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	ctx := context.Background()
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: f.other.ID, AuthorKind: "user", AuthorID: f.alice, Body: "what somebody said"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: f.other.ID, AuthorKind: "system", AuthorID: "system", Body: "wren stopped"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM message WHERE room_id=? AND author_kind!='system'`, f.home.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: f.home.ID, AuthorKind: "system", AuthorID: "system", Body: "alice joined"}); err != nil {
		t.Fatal(err)
	}
	check := func(label string, list func() ([]Room, error)) {
		t.Helper()
		rooms, err := list()
		if err != nil {
			t.Fatal(err)
		}
		for _, room := range rooms {
			switch room.ID {
			case f.other.ID:
				if room.LastMessageBody != "what somebody said" {
					t.Fatalf("%s: a room whose newest message is a notice shows %q", label, room.LastMessageBody)
				}
			case f.home.ID:
				if room.LastMessageBody != "" || room.MessageCount == 0 {
					t.Fatalf("%s: a room with only notices shows %q (messages %d)", label, room.LastMessageBody, room.MessageCount)
				}
			}
		}
	}
	check("project list", func() ([]Room, error) { return f.s.RoomsForProject(ctx, f.project.ID) })
	check("user list", func() ([]Room, error) { return f.s.RoomsForUserProject(ctx, f.project.ID, f.alice) })
}
