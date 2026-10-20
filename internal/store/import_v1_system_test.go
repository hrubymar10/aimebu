package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

// v1's _system room is never imported: a mapping that sends it to a project (a saved draft, a hand-written map) is ignored, in the
// preview's dry run and in the commit.
func TestV1ImportAlwaysSkipsTheSystemRoom(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	at := "2026-10-01T12:00:00Z"
	room, _ := json.Marshal(map[string]any{"id": "_system", "members": []string{}, "created_at": at, "created_by": "system"})
	message, _ := json.Marshal(map[string]any{"id": 4, "room_id": "_system", "from": "system", "from_kind": "system", "body": "joined", "created_at": at})
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO rooms(id,data) VALUES (?,?)", []any{"_system", string(room)}},
		{"INSERT INTO messages(id,room_id,data) VALUES (?,?,?)", []any{4, "_system", string(message)}},
	} {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	ctx := context.Background()
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range preview.Rooms {
		if room.ID == "_system" && (!room.SuggestedSkip || room.SkipReason == "") {
			t.Errorf("the preview skips _system with a reason: %+v", room)
		}
	}
	// The mapping puts _system into a project, as a stale draft could.
	mapping.Rooms["_system"] = V1ImportRoom{Project: "repo", Target: V1TargetSub, Title: "general", Lifecycle: "open"}
	s := cursorStore(t)
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Skipped["rooms"] == 0 {
		t.Errorf("the dry run counts _system as skipped: %+v", dry.Skipped)
	}
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatalf("import: %v", err)
	}
	var rooms, messages int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room WHERE title='_system'`).Scan(&rooms); err != nil || rooms != 0 {
		t.Errorf("a _system room exists: %d %v", rooms, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE body='joined'`).Scan(&messages); err != nil || messages != 0 {
		t.Errorf("a _system message was imported: %d %v", messages, err)
	}
}
