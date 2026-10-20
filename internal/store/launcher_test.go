package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestReservedDockerLauncherNameCannotBeCustom(t *testing.T) {
	s, _, _, agent, _ := wakeFixture(t)
	for _, name := range []string{"docker"} {
		if _, err := s.SaveLauncher(t.Context(), "owner", Launcher{MachineID: agent.MachineID, Tool: "codex", Name: name, Command: "codex-docker"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("custom launcher name %q: %v", name, err)
		}
	}
}

func TestLauncherDeleteRejectsPendingSwitchAndRevokedRunningAgent(t *testing.T) {
	s, _, room, agent, _ := wakeFixture(t)
	launcher, err := s.SaveLauncher(t.Context(), "owner", Launcher{MachineID: agent.MachineID, Tool: "codex", Name: "custom", Command: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO agent_switch(agent_id,room_id,tool,variant,launcher_id,model,provider,state,requested_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		agent.ID, room.ID, "codex", "custom", launcher.ID, "gpt-6-sol", "openai", "draining", timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLauncher(t.Context(), agent.MachineID, launcher.ID, "owner"); !errors.Is(err, ErrLauncherInUse) {
		t.Fatalf("pending switch deletion: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent_switch SET state='starting' WHERE agent_id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLauncher(t.Context(), agent.MachineID, launcher.ID, "owner"); !errors.Is(err, ErrLauncherInUse) {
		t.Fatalf("starting switch deletion: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM agent_switch WHERE agent_id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET launcher_id=?,revoked_at=?,desired='active' WHERE id=?`, launcher.ID, timestamp(), agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLauncher(t.Context(), agent.MachineID, launcher.ID, "owner"); !errors.Is(err, ErrLauncherInUse) {
		t.Fatalf("revoked running agent deletion: %v", err)
	}
}

// Nothing is seeded: a new machine has no harness until the owner adds one.
func TestNewMachineHasNoHarnesses(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	var machine Machine
	if err := s.Tx(t.Context(), func(tx *Tx) error {
		var err error
		machine, err = tx.CreateMachine(Machine{Name: "new", KeyHash: "hash", Approved: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	launchers, err := s.MachineLaunchers(t.Context(), machine.ID)
	if err != nil || len(launchers) != 0 {
		t.Fatalf("new machine launchers=%+v err=%v", launchers, err)
	}
}

func TestLauncherStaysInUseUntilAgentObservesStop(t *testing.T) {
	t.Parallel()
	s, _, _, agent, _ := wakeFixture(t)
	launcher, err := s.SaveLauncher(t.Context(), "owner", Launcher{MachineID: agent.MachineID, Tool: "codex", Name: "native", Label: "Codex", Command: "codex", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	id := launcher.ID
	if _, err := s.db.Exec(`UPDATE agent SET launcher_id=?,desired='stopped',observed='in_turn' WHERE id=?`, id, agent.ID); err != nil {
		t.Fatal(err)
	}
	if used, err := s.LauncherInUse(t.Context(), id); err != nil || !used {
		t.Fatalf("stopping agent used=%v err=%v", used, err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if used, err := s.LauncherInUse(t.Context(), id); err != nil || used {
		t.Fatalf("stopped agent used=%v err=%v", used, err)
	}
}

func TestMigrationTwentyEightPreservesExistingLauncherChoices(t *testing.T) {
	for _, nativeAllowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "native-off", true: "native-on"}[nativeAllowed], func(t *testing.T) {
			if nativeAllowed {
				t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
			} else {
				t.Setenv("AIMEBU2_ALLOW_NATIVE", "")
			}
			db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-26.sqlite")))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			for _, migration := range migrations {
				if migration.version >= 28 {
					break
				}
				if _, err := db.Exec(migration.sql); err != nil {
					t.Fatalf("migration %d: %v", migration.version, err)
				}
				if _, err := db.Exec(`INSERT INTO schema_version(version,applied_at) VALUES (?,'now')`, migration.version); err != nil {
					t.Fatal(err)
				}
			}
			for _, statement := range []string{
				`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_old','host','hash',1,1)`,
				`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_remote','remote','hash',1,0)`,
				`INSERT INTO machine_harness_setting(machine_id,tool,variant,enabled,override_path) VALUES ('mch_old','claude','docker',0,'/custom/claude-docker')`,
				`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_old','Old','/tmp/old','now')`,
				`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_old','prj_old','mch_old','/tmp/old')`,
				`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('agt_old','old','prj_old','role_worker_default','codex','native','gpt-6-sol','openai','mch_old','chk_old','hash','owner','now')`,
			} {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := migrateUpTo(context.Background(), db, 68); err != nil { // these are migration 28's decisions; migration 69 turns launchers into harnesses
				t.Fatal(err)
			}
			var native, docker, disabled bool
			if err := db.QueryRow(`SELECT enabled FROM machine_launcher WHERE machine_id='mch_old' AND tool='codex' AND name='native'`).Scan(&native); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT enabled FROM machine_launcher WHERE machine_id='mch_old' AND tool='codex' AND name='harness-docker'`).Scan(&docker); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT enabled FROM machine_launcher WHERE machine_id='mch_old' AND tool='claude' AND name='harness-docker'`).Scan(&disabled); err != nil {
				t.Fatal(err)
			}
			if native != nativeAllowed || !docker || disabled {
				t.Fatalf("native=%t docker=%t disabled=%t", native, docker, disabled)
			}
			var remoteNative bool
			if err := db.QueryRow(`SELECT enabled FROM machine_launcher WHERE machine_id='mch_remote' AND tool='codex' AND name='native'`).Scan(&remoteNative); err != nil || remoteNative {
				t.Fatalf("remote native=%t err=%v", remoteNative, err)
			}
			var reservedCustom int
			if err := db.QueryRow(`SELECT COUNT(*) FROM machine_launcher WHERE preset=0 AND name IN ('docker','harness-docker')`).Scan(&reservedCustom); err != nil || reservedCustom != 0 {
				t.Fatalf("migration created reserved custom launchers=%d err=%v", reservedCustom, err)
			}
			if nativeAllowed {
				t.Setenv("AIMEBU2_ALLOW_NATIVE", "")
			} else {
				t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
			}
			if err := migrateUpTo(context.Background(), db, 68); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT enabled FROM machine_launcher WHERE machine_id='mch_old' AND tool='codex' AND name='native'`).Scan(&native); err != nil || native != nativeAllowed {
				t.Fatalf("native changed after migration: %t, %v", native, err)
			}
			var command, launcherID string
			if err := db.QueryRow(`SELECT command FROM machine_launcher WHERE machine_id='mch_old' AND tool='claude' AND name='harness-docker'`).Scan(&command); err != nil || command != "/custom/claude-docker" {
				t.Fatalf("command=%q err=%v", command, err)
			}
			if err := db.QueryRow(`SELECT launcher_id FROM agent WHERE id='agt_old'`).Scan(&launcherID); err != nil || launcherID == "" {
				t.Fatalf("agent launcher=%q err=%v", launcherID, err)
			}
		})
	}
}
