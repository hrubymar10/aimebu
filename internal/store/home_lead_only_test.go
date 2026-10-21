package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// Migration 70 adds "Home room: lead only" to every project, existing ones included, switched on.
func TestMigrationSeventyTurnsHomeLeadOnlyOnForExistingProjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "from-69.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version >= 70 {
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
	for _, statement := range []string{
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_old','Old','/tmp/old','now')`,
		`INSERT INTO settings_project(project_id) VALUES ('prj_old')`,
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
	var on bool
	if err := s.db.QueryRow(`SELECT home_lead_only FROM settings_project WHERE project_id='prj_old'`).Scan(&on); err != nil || !on {
		t.Fatalf("existing project home_lead_only=%v err=%v", on, err)
	}
	if _, err := s.db.Exec(`UPDATE settings_project SET home_lead_only=2 WHERE project_id='prj_old'`); err == nil {
		t.Fatal("home_lead_only accepted a value other than 0 or 1")
	}
}

// While the setting is on, only a lead-capable role is spawned into the home room; a task room takes any role, and turning the setting
// off restores spawning workers into the home room.
func TestSpawnIntoHomeRoomFollowsLeadOnly(t *testing.T) {
	t.Parallel()
	s, project, task, _, _ := wakeFixture(t)
	ctx := context.Background()
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: project.Name, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO settings_project(project_id) VALUES (?)`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_lead_only','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_lead_only','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE settings_project SET max_live_agents=50,spawn_rate_per_min=50 WHERE project_id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	settings, err := s.GetProjectRecoverySettings(ctx, project.ID, "usr_lead_only")
	if err != nil || !settings.HomeLeadOnly {
		t.Fatalf("new project settings=%+v err=%v", settings, err)
	}
	spawn := func(roomID, role, key string) error {
		return s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.SpawnAgent(SpawnInput{RoomID: roomID, ActorKind: "user", ActorID: "usr_lead_only", MachineID: "mch_wake", Path: "/tmp/wake", RoleKey: role, Tool: "fake", Variant: "test", Model: "test", Provider: "test", KeyHash: key})
			return err
		})
	}
	if err := spawn(home.ID, "worker", "k1"); !errors.Is(err, ErrHomeLeadOnly) {
		t.Fatalf("worker into lead-only home: %v", err)
	}
	if err := spawn(task.ID, "worker", "k2"); err != nil {
		t.Fatalf("worker into task room: %v", err)
	}
	off := false
	if _, err := s.SetProjectRecoverySettings(ctx, project.ID, "usr_lead_only", ProjectSettingsUpdate{HomeLeadOnly: &off}); err != nil {
		t.Fatal(err)
	}
	if err := spawn(home.ID, "worker", "k3"); err != nil {
		t.Fatalf("worker into home with the setting off: %v", err)
	}
}
