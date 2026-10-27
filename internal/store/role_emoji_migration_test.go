package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func migratedFrom88(t *testing.T, prepare func(db *sql.DB)) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-88.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if err := migrateUpTo(context.Background(), db, 88); err != nil {
		t.Fatal(err)
	}
	prepare(db)
	if err := migrateUpTo(context.Background(), db, 0); err != nil {
		t.Fatal(err)
	}
	return db
}

func roleEmoji(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var value string
	if err := db.QueryRow(`SELECT emoji FROM role WHERE id=?`, id).Scan(&value); err != nil {
		t.Fatalf("role %s: %v", id, err)
	}
	return value
}

// Migration 89 changes the lead's emoji from the compass it shipped with to a crown, and only where the compass is still there.
func TestMigrationEightyNineChangesAnUntouchedLeadCompass(t *testing.T) {
	t.Parallel()
	db := migratedFrom88(t, func(db *sql.DB) {
		var before string
		if err := db.QueryRow(`SELECT emoji FROM role WHERE id='role_lead_default'`).Scan(&before); err != nil || before != "🧭" {
			t.Fatalf("before: the shipped lead has %q (%v), want the compass", before, err)
		}
		// A role that is not the lead and happens to carry the compass.
		if _, err := db.Exec(`UPDATE role SET emoji='🧭' WHERE id='role_worker_default'`); err != nil {
			t.Fatal(err)
		}
	})
	if got := roleEmoji(t, db, "role_lead_default"); got != "👑" {
		t.Fatalf("the shipped lead has %q after the migration, want the crown", got)
	}
	if got := roleEmoji(t, db, "role_worker_default"); got != "🧭" {
		t.Fatalf("a role that is not the lead changed to %q", got)
	}
	if got := roleEmoji(t, db, "role_reviewer_default"); got != "🔎" {
		t.Fatalf("the reviewer's emoji changed to %q", got)
	}
}

func TestMigrationEightyNineKeepsAnEmojiTheOwnerChose(t *testing.T) {
	t.Parallel()
	db := migratedFrom88(t, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE role SET emoji='🎯' WHERE id='role_lead_default'`); err != nil {
			t.Fatal(err)
		}
	})
	if got := roleEmoji(t, db, "role_lead_default"); got != "🎯" {
		t.Fatalf("an emoji the owner chose changed to %q", got)
	}
}

// The shipped lead role (what "reset to default" restores) is the crown too.
func TestBuiltinLeadRoleHasACrown(t *testing.T) {
	t.Parallel()
	if got := builtinRoles()["role_lead_default"].Emoji; got != "👑" {
		t.Fatalf("builtin lead emoji %q", got)
	}
}
