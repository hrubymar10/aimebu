package store

import (
	"context"
	"testing"
)

func TestFirstMachineFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	var host, remote Machine
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		host, err = tx.CreateMachine(Machine{Name: "host", KeyHash: "host-key", IsHost: true, Approved: true})
		if err != nil {
			return err
		}
		remote, err = tx.CreateMachine(Machine{Name: "remote", KeyHash: "remote-key", Approved: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check := func(machineID, want string) {
		t.Helper()
		got, err := s.FirstMachineFolder(ctx, machineID)
		if err != nil || got != want {
			t.Fatalf("first folder for %s = %q, %v; want %q", machineID, got, err, want)
		}
	}
	check(host.ID, "")
	check(remote.ID, "")
	project, err := s.CreateProject(ctx, Project{Name: "mapped", Folder: "/host/project"})
	if err != nil {
		t.Fatal(err)
	}
	check(host.ID, "/host/project")
	check(remote.ID, "")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,?,?)`, project.ID, remote.ID, "/remote/project"); err != nil {
		t.Fatal(err)
	}
	check(remote.ID, "/remote/project")
}

func TestChangingRemoteProjectFolderRequiresNewAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	project, _, _ := fixture(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_folder_owner','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_folder_owner','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	var machine Machine
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		machine, err = tx.CreateMachine(Machine{Name: "remote", KeyHash: "remote-key", Approved: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	set := func(path string) {
		t.Helper()
		if _, err := s.SetProjectMachineFolder(ctx, project.ID, machine.ID, "usr_folder_owner", path); err != nil {
			t.Fatal(err)
		}
	}
	allowed := func(want bool) {
		t.Helper()
		got, err := s.ProjectMachineAllowed(ctx, project.ID, machine.ID)
		if err != nil || got != want {
			t.Fatalf("project machine allowed=%t, %v; want %t", got, err, want)
		}
	}
	set("/remote/first")
	if _, err := s.db.Exec(`INSERT INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at) VALUES (?,?,?,?)`, project.ID, machine.ID, "usr_folder_owner", timestamp()); err != nil {
		t.Fatal(err)
	}
	set("/remote/first")
	allowed(true)
	set("/remote/second")
	allowed(false)
}
