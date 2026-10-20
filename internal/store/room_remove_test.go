package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRemoveRoomRefusesWhatItShouldNotRemove(t *testing.T) {
	t.Parallel()
	s, project, task, _, _ := wakeFixture(t)
	ctx := context.Background()
	if err := s.RemoveRoom(ctx, task.ID); !errors.Is(err, ErrRoomNotClosed) {
		t.Fatalf("an open room must be archived first, got %v", err)
	}
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: "home", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, home.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRoom(ctx, home.ID); !errors.Is(err, ErrHomeRoom) {
		t.Fatalf("a home room is never removed, got %v", err)
	}
	if err := s.RemoveRoom(ctx, "room_missing"); err == nil {
		t.Fatal("a missing room must be an error")
	}
	// A live worktree keeps the room.
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO room_worktree(room_id,base_ref,branch_name,relative_path,state,created_at,updated_at) VALUES (?,'main','b','p','ready','t','t')`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRoom(ctx, task.ID); !errors.Is(err, ErrRoomNotRemovable) {
		t.Fatalf("a room with a live worktree must stay, got %v", err)
	}
	if countRows(t, s, `SELECT COUNT(*) FROM room WHERE id=?`, task.ID) != 1 {
		t.Fatal("a refused removal deleted the room")
	}
}

func TestRemoveRoomDeletesItsContentAndKeepsReferencesElsewhere(t *testing.T) {
	t.Parallel()
	s, project, task, one, _ := wakeFixture(t)
	ctx := context.Background()
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "other", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	post := func(roomID, body string) Message {
		m, err := s.InsertMessage(ctx, Message{RoomID: roomID, AuthorKind: "user", AuthorID: "owner", Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := post(task.ID, "first")
	reply, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "user", AuthorID: "owner", Body: "reply", ReplyTo: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	keep := post(other.ID, "stays")
	file := filepath.Join(t.TempDir(), "blob.png")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO reaction(message_id,member_kind,member_id,emoji,created_at) VALUES (?,'user','owner','👍','t')`, []any{first.ID}},
		{`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_rm',?,?,?,'h','image/png',1,?,'owner','t')`, []any{project.ID, task.ID, first.ID, file}},
		{`INSERT INTO project_memory(id,project_id,body,author_kind,author_id,created_at,updated_at,source_message_id) VALUES ('mem_rm',?,'a fact','user','owner','t','t',?)`, []any{project.ID, first.ID}},
		{`INSERT INTO task_note(id,room_id,body,rev,author_kind,author_id,created_at,updated_at) VALUES ('tn_rm',?,'note',1,'user','owner','t','t')`, []any{task.ID}},
	} {
		if _, err := s.db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	events := countRows(t, s, `SELECT COUNT(*) FROM event WHERE room_id=?`, task.ID)
	if events == 0 {
		t.Log("no events recorded for the room in this fixture")
	}

	if countRows(t, s, `SELECT COUNT(*) FROM note WHERE room_id=?`, task.ID) == 0 {
		t.Fatal("the fixture should have queued notes for the agent")
	}
	if err := s.RemoveRoom(ctx, task.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, check := range []struct {
		name, query string
		args        []any
		want        int
	}{
		{"the room", `SELECT COUNT(*) FROM room WHERE id=?`, []any{task.ID}, 0},
		{"its messages", `SELECT COUNT(*) FROM message WHERE id IN (?,?)`, []any{first.ID, reply.ID}, 0},
		{"its reactions", `SELECT COUNT(*) FROM reaction WHERE message_id=?`, []any{first.ID}, 0},
		{"its attachment row", `SELECT COUNT(*) FROM attachment WHERE id='att_rm'`, nil, 0},
		{"its task notes", `SELECT COUNT(*) FROM task_note WHERE room_id=?`, []any{task.ID}, 0},
		{"its queued notes", `SELECT COUNT(*) FROM note WHERE room_id=?`, []any{task.ID}, 0},
		{"its members", `SELECT COUNT(*) FROM room_member WHERE room_id=?`, []any{task.ID}, 0},
		{"events still naming the room", `SELECT COUNT(*) FROM event WHERE room_id=?`, []any{task.ID}, 0},
		{"the other room", `SELECT COUNT(*) FROM room WHERE id=?`, []any{other.ID}, 1},
		{"the other room's message", `SELECT COUNT(*) FROM message WHERE id=?`, []any{keep.ID}, 1},
		{"the memory record", `SELECT COUNT(*) FROM project_memory WHERE id='mem_rm'`, nil, 1},
		{"the memory record's dangling source", `SELECT COUNT(*) FROM project_memory WHERE id='mem_rm' AND source_message_id IS NULL`, nil, 1},
		{"the agent", `SELECT COUNT(*) FROM agent WHERE id=?`, []any{one.ID}, 1},
	} {
		if got := countRows(t, s, check.query, check.args...); got != check.want {
			t.Errorf("%s: %d rows, want %d", check.name, got, check.want)
		}
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the attachment file must be removed, stat err = %v", err)
	}
	var violations int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Errorf("foreign key violations after removal: %d %v", violations, err)
	}
}

func TestRemoveRoomSucceedsEvenIfAnAttachmentFileCannotBeRemoved(t *testing.T) {
	t.Parallel()
	s, project, task, _, _ := wakeFixture(t)
	ctx := context.Background()
	msg, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "user", AuthorID: "owner", Body: "with a file"})
	if err != nil {
		t.Fatal(err)
	}
	// A non-empty directory cannot be removed with os.Remove: stands in for a file that is stuck.
	stuck := t.TempDir()
	if err := os.WriteFile(filepath.Join(stuck, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_stuck',?,?,?,'h','image/png',1,?,'owner','t')`, project.ID, task.ID, msg.ID, stuck); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRoom(ctx, task.ID); err != nil {
		t.Fatalf("the room is gone, so a leftover file must not fail the removal: %v", err)
	}
	if countRows(t, s, `SELECT COUNT(*) FROM room WHERE id=?`, task.ID) != 0 {
		t.Fatal("the room should be removed")
	}
}
