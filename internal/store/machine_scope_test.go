package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineScopeDefaultsAndActiveAgentGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	p1, agent := f.project, f.agent
	p2, err := s.CreateProject(ctx, Project{Name: "second", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := s.MachineByID(ctx, agent.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	if machine.ScopeMode != "all" || len(machine.AllowedProjects) != 0 || machine.Disabled {
		t.Fatalf("new machine scope: %+v", machine)
	}
	if err := s.SetMachineScope(ctx, machine.ID, "listed", []string{p2.ID}, false); err == nil || !strings.Contains(err.Error(), agent.Slug) || !strings.Contains(err.Error(), p1.Name) {
		t.Fatalf("narrow active scope: %v", err)
	}
	if err := s.SetMachineScope(ctx, machine.ID, "all", nil, true); err == nil || !strings.Contains(err.Error(), agent.Slug) {
		t.Fatalf("disable active host: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMachineScope(ctx, machine.ID, "listed", []string{p2.ID}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMachineProject(ctx, p1.ID, machine.ID); err == nil || !strings.Contains(err.Error(), machine.Name) || !strings.Contains(err.Error(), p1.Name) {
		t.Fatalf("scope refusal: %v", err)
	}
	if err := s.CheckMachineProject(ctx, p2.ID, machine.ID); err == nil {
		t.Fatal("disabled machine accepted a project")
	}
}

func TestMachineScopeRefusesToExcludeDrainingSwitchTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	var target Machine
	if err := s.Tx(ctx, func(tx *Tx) (err error) {
		target, err = tx.CreateMachine(Machine{Name: "switch-target", KeyHash: "switch-key", Approved: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO agent_switch(agent_id,room_id,tool,variant,model,provider,machine_id,checkout_path,state,requested_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, f.agent.ID, f.room.ID, "codex", "native", "test", "test", target.ID, "/remote/test", "draining", timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMachineScope(ctx, target.ID, "listed", nil, false); err == nil || !strings.Contains(err.Error(), f.agent.Slug) {
		t.Fatalf("excluding the pending switch target: %v", err)
	}
}

func TestMachineScopeMigrationKeepsExistingMachinesUnrestricted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "prior.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrateUpTo(ctx, db, 146); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_prior','Host','key',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var mode string
	var disabled bool
	if err := db.QueryRow(`SELECT scope_mode,disabled FROM machine WHERE id='mch_prior'`).Scan(&mode, &disabled); err != nil {
		t.Fatal(err)
	}
	if mode != "all" || disabled {
		t.Fatalf("old machine mode=%q disabled=%t", mode, disabled)
	}
}

func TestListedMachineScopeStaysClosedAfterProjectDeletionAndArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	var machine Machine
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		machine, err = tx.CreateMachine(Machine{Name: "host", KeyHash: "key", Approved: true, IsHost: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	project, err := s.CreateProject(ctx, Project{Name: "only", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateProject(ctx, Project{Name: "other", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMachineScope(ctx, machine.ID, "listed", []string{project.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMachineProject(ctx, other.ID, machine.ID); err == nil {
		t.Fatal("another project entered listed scope")
	}
	if _, err := s.db.Exec(`UPDATE project SET archived_at=? WHERE id=?`, timestamp(), project.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMachineProject(ctx, other.ID, machine.ID); err == nil {
		t.Fatal("archiving the only listed project widened scope")
	}
	if err := s.DeleteProject(ctx, project.ID, project.Name, "usr_test"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMachineProject(ctx, other.ID, machine.ID); err == nil {
		t.Fatal("deleting the last listed project widened scope")
	}
	machine, err = s.MachineByID(ctx, machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if machine.ScopeMode != "listed" || len(machine.AllowedProjects) != 0 {
		t.Fatalf("deleted project scope: %+v", machine)
	}
}
