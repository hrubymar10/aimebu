package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestRoleDefaultFingerprintMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "roles.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrateUpTo(ctx, db, roleDefaultBaseMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE role SET body='saved edit' WHERE id='role_worker_default'`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	if err := db.QueryRow(`SELECT default_body_fingerprint FROM role WHERE id='role_worker_default'`).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if fingerprint != "unknown" {
		t.Fatalf("fingerprint=%q", fingerprint)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
}

func TestRoleDefaultFingerprintActions(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	defer s.Close()
	project, _, _ := fixture(t, s)
	roleReviewUsers(t, s, project.ID)
	role := globalRoles(t, s)["worker"]
	role.Body = "owner instructions"
	saved, err := s.ManageRole(ctx, "usr_owner", role, false)
	if err != nil {
		t.Fatal(err)
	}
	var fp string
	if err := s.db.QueryRow(`SELECT default_body_fingerprint FROM role WHERE id=?`, role.ID).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	if fp != roleDefaultFingerprint(role.Key, builtinRoles()[role.ID].Body) {
		t.Fatal("save did not fingerprint current default")
	}
	var base string
	if err := s.db.QueryRow(`SELECT default_body_base FROM role WHERE id=?`, role.ID).Scan(&base); err != nil || base != builtinRoles()[role.ID].Body {
		t.Fatalf("saved base=%q err=%v", base, err)
	}
	if _, err := s.db.Exec(`UPDATE role SET default_body_fingerprint='unknown',default_body_base=NULL WHERE id=?`, role.ID); err != nil {
		t.Fatal(err)
	}
	if unknown := globalRoles(t, s)["worker"]; !unknown.DefaultBodyChanged || unknown.DefaultBodyBase != "" {
		t.Fatal("an edit with an unknown base did not show the marker")
	}
	if _, err := s.db.Exec(`UPDATE role SET default_body_fingerprint='prior' WHERE id=?`, role.ID); err != nil {
		t.Fatal(err)
	}
	role = globalRoles(t, s)["worker"]
	if !role.DefaultBodyChanged || role.DefaultBody != builtinRoles()[role.ID].Body {
		t.Fatal("changed default not exposed")
	}
	role.Label = "Worker renamed"
	saved, err = s.ManageRole(ctx, "usr_owner", role, false)
	if err != nil {
		t.Fatal(err)
	}
	if !globalRoles(t, s)["worker"].DefaultBodyChanged {
		t.Fatal("metadata save acknowledged changed default")
	}
	if _, err := s.KeepRoleDefault(ctx, "usr_owner", role.ID, saved.Rev+1); err != ErrStaleRev {
		t.Fatalf("stale keep: %v", err)
	}
	next, err := s.KeepRoleDefault(ctx, "usr_owner", role.ID, saved.Rev)
	if err != nil {
		t.Fatal(err)
	}
	if globalRoles(t, s)["worker"].DefaultBodyChanged {
		t.Fatal("keep did not clear marker")
	}
	if err := s.db.QueryRow(`SELECT default_body_base FROM role WHERE id=?`, role.ID).Scan(&base); err != nil || base != builtinRoles()[role.ID].Body {
		t.Fatalf("acknowledged base=%q err=%v", base, err)
	}
	if _, err := s.db.Exec(`UPDATE role SET default_body_fingerprint='prior' WHERE id=?`, role.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseRoleDefault(ctx, "usr_owner", role.ID, next); err != nil {
		t.Fatal(err)
	}
	role = globalRoles(t, s)["worker"]
	if role.DefaultBodyChanged || role.Body != builtinRoles()[role.ID].Body || role.Label != "Worker renamed" {
		t.Fatal("use default did not preserve metadata")
	}
	var body string
	if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, role.ID).Scan(&body); err != nil || body != "" {
		t.Fatalf("stored body=%q err=%v", body, err)
	}
}
