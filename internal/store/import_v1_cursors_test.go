package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
)

// v1CursorFixture is the shared v1 fixture with alice's read cursors set:
// message 1 in "work" (so one of its two messages is read) and a cursor far
// past the only message of the DM room.
func v1CursorFixture(t *testing.T, cursors map[string]int64) (string, V1ImportMap) {
	t.Helper()
	root, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", fixtureDSN(filepath.Join(root, "server", "aimebu.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := json.Marshal(map[string]any{"id": "alice", "kind": "human", "name": "alice", "read_cursors": cursors})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE agents SET data=? WHERE id='alice'", string(raw)); err != nil {
		t.Fatal(err)
	}
	return root, mapping
}

func cursorStore(t *testing.T) *Store {
	t.Helper()
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	return s
}

// importedSeq returns the v2 seq of v1 message number n.
func importedSeq(t *testing.T, s *Store, n string) int64 {
	t.Helper()
	var seq int64
	if err := s.db.QueryRow(`SELECT m.seq FROM legacy_import_map lm JOIN message m ON m.id=lm.new_id WHERE lm.entity_kind='message' AND lm.old_id=?`, n).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func readCursor(t *testing.T, s *Store, roomTitle string) int64 {
	t.Helper()
	var seq int64
	err := s.db.QueryRow(`SELECT COALESCE((SELECT c.last_read_seq FROM room_read_cursor c WHERE c.user_id='usr_owner' AND c.room_id=r.id),-1)
		FROM room r JOIN legacy_room l ON l.room_id=r.id WHERE r.title=? OR r.id=?`, roomTitle, roomTitle).Scan(&seq)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestV1ImportMapsHumanReadCursorsToSeq(t *testing.T) {
	t.Parallel()
	source, mapping := v1CursorFixture(t, map[string]int64{"work": 1, "dm:alice:bot@repo": 99})
	s := cursorStore(t)
	ctx := context.Background()

	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil || preview.Counts["read_cursors"] != 2 {
		t.Fatalf("preview read_cursors=%d err=%v", preview.Counts["read_cursors"], err)
	}
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil || dry.Imported["read_cursors"] != 2 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room_read_cursor`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry run wrote cursors: %d %v", count, err)
	}

	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["read_cursors"] != 2 || report.Imported["read_cursor_baselines"] != 0 {
		t.Fatalf("import: %+v %v", report, err)
	}
	// work: message 1 read, message 2 still unread.
	if got, want := readCursor(t, s, "work"), importedSeq(t, s, "1"); got != want {
		t.Fatalf("work cursor=%d want seq of v1 message 1 (%d)", got, want)
	}
	// dm: the cursor (99) is past its only message (3), so it clamps to it.
	var dmID string
	if err := s.db.QueryRow(`SELECT id FROM room WHERE kind='dm'`).Scan(&dmID); err != nil {
		t.Fatal(err)
	}
	if got, want := readCursor(t, s, dmID), importedSeq(t, s, "3"); got != want {
		t.Fatalf("dm cursor=%d want clamp to last imported message (%d)", got, want)
	}
}

func TestV1ImportReadCursorsNeverMoveBackwards(t *testing.T) {
	t.Parallel()
	source, mapping := v1CursorFixture(t, map[string]int64{"work": 1})
	s := cursorStore(t)
	ctx := context.Background()
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatal(err)
	}
	// The user reads on in v2, then the same backup is imported again.
	last := importedSeq(t, s, "2")
	if _, err := s.db.Exec(`UPDATE room_read_cursor SET last_read_seq=? WHERE user_id='usr_owner'`, last); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["read_cursors"] != 0 || report.AlreadyPresent["read_cursors"] != 1 {
		t.Fatalf("rerun: %+v %v", report, err)
	}
	if got := readCursor(t, s, "work"); got != last {
		t.Fatalf("cursor moved backwards to %d, want %d", got, last)
	}
}

func TestV1ImportSkipsUnusableReadCursors(t *testing.T) {
	t.Parallel()
	// 0 is "nothing read"; 2 is a message of another room, so the DM has no
	// imported message at or below it; the skipped room imports nothing.
	source, mapping := v1CursorFixture(t, map[string]int64{"work": 0, "dm:alice:bot@repo": 2})
	s := cursorStore(t)
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Imported["read_cursors"] != 0 || report.Skipped["read_cursors"] != 1 {
		t.Fatalf("report: %+v %v", report, err)
	}
	// Only work is baselined (it has no usable v1 cursor and is read in full);
	// the DM had a real v1 cursor that maps to nothing, so it stays unread.
	if got, want := readCursor(t, s, "work"), importedSeq(t, s, "2"); got != want {
		t.Fatalf("work baseline=%d want newest (%d)", got, want)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room_read_cursor`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("cursor rows=%d (want only the work baseline) err=%v", count, err)
	}

	// Skipping a room needs the wizard; its cursor must not be imported.
	source, mapping = v1CursorFixture(t, map[string]int64{"dm:alice:bot@repo": 3})
	mapping.Wizard = true
	mapping.Rooms["dm:alice:bot@repo"] = V1ImportRoom{Project: "repo", Skip: true}
	mapping.MemoryProjects["repo"] = "-"
	mapping.SkipProfiles = true
	mapping.SkipShared = true
	s = cursorStore(t)
	report, err = s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Imported["read_cursors"] != 0 || report.Skipped["read_cursors"] != 1 {
		t.Fatalf("skipped room: %+v %v", report, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room_read_cursor c JOIN room r ON r.id=c.room_id WHERE r.kind='dm'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("skipped room wrote cursors: %d %v", count, err)
	}
}

func TestV1ImportBaselinesReadersWithoutAV1Cursor(t *testing.T) {
	t.Parallel()
	// No read cursors in the backup at all: imported history is not new to
	// anyone, so every reader starts at the newest imported message.
	source, mapping := v1CursorFixture(t, nil)
	s := cursorStore(t)
	ctx := context.Background()
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["read_cursors"] != 0 || report.Imported["read_cursor_baselines"] != 2 {
		t.Fatalf("import: %+v %v", report, err)
	}
	var dmID string
	if err := s.db.QueryRow(`SELECT id FROM room WHERE kind='dm'`).Scan(&dmID); err != nil {
		t.Fatal(err)
	}
	if got, want := readCursor(t, s, "work"), importedSeq(t, s, "2"); got != want {
		t.Fatalf("work baseline=%d want %d", got, want)
	}
	if got, want := readCursor(t, s, dmID), importedSeq(t, s, "3"); got != want {
		t.Fatalf("dm baseline=%d want %d", got, want)
	}
	// A rerun leaves existing cursors alone.
	report, err = s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["read_cursor_baselines"] != 0 {
		t.Fatalf("rerun: %+v %v", report, err)
	}
}

// A room that ends with v1's "x pruned after stale timeout" has its read position on that line, which is not imported. The position
// falls back to the nearest earlier imported message, so the room is not shown as entirely unread.
func TestV1ImportReadCursorOnAPresenceLineFallsBackToTheMessageBefore(t *testing.T) {
	t.Parallel()
	source, mapping := v1CursorFixture(t, map[string]int64{"work": 11})
	db, err := sql.Open("sqlite", fixtureDSN(filepath.Join(source, "server", "aimebu.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	for id, body := range map[int]string{10: "bot@repo left", 11: "alice pruned after stale timeout"} {
		raw, _ := json.Marshal(map[string]any{"id": id, "room_id": "work", "from": "_system", "from_kind": "system", "body": body, "created_at": "2026-10-02T09:00:00Z"})
		if _, err := db.Exec("INSERT INTO messages(id,room_id,data) VALUES (?,?,?)", id, "work", string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s := cursorStore(t)
	ctx := context.Background()
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["read_cursors"] != 1 || report.Skipped["read_cursors"] != 0 {
		t.Fatalf("import: %+v %v", report, err)
	}
	if got, want := readCursor(t, s, "work"), importedSeq(t, s, "2"); got != want {
		t.Fatalf("cursor on a presence line is %d, want the seq of the message before it (%d)", got, want)
	}
	// And the room is not left without a position when it is imported again.
	if rerun, err := s.ImportV1(ctx, source, mapping, false); err != nil || rerun.Skipped["read_cursors"] != 0 {
		t.Fatalf("rerun: %+v %v", rerun, err)
	}
}
