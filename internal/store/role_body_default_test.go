package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOldRoleBodiesBecomeCurrentDefaults(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alpha4.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Alpha.4 ended at schema 126; replay that schema before upgrading once to the tip.
	if err := migrateUpTo(ctx, db, 126); err != nil {
		t.Fatal(err)
	}
	var alphaLead string
	if err := db.QueryRow(`SELECT body FROM role WHERE id='role_leader_default'`).Scan(&alphaLead); err != nil {
		t.Fatal(err)
	}
	if alphaLead != leadRoleBody {
		t.Fatal("the old schema fixture lacks its earlier shipped leader body")
	}
	var alphaDispatcher string
	if err := db.QueryRow(`SELECT body FROM role WHERE id='role_dispatcher_default'`).Scan(&alphaDispatcher); err != nil || alphaDispatcher != dispatcherRoleBody {
		t.Fatal("the old schema fixture lacks its earlier shipped dispatcher body")
	}
	// Unrelated edits raise revisions while both old shipped texts stay.
	if _, err := db.Exec(`UPDATE role SET rev=rev+4 WHERE id IN ('role_leader_default','role_dispatcher_default')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	roles, err := allLiveRoles(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		def, builtin := builtinRoles()[role.ID]
		if !builtin {
			continue
		}
		var stored string
		if err := db.QueryRow(`SELECT body FROM role WHERE id=?`, role.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != "" || role.Body != def.Body {
			t.Errorf("%s: stored body=%q, delivered body matches current=%t", role.ID, stored, role.Body == def.Body)
		}
		var archived string
		if err := db.QueryRow(`SELECT body FROM builtin_role_body_archive WHERE role_id=?`, role.ID).Scan(&archived); err != nil || archived == "" {
			t.Errorf("%s: old body was not archived: len=%d err=%v", role.ID, len(archived), err)
		}
	}
	var oldLead string
	if err := db.QueryRow(`SELECT body FROM builtin_role_body_archive WHERE role_id='role_leader_default'`).Scan(&oldLead); err != nil || oldLead != alphaLead {
		t.Fatalf("old leader body differs from the earlier shipped text: len=%d err=%v", len(oldLead), err)
	}
	var oldDispatcher string
	if err := db.QueryRow(`SELECT body FROM builtin_role_body_archive WHERE role_id='role_dispatcher_default'`).Scan(&oldDispatcher); err != nil || oldDispatcher != alphaDispatcher {
		t.Fatalf("old dispatcher body differs from the earlier shipped text: len=%d err=%v", len(oldDispatcher), err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	var archives int
	if err := db.QueryRow(`SELECT COUNT(*) FROM builtin_role_body_archive WHERE role_id='role_leader_default'`).Scan(&archives); err != nil || archives != 1 {
		t.Fatalf("replay changed archive count=%d err=%v", archives, err)
	}
	if _, err := db.Exec(`UPDATE role SET body='new owner edit' WHERE id='role_leader_default'`); err != nil {
		t.Fatal(err)
	}
	dropSupervisorInstances(t, &Store{db: db})
	if _, err := db.Exec(`DELETE FROM schema_version WHERE version>=139`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("forced replay: %v", err)
	}
	var kept string
	if err := db.QueryRow(`SELECT body FROM role WHERE id='role_leader_default'`).Scan(&kept); err != nil || kept != "new owner edit" {
		t.Fatalf("forced replay changed a later edit: %q %v", kept, err)
	}
}

func TestRoleBodyDefaultMigrationKeepsRecoverableEditsAndExtensions(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	defer s.Close()
	project, _, _ := fixture(t, s)
	roleReviewUsers(t, s, project.ID)
	custom, err := s.ManageRole(ctx, "usr_owner", RoleDefinition{Key: "custom_body", Label: "Custom", Body: "custom instructions", Cardinality: "multi"}, false)
	if err != nil {
		t.Fatal(err)
	}
	dropSupervisorInstances(t, s)
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version>=139`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE builtin_role_body_archive`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO role(id,key,role_key,label,emoji,body,cardinality)
		VALUES ('role_custom_default','custom-tail','custom-tail','Custom tail','','keep this body','multi')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE role SET body='owner leader edit',rev=rev+4 WHERE id='role_leader_default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO role_body_extension(role_id,project_id,body,rev,updated_at) VALUES
		('role_leader_default',NULL,'global extension',1,'now'),
		('role_leader_default',?,'project extension',1,'now')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := s.db.QueryRow(`SELECT body FROM builtin_role_body_archive WHERE role_id='role_leader_default'`).Scan(&body); err != nil || body != "owner leader edit" {
		t.Fatalf("archive body=%q err=%v", body, err)
	}
	if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, custom.ID).Scan(&body); err != nil || body != "custom instructions" {
		t.Fatalf("custom body=%q err=%v", body, err)
	}
	if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_custom_default'`).Scan(&body); err != nil || body != "keep this body" {
		t.Fatalf("custom suffix body=%q err=%v", body, err)
	}
	roles, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		if role.ID == "role_custom_default" && (role.Builtin || role.Body != "keep this body") {
			t.Fatalf("custom suffix role became built-in: %+v", role)
		}
	}
	var extensions int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM role_body_extension WHERE role_id='role_leader_default'`).Scan(&extensions); err != nil || extensions != 2 {
		t.Fatalf("extensions=%d err=%v", extensions, err)
	}
	if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_leader_default'`).Scan(&body); err != nil || body != "" {
		t.Fatalf("stored leader body=%q err=%v", body, err)
	}
}

func TestSavingAndResettingShippedBodyKeepsDefaultMode(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	defer s.Close()
	project, _, _ := fixture(t, s)
	roleReviewUsers(t, s, project.ID)
	role := globalRoles(t, s)["leader"]
	if role.Body != builtinRoles()[role.ID].Body {
		t.Fatal("list did not resolve the built-in body")
	}
	saved, err := s.ManageRole(ctx, "usr_owner", role, false)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, role.ID).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("saving shipped text stored %q: %v", stored, err)
	}
	saved.Body = "owner edit"
	saved, err = s.ManageRole(ctx, "usr_owner", saved, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetRole(ctx, "usr_owner", saved.ID, saved.Rev); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, role.ID).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("reset stored %q: %v", stored, err)
	}
}
