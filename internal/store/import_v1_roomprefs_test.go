package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
)

func v1RoomPrefsFixture(t *testing.T) (string, V1ImportMap) {
	t.Helper()
	root, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", fixtureDSN(filepath.Join(root, "server", "aimebu.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, human := range []string{"bob", "carol"} {
		raw, _ := json.Marshal(map[string]any{"id": human, "kind": "human", "name": human})
		if _, err := db.Exec(`INSERT INTO agents(id,data) VALUES (?,?)`, human, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`CREATE TABLE room_prefs (agent_id TEXT NOT NULL, room_id TEXT NOT NULL, hidden INTEGER NOT NULL DEFAULT 0, pinned INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (agent_id, room_id))`,
		`INSERT INTO room_prefs VALUES ('alice','work',1,0)`,
		`INSERT INTO room_prefs VALUES ('alice','dm:alice:bot@repo',0,1)`,
		`INSERT INTO room_prefs VALUES ('bob','work',0,1)`,
		`INSERT INTO room_prefs VALUES ('alice','gone-room',1,0)`,
		`INSERT INTO room_prefs VALUES ('carol','work',1,1)`,
		`INSERT INTO room_prefs VALUES ('bot@repo','work',1,1)`,
		`INSERT INTO room_prefs VALUES ('bob','dm:alice:bot@repo',0,0)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	mapping.Humans["bob"] = "owner"
	// carol has no mapping, so her row has no destination user and is skipped.
	return root, mapping
}

// roomPref reports the owner's hidden flag for a room.
func roomPref(t *testing.T, s *Store, roomOrID string) (hidden, found bool) {
	t.Helper()
	var roomID string
	if err := s.db.QueryRow(`SELECT id FROM room WHERE title=? OR id=? LIMIT 1`, roomOrID, roomOrID).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	err := s.db.QueryRow(`SELECT hidden FROM room_view_preference WHERE user_id='usr_owner' AND room_id=?`, roomID).Scan(&hidden)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return hidden, err == nil
}

// ownerOrder is the owner's stored manual project order (nil, false when they have none).
func ownerOrder(t *testing.T, s *Store) ([]string, bool) { return sidebarOrder(t, s, "usr_owner") }

func TestV1ImportCarriesHiddenRoomsAndPinsAsAProjectOrder(t *testing.T) {
	t.Parallel()
	source, mapping := v1RoomPrefsFixture(t)
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	// alice work, alice dm, alice gone-room, bob work and carol work; the agent's row and the
	// all-false row carry nothing.
	if err != nil || preview.Counts["room_prefs"] != 5 {
		t.Fatalf("preview count: %d %v", preview.Counts["room_prefs"], err)
	}
	if _, listed := preview.NotCarried["room_prefs"]; listed {
		t.Fatalf("room_prefs is carried and must not be listed as left behind: %v", preview.NotCarried)
	}
	s := cursorStore(t)
	ctx := context.Background()
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil || dry.Imported["room_prefs"] != 1 || dry.Skipped["room_prefs"] != 3 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM room_view_preference)+(SELECT COUNT(*) FROM user_preference WHERE key='sidebar_project_order')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry run wrote preferences: %d %v", count, err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["room_prefs"] != 1 || report.Skipped["room_prefs"] != 3 || report.AlreadyPresent["room_prefs"] != 0 {
		t.Fatalf("report: %+v %v", report, err)
	}
	// Alice hid "work" and bob pinned it; both map to one login, so the flags merge. The pin puts the room's project first in
	// the owner's manual order (v2 has no pins).
	if hidden, found := roomPref(t, s, "work"); !found || !hidden {
		t.Fatalf("work: hidden=%t found=%t", hidden, found)
	}
	var workProject string
	if err := s.db.QueryRow(`SELECT project_id FROM room WHERE title='work'`).Scan(&workProject); err != nil {
		t.Fatal(err)
	}
	if order, found := ownerOrder(t, s); !found || len(order) != 1 || order[0] != workProject {
		t.Fatalf("the owner's order = %v (found %t), want [%s]", order, found, workProject)
	}
	// A pinned DM has no project in the tree: it is skipped, not turned into something else.
	if hidden, found := roomPref(t, s, "bot"); found && hidden {
		t.Fatalf("a pinned DM must not be hidden: found=%t", found)
	}
	// A choice made in v2 since the import survives a rerun: an unhide, and an order of the person's own.
	if _, err := s.db.Exec(`UPDATE room_view_preference SET hidden=0 WHERE room_id IN (SELECT id FROM room WHERE title='work')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE user_preference SET value='["mine"]' WHERE user_id='usr_owner' AND key='sidebar_project_order'`); err != nil {
		t.Fatal(err)
	}
	rerun, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || rerun.Imported["room_prefs"] != 0 || rerun.AlreadyPresent["room_prefs"] != 1 {
		t.Fatalf("rerun: %+v %v", rerun, err)
	}
	if hidden, found := roomPref(t, s, "work"); !found || hidden {
		t.Fatalf("rerun overwrote the unhide: hidden=%t", hidden)
	}
	if order, _ := ownerOrder(t, s); len(order) != 1 || order[0] != "mine" {
		t.Fatalf("rerun overwrote the person's own order: %v", order)
	}
}

// A person who already has a manual order keeps it: their pinned v1 rooms add nothing.
func TestV1ImportLeavesAnExistingProjectOrderAlone(t *testing.T) {
	t.Parallel()
	source, mapping := v1RoomPrefsFixture(t)
	s := cursorStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user_preference(user_id,key,value,updated_at) VALUES ('usr_owner','sidebar_project_order','["mine"]','now')`); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["room_prefs"] != 1 {
		// The hidden flag of "work" is still imported; the pin is what stays out.
		t.Fatalf("report: %+v %v", report, err)
	}
	if order, _ := ownerOrder(t, s); len(order) != 1 || order[0] != "mine" {
		t.Fatalf("the person's own order was replaced: %v", order)
	}
}
