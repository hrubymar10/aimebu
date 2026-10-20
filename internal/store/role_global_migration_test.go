package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Migration 64 makes roles global only. A live project role becomes global when no live global role has its key (the first by id when
// several projects define it); any other live project role is dropped and its agents move to the global role with that key; a deleted
// project role stays as deleted history.
func TestMigrationSixtyFourMakesRolesGlobal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "from-63.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version >= 64 {
			break
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if migration.version == 54 {
			if err := ensureRoleExtendsColumn(context.Background(), tx); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version(version,applied_at) VALUES (?,'now')`, migration.version); err != nil {
			t.Fatal(err)
		}
	}
	projectRole := func(id, project, key, extends string, deleted bool) string {
		deletedAt := "NULL"
		if deleted {
			deletedAt = "'then'"
		}
		return `INSERT INTO role(id,key,project_id,role_key,label,body,extends,cardinality,deleted_at) VALUES ('` + id + `','` + key + `@` + project + `','` + project +
			`','` + key + `','` + key + `','body of ` + id + `','` + extends + `','multi',` + deletedAt + `)`
	}
	for _, statement := range []string{
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_old','host','hash',1,1)`,
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_a','A','/tmp/a','now'),('prj_b','B','/tmp/b','now')`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_a','prj_a','mch_old','/tmp/a')`,
		// Only projects define "analyst"; two do, so the first by id is converted and the second dropped.
		projectRole("role_1", "prj_a", "analyst", "", false),
		`UPDATE role SET cap_is_lead=1,cap_can_spawn=1,cap_singleton_writer=1 WHERE id='role_1'`,
		projectRole("role_2", "prj_b", "analyst", "", false),
		// A project override of a global role is dropped, and so is a project lead.
		projectRole("role_3", "prj_a", "reviewer", "", false),
		projectRole("role_4", "prj_a", "lead", "", false),
		// A deleted project role stays as history.
		projectRole("role_5", "prj_a", "gone", "", true),
		// An agent held the dropped override, another the converted role.
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('agt_over','over','prj_a','role_3','codex','native','m','p','mch_old','chk_a','h1','owner','now')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('agt_kept','kept','prj_a','role_1','codex','native','m','p','mch_old','chk_a','h2','owner','now')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	type row struct {
		project sql.NullString
		deleted bool
		key     string
	}
	read := func(id string) row {
		var r row
		if err := s.db.QueryRow(`SELECT project_id,deleted_at IS NOT NULL,key FROM role WHERE id=?`, id).Scan(&r.project, &r.deleted, &r.key); err != nil {
			t.Fatalf("role %s: %v", id, err)
		}
		return r
	}
	if r := read("role_1"); r.project.Valid || r.deleted || r.key != "analyst" {
		t.Errorf("the first analyst should be a live global role: %+v", r)
	}
	var lead, spawn, writer bool
	if err := s.db.QueryRow(`SELECT cap_is_lead,cap_can_spawn,cap_singleton_writer FROM role WHERE id='role_1'`).Scan(&lead, &spawn, &writer); err != nil || lead || spawn || writer {
		t.Errorf("a converted role must not carry capabilities: %v %v %v %v", lead, spawn, writer, err)
	}
	for _, id := range []string{"role_2", "role_3", "role_4"} {
		if r := read(id); r.project.Valid || !r.deleted || r.key != "deleted:"+id {
			t.Errorf("%s should be dropped: %+v", id, r)
		}
	}
	if r := read("role_5"); r.project.Valid || !r.deleted {
		t.Errorf("a deleted project role should stay deleted: %+v", r)
	}
	var project int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM role WHERE project_id IS NOT NULL`).Scan(&project); err != nil || project != 0 {
		t.Errorf("project roles left: %d %v", project, err)
	}
	for agent, want := range map[string]string{"agt_over": "role_reviewer_default", "agt_kept": "role_1"} {
		var got string
		if err := s.db.QueryRow(`SELECT role_id FROM agent WHERE id=?`, agent).Scan(&got); err != nil || got != want {
			t.Errorf("%s role=%q err=%v, want %q", agent, got, err, want)
		}
	}
	roles, err := s.ListRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]int{}
	for _, r := range roles {
		live[r.Key]++
	}
	if live["analyst"] != 1 || live["reviewer"] != 1 || live["lead"] != 1 {
		t.Errorf("live roles by key: %v", live)
	}
}
