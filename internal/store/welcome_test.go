package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// welcomeMigration is the migration that seeds welcome_seen, found by what it does so a renumbering does not break the test.
func welcomeMigrationIndex(t *testing.T) int {
	t.Helper()
	for i, migration := range migrations {
		if strings.Contains(migration.sql, "welcome_seen") {
			return i
		}
	}
	t.Fatal("no migration seeds welcome_seen")
	return -1
}

func schemaBefore(t *testing.T, upTo int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "before.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:upTo] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
	}
	return db
}

// An install that already has people predates the welcome: it counts as seen. A new one has nobody when its migrations run, so the
// owner (created afterwards, or by a v1 import) still gets the welcome.
func TestWelcomeMigrationMarksOnlyExistingInstallsAsSeen(t *testing.T) {
	t.Parallel()
	index := welcomeMigrationIndex(t)
	seeded := func(db *sql.DB) string {
		var value string
		if err := db.QueryRow(`SELECT value FROM global_setting WHERE key='welcome_seen'`).Scan(&value); err == sql.ErrNoRows {
			return ""
		} else if err != nil {
			t.Fatal(err)
		}
		return value
	}

	fresh := schemaBefore(t, index)
	if _, err := fresh.Exec(migrations[index].sql); err != nil {
		t.Fatal(err)
	}
	if got := seeded(fresh); got != "" {
		t.Fatalf("a database with no people was marked: %q", got)
	}

	existing := schemaBefore(t, index)
	if _, err := existing.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_old','owner','x','{}','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := existing.Exec(migrations[index].sql); err != nil {
		t.Fatal(err)
	}
	if got := seeded(existing); got != "1" {
		t.Fatalf("an install that already had people was not marked seen: %q", got)
	}
	// Running it again changes nothing.
	if _, err := existing.Exec(migrations[index].sql); err != nil {
		t.Fatalf("rerun: %v", err)
	}
}
