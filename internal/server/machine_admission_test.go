package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// remoteMachine adds a connected machine that is not the host, with the project's folder mapped on it. The project's owner has not admitted it unless
// the test says so.
func (f *fixture) remoteMachine(t *testing.T, admitted bool) string {
	t.Helper()
	var remote store.Machine
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		if remote, err = tx.CreateMachine(store.Machine{Name: "remote-box", KeyHash: "remote-key", Approved: true}); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,?,?)`, f.project.ID, remote.ID, "/remote/project"); err != nil {
			return err
		}
		if admitted {
			_, err = tx.Exec(`INSERT INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at) SELECT ?,?,id,? FROM user WHERE login='owner'`, f.project.ID, remote.ID, "now")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conn := &machineConn{id: remote.ID, supportsSessionScan: true, supportsWorktree: true, supportsPathProbe: true, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{})}
	f.srv.machineMu.Lock()
	f.srv.machines[remote.ID] = conn
	f.srv.machineMu.Unlock()
	t.Cleanup(func() {
		select {
		case <-conn.done:
		default:
			close(conn.done)
		}
	})
	return remote.ID
}

// What lists a project's Claude sessions or makes a worktree in its folder on another machine waits for the owner to admit that machine for the project: an
// operator of the project is not enough, and nothing is sent to the machine in the meantime. The owner may look, and the host is always admitted.
func TestAnotherMachineIsNotReadOrWrittenBeforeItIsAdmitted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	remote := f.remoteMachine(t, false)
	operator, operatorCSRF := f.projectMember(t, "operator-anna", f.project.ID, "operator")
	list := func(cookie *http.Cookie, csrf, machine string) (int, map[string]any) {
		t.Helper()
		resp := f.requestAs(t, cookie, csrf, "GET", "/api/projects/"+f.project.ID+"/claude-sessions?machine_id="+machine, nil)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	conn, err := f.srv.machineForID(remote)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := list(operator, operatorCSRF, remote); code != 403 || out["error"] != "machine_not_admitted" {
		t.Fatalf("an operator listing sessions on a machine that is not admitted: %d %v", code, out)
	}
	if len(conn.sendQueue) != 0 {
		t.Fatalf("a frame went to the machine before it was admitted: %d", len(conn.sendQueue))
	}
	if code, out := list(f.cookie, f.csrf, remote); code == 403 && out["error"] == "machine_not_admitted" {
		t.Fatalf("the owner may look at a machine that is not admitted yet: %d %v", code, out)
	}
	// The host is always admitted.
	if code, out := list(operator, operatorCSRF, f.machineID); code == 403 && out["error"] == "machine_not_admitted" {
		t.Fatalf("the host is refused: %d %v", code, out)
	}
	// A worktree is not made there either, by a request for one or by a spawn. The owner's look above did reach the machine, so count from here.
	before := len(conn.sendQueue)
	room, err := f.srv.Store.RoomByID(t.Context(), f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := f.db.MachineByID(t.Context(), remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.ensureRoomWorktree(t.Context(), room, machine, ""); err != errMachineNotAdmitted {
		t.Fatalf("a room worktree on a machine that is not admitted: %v", err)
	}
	slug := "ada"
	if tree, reason := f.srv.agentWorktreeForLocked(t.Context(), room, machine, &slug, "", false); tree != nil || reason != errMachineNotAdmitted.Error() {
		t.Fatalf("an agent worktree on a machine that is not admitted: %+v %q", tree, reason)
	}
	if len(conn.sendQueue) != before {
		t.Fatalf("a worktree frame went to the machine before it was admitted: %d", len(conn.sendQueue))
	}
	// Once the owner has admitted it, the gate opens for the operator too.
	if _, err := f.exec(`INSERT INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at) SELECT ?,?,id,? FROM user WHERE login='owner'`, f.project.ID, remote, "now"); err != nil {
		t.Fatal(err)
	}
	if code, out := list(operator, operatorCSRF, remote); code == 403 && out["error"] == "machine_not_admitted" {
		t.Fatalf("an admitted machine is still refused: %d %v", code, out)
	}
}

// The project's owner admits a machine without having to start an agent to raise the approval (a task room that makes worktrees could not raise
// it before the machine is admitted), an operator cannot, and the project's owner is not held at the gate they alone can open.
func TestTheProjectOwnerCanAdmitAMachineAndIsNotHeldAtTheGate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	remote := f.remoteMachine(t, false)
	operator, operatorCSRF := f.projectMember(t, "admit-operator", f.project.ID, "operator")
	projectOwner, projectOwnerCSRF := f.projectMember(t, "admit-owner", f.project.ID, "owner")
	listing := func(cookie *http.Cookie, csrf string) (int, string) {
		t.Helper()
		resp := f.requestAs(t, cookie, csrf, "GET", "/api/projects/"+f.project.ID+"/claude-sessions?machine_id="+remote, nil)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		code, _ := out["error"].(string)
		return resp.StatusCode, code
	}
	admit := func(cookie *http.Cookie, csrf, machine string) int {
		t.Helper()
		resp := f.requestAs(t, cookie, csrf, "POST", "/api/projects/"+f.project.ID+"/machine-folders/"+machine+"/admit", map[string]any{})
		resp.Body.Close()
		return resp.StatusCode
	}
	if code, name := listing(operator, operatorCSRF); code != 403 || name != "machine_not_admitted" {
		t.Fatalf("an operator before admission: %d %q", code, name)
	}
	if _, name := listing(projectOwner, projectOwnerCSRF); name == "machine_not_admitted" {
		t.Fatal("the project's owner is held at a gate only they can open")
	}
	if got := admit(operator, operatorCSRF, remote); got != 403 {
		t.Fatalf("an operator admitting a machine: %d", got)
	}
	if got := admit(projectOwner, projectOwnerCSRF, "mch_does_not_exist"); got != 404 {
		t.Fatalf("admitting a machine that does not exist: %d", got)
	}
	if got := admit(projectOwner, projectOwnerCSRF, remote); got != 204 {
		t.Fatalf("the project's owner admitting a machine: %d", got)
	}
	if got := admit(projectOwner, projectOwnerCSRF, remote); got != 204 {
		t.Fatalf("admitting twice: %d", got)
	}
	if allowed, err := f.db.ProjectMachineAllowed(t.Context(), f.project.ID, remote); err != nil || !allowed {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
	if _, name := listing(operator, operatorCSRF); name == "machine_not_admitted" {
		t.Fatalf("an operator after admission is still held")
	}
	// A machine the project has no folder on cannot be admitted.
	var bare store.Machine
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		bare, err = tx.CreateMachine(store.Machine{Name: "bare-box", KeyHash: "bare-key", Approved: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := admit(projectOwner, projectOwnerCSRF, bare.ID); got != 409 {
		t.Fatalf("admitting a machine with no folder mapped: %d", got)
	}
}
