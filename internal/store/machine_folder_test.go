package store

import (
	"context"
	"errors"
	"testing"
)

func TestMachineScopeRefusalDoesNotDiscloseToNonMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	project, _, _ := fixture(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_admin','admin','hash','{}','a'),('usr_stranger','stranger','hash','{}','z')`); err != nil {
		t.Fatal(err)
	}
	var machine Machine
	err := s.Tx(ctx, func(tx *Tx) (err error) {
		machine, err = tx.CreateMachine(Machine{Name: "private machine", KeyHash: "private-key", Approved: true})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMachineScope(ctx, machine.ID, "listed", nil, false); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"folder": func() error {
			_, err := s.SetProjectMachineFolder(ctx, project.ID, machine.ID, "usr_stranger", "/private")
			return err
		},
		"admit": func() error { return s.AdmitProjectMachine(ctx, project.ID, machine.ID, "usr_stranger") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrForbidden) {
				t.Fatalf("got %v, want forbidden before scope details", err)
			}
		})
	}
}

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

// A container launcher is asked its version from a folder its container mounts, which the server cannot tell: the machine's folders come
// back in project order, each once, up to the limit; on the host a project's own folder counts, on another machine only a mapped one.
func TestMachineProbeFolders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	var host, remote Machine
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		if host, err = tx.CreateMachine(Machine{Name: "host", KeyHash: "host-key", IsHost: true, Approved: true}); err != nil {
			return err
		}
		remote, err = tx.CreateMachine(Machine{Name: "remote", KeyHash: "remote-key", Approved: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var projects []Project
	for _, folder := range []string{"/work/a", "/work/b", "/work/a", "/work/c"} { // one folder shared by two projects counts once
		project, err := s.CreateProject(ctx, Project{Name: "p" + folder, Folder: folder})
		if err != nil {
			t.Fatal(err)
		}
		projects = append(projects, project)
	}
	list := func(machineID string, limit int) []string {
		t.Helper()
		got, err := s.MachineProbeFolders(ctx, machineID, limit)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := list(host.ID, 8); len(got) != 3 || got[0] != "/work/a" || got[1] != "/work/b" || got[2] != "/work/c" {
		t.Fatalf("host folders: %v", got)
	}
	if got := list(host.ID, 2); len(got) != 2 || got[1] != "/work/b" {
		t.Fatalf("limited: %v", got)
	}
	if got := list(remote.ID, 8); len(got) != 0 {
		t.Fatalf("a remote machine has no folder until one is mapped: %v", got)
	}
	for i, folder := range map[int]string{3: "/remote/c", 1: "/remote/b"} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,?,?)`, projects[i].ID, remote.ID, folder); err != nil {
			t.Fatal(err)
		}
	}
	if got := list(remote.ID, 8); len(got) != 2 || got[0] != "/remote/b" || got[1] != "/remote/c" {
		t.Fatalf("remote folders in project order: %v", got)
	}
	if first, err := s.FirstMachineFolder(ctx, remote.ID); err != nil || first != "/remote/b" {
		t.Fatalf("first folder: %q %v", first, err)
	}
}
