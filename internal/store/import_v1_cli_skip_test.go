package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

// The CLI used to refuse every skip choice ("require the web import wizard"),
// so the map the wizard suggests could not be imported from the command line.
func TestV1CLIMapAcceptsTheWizardsSkipChoices(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s := cursorStore(t)
	ctx := context.Background()
	if mapping.Wizard {
		t.Fatal("the CLI path must not be the wizard path")
	}
	mapping.Rooms["dm:alice:bot@repo"] = V1ImportRoom{Project: "repo", Skip: true}
	mapping.Humans["alice"] = "-"
	mapping.MemoryProjects["repo"] = "-"
	mapping.SkipProfiles = true
	mapping.SkipShared = true

	// Skipped people keep out of shared projects here too.
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_viewer','viewer','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	mapping.ProjectMembers = map[string][]string{"repo": {"viewer"}}
	if _, err := s.ImportV1(ctx, source, mapping, true); err == nil || !strings.Contains(err.Error(), "must be owner-only") {
		t.Fatalf("skipped person's room reached a shared project: %v", err)
	}
	mapping.ProjectMembers = nil

	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatalf("import with skips: %v", err)
	}
	if report.Skipped["rooms"] != 1 || report.Skipped["messages"] != 1 || report.Skipped["memory"] != 3 || report.Skipped["reactions"] != 1 {
		t.Fatalf("skip report: %+v", report)
	}
	var skippedRooms int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room WHERE kind='dm'`).Scan(&skippedRooms); err != nil || skippedRooms != 0 {
		t.Fatalf("skipped room was imported: %d %v", skippedRooms, err)
	}
	// Reruns of an unchanged map still work and change nothing.
	again, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || len(again.Imported) != 0 {
		t.Fatalf("rerun: %+v %v", again, err)
	}
}

// The map the wizard suggests is a JSON file the CLI reads; importing it
// as-is must skip _system without any editing.
func TestV1WizardSuggestedMapImportsThroughTheCLIPath(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	at := "2026-10-01T12:00:00Z"
	room, _ := json.Marshal(map[string]any{"id": "_system", "members": []string{"system"}, "created_at": at, "created_by": "system"})
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

	s := cursorStore(t)
	ctx := context.Background()
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	suggested := preview.Suggested
	if !suggested.Rooms["_system"].Skip {
		t.Fatalf("the wizard does not suggest skipping _system: %+v", suggested.Rooms)
	}
	for alias, project := range suggested.Projects {
		project.Folder = filepath.Join(t.TempDir(), alias)
		suggested.Projects[alias] = project
	}
	raw, err := json.Marshal(suggested)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "map.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Exactly what `aimebu import-v1 --map` does with the file.
	mapping, err := ReadV1ImportMap(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatalf("import of the wizard's suggested map: %v", err)
	}
	// _system and the DM are skipped by suggestion (a DM is imported only when the owner ticks it); the fixture's "work" is a
	// project of its own and is imported.
	if report.Skipped["rooms"] != 2 || report.Skipped["messages"] != 2 || len(report.DMOnlyAccess) != 0 {
		t.Fatalf("only _system and the DM should be skipped: %+v", report)
	}
	var imported int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room r JOIN legacy_room l ON l.room_id=r.id WHERE r.title='_system'`).Scan(&imported); err != nil || imported != 0 {
		t.Fatalf("_system room exists: %d %v", imported, err)
	}
}
