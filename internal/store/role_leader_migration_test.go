package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func leaderRoleDatabaseBefore(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "leader.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(context.Background(), db, 101); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLeaderRoleMigrationPreservesAssignmentsAndCustomRoles(t *testing.T) {
	db := leaderRoleDatabaseBefore(t)
	defer db.Close()
	statements := []string{
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_rename','rename','/tmp/rename','now')`,
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_rename','host','hash',1,1)`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_rename','prj_rename','mch_rename','/tmp/rename')`,
		`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_rename','prj_rename',0,'home','Home','owner','now')`,
		`INSERT INTO role(id,key,label,body,role_key,project_id) VALUES ('role_custom_leader','leader','Custom','keep custom body','leader',NULL)`,
		`INSERT INTO role(id,key,label,body,role_key,project_id) VALUES ('role_taken','leader-v1','Taken','taken body','leader-v1',NULL)`,
		`INSERT INTO role(id,key,label,body,role_key,project_id) VALUES ('role_scoped','leader@rename','Scoped','scoped body','leader','prj_rename')`,
		`INSERT INTO role(id,key,label,body,role_key,extends) VALUES ('role_child','child','Child','child body','child','leader')`,
		`INSERT INTO role(id,key,label,body,role_key,extends) VALUES ('role_child_lead','child-lead','Child lead','child body','child-lead','lead')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
		 VALUES ('agt_builtin','builtin','prj_rename','role_lead_default','fake','test','test','test','mch_rename','chk_rename','key','owner','now')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
		 VALUES ('agt_custom','custom','prj_rename','role_custom_leader','fake','test','test','test','mch_rename','chk_rename','key','owner','now')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
		 VALUES ('agt_scoped','scoped','prj_rename','role_scoped','fake','test','test','test','mch_rename','chk_rename','key','owner','now')`,
		`INSERT INTO project_role_spawn_default(project_id,role_id) VALUES ('prj_rename','role_lead_default')`,
		`INSERT INTO role_spawn_cursor(project_id,role_id) VALUES ('prj_rename','role_lead_default')`,
		`INSERT INTO role_spawn_warning(room_id,role_id,last_warned_at) VALUES ('room_rename','role_lead_default','now')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := migrateUpTo(context.Background(), db, 102); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ query, want string }{
		{`SELECT role_key FROM role WHERE id='role_leader_default'`, "leader"},
		{`SELECT label FROM role WHERE id='role_leader_default'`, "Leader"},
		{`SELECT role_id FROM agent WHERE id='agt_builtin'`, "role_leader_default"},
		{`SELECT role_id FROM agent WHERE id='agt_custom'`, "role_custom_leader"},
		{`SELECT role_id FROM agent WHERE id='agt_scoped'`, "role_scoped"},
		{`SELECT role_key FROM role WHERE id='role_custom_leader'`, "leader-v1-2"},
		{`SELECT label FROM role WHERE id='role_custom_leader'`, "Custom (v1)"},
		{`SELECT body FROM role WHERE id='role_custom_leader'`, "keep custom body"},
		{`SELECT role_key FROM role WHERE id='role_scoped'`, "leader"},
		{`SELECT extends FROM role WHERE id='role_child'`, "leader-v1-2"},
		{`SELECT extends FROM role WHERE id='role_child_lead'`, "leader"},
		{`SELECT role_id FROM project_role_spawn_default`, "role_leader_default"},
		{`SELECT role_id FROM role_spawn_cursor`, "role_leader_default"},
		{`SELECT role_id FROM role_spawn_warning`, "role_leader_default"},
	} {
		var got string
		if err := db.QueryRow(item.query).Scan(&got); err != nil || got != item.want {
			t.Errorf("%s: got %q, want %q, err %v", item.query, got, item.want, err)
		}
	}
	var legacy int
	if err := db.QueryRow(`SELECT COUNT(*) FROM role WHERE id='role_lead_default' OR role_key='lead' AND deleted_at IS NULL`).Scan(&legacy); err != nil || legacy != 0 {
		t.Errorf("old role remains: %d, %v", legacy, err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation after role migration")
	}
}

func TestLeaderRoleMigrationReportsProjectCollision(t *testing.T) {
	db := leaderRoleDatabaseBefore(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_scoped','scoped','/tmp/scoped','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO role(id,key,label,body,role_key,project_id) VALUES ('role_scoped','leader@scoped','Scoped','keep me','leader','prj_scoped')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := migrateLeaderRole(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || !strings.Contains(facts[0], "prj_scoped") || !strings.Contains(facts[0], "left unchanged") {
		t.Fatalf("missing collision report: %v", facts)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM role WHERE project_id='prj_scoped' AND role_key='leader'`).Scan(&id); err != nil || id != "role_scoped" {
		t.Fatalf("scoped role changed: %q, %v", id, err)
	}
}
