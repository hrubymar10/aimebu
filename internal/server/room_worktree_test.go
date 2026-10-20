package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/worktree"
)

func TestRemoteWorktreeOperationUsesSupervisor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	machine := &machineConn{id: "mch_worktree", supportsWorktree: true, sendQueue: make(chan protocol.Frame, 1), done: make(chan struct{})}
	f.srv.machineMu.Lock()
	f.srv.machines[machine.id] = machine
	f.srv.machineMu.Unlock()
	go func() {
		request := <-machine.sendQueue
		if request.Type != protocol.WorktreeOp || request.CheckoutPath != "/remote/project" || request.RoomID != "room_test" || request.WorktreeAction != "ensure" {
			close(machine.done)
			return
		}
		payload, _ := json.Marshal(worktree.Result{Path: "/remote/project/.aimebu/worktrees/room_test", Branch: "aimebu/room_test", State: "ready"})
		machine.harnessMu.Lock()
		waiter := machine.pathPending[request.ReqID]
		machine.harnessMu.Unlock()
		waiter <- protocol.Frame{V: protocol.Version, Type: protocol.WorktreeResult, ReqID: request.ReqID, Details: payload}
	}()
	result, err := f.srv.machineWorktree(context.Background(), store.Machine{ID: machine.id}, "/remote/project", "room_test", "main", "ensure", false)
	if err != nil || result.State != "ready" {
		t.Fatalf("remote operation=%+v %v", result, err)
	}
}

func TestRemoteWorktreeRejectsOldSupervisorWithoutSending(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	machine := &machineConn{id: "mch_old_worktree", sendQueue: make(chan protocol.Frame, 1), done: make(chan struct{})}
	f.srv.machineMu.Lock()
	f.srv.machines[machine.id] = machine
	f.srv.machineMu.Unlock()
	if _, err := f.srv.machineWorktree(t.Context(), store.Machine{ID: machine.id}, "/remote/project", "room_test", "main", "ensure", false); err == nil {
		t.Fatal("old supervisor accepted worktree operation")
	}
	if len(machine.sendQueue) != 0 {
		t.Fatal("old supervisor received an unsupported frame")
	}
}

func TestRemoteWorktreePreservesTypedGitError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	machine := &machineConn{id: "mch_worktree_error", supportsWorktree: true, sendQueue: make(chan protocol.Frame, 1), done: make(chan struct{})}
	f.srv.machineMu.Lock()
	f.srv.machines[machine.id] = machine
	f.srv.machineMu.Unlock()
	go func() {
		request := <-machine.sendQueue
		machine.harnessMu.Lock()
		waiter := machine.pathPending[request.ReqID]
		machine.harnessMu.Unlock()
		waiter <- protocol.Frame{V: protocol.Version, Type: protocol.WorktreeResult, ReqID: request.ReqID, Code: "not_git_root", Message: "project folder must be the Git worktree root"}
	}()
	_, err := f.srv.machineWorktree(t.Context(), store.Machine{ID: machine.id}, "/remote/project", "room_test", "", "validate", false)
	var gitErr *worktree.Error
	if !errors.As(err, &gitErr) || gitErr.Code != "not_git_root" {
		t.Fatalf("remote error=%v", err)
	}
}

func TestRoomWorktreeKeepsMachineStatusSeparate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if _, err := f.db.RequestRoomWorktree(t.Context(), f.room.ID, "main"); err != nil {
		t.Fatal(err)
	}
	hostPath := "/host/.aimebu/worktrees/" + f.room.ID
	if err := f.db.RecordRoomWorktree(t.Context(), f.room.ID, f.machineID, hostPath, "main", "base", "ready", "", 2); err != nil {
		t.Fatal(err)
	}
	var remote store.Machine
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		remote, err = tx.CreateMachine(store.Machine{Name: "remote-worktree", KeyHash: "remote-key", Approved: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	remotePath := "/remote/.aimebu/worktrees/" + f.room.ID
	if err := f.db.RecordRoomWorktree(t.Context(), f.room.ID, remote.ID, remotePath, "main", "remote-base", "ready", "", 0); err != nil {
		t.Fatal(err)
	}
	host, _, err := f.db.RoomWorktree(t.Context(), f.room.ID, f.machineID)
	if err != nil || host.Path != hostPath || host.DirtyBaseCount != 2 || host.BaseCommit != "base" {
		t.Fatalf("host worktree=%+v err=%v", host, err)
	}
	other, _, err := f.db.RoomWorktree(t.Context(), f.room.ID, remote.ID)
	if err != nil || other.Path != remotePath || other.DirtyBaseCount != 0 || other.BaseCommit != "remote-base" {
		t.Fatalf("remote worktree=%+v err=%v", other, err)
	}
	if err := f.db.RecordRoomWorktree(t.Context(), f.room.ID, remote.ID, "", "main", "", "failed", "temporarily unavailable", 0); err != nil {
		t.Fatal(err)
	}
	other, _, err = f.db.RoomWorktree(t.Context(), f.room.ID, remote.ID)
	if err != nil || other.Path != remotePath || other.BaseCommit != "remote-base" || other.State != "failed" {
		t.Fatalf("failed retry discarded path: %+v err=%v", other, err)
	}
}

func TestWorktreeOptInValidatesBeforePersistingAndCanCancel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID + "/worktree"
	resp := f.request(http.MethodPost, path, map[string]any{"enabled": true}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("non-Git opt-in status=%d", resp.StatusCode)
	}
	if _, found, err := f.db.RoomWorktree(t.Context(), f.room.ID, f.machineID); err != nil || found {
		t.Fatalf("invalid opt-in persisted: found=%v err=%v", found, err)
	}
	gitTest(t, f.project.Folder, "init", "-q")
	gitTest(t, f.project.Folder, "config", "user.name", "Test")
	gitTest(t, f.project.Folder, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(f.project.Folder, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.project.Folder, "add", "tracked.txt")
	gitTest(t, f.project.Folder, "commit", "-qm", "base")
	if _, err := f.db.RequestRoomWorktree(t.Context(), f.room.ID, "main"); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodDelete, path, map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status=%d", resp.StatusCode)
	}
	if _, found, err := f.db.RoomWorktree(t.Context(), f.room.ID, f.machineID); err != nil || found {
		t.Fatalf("canceled opt-in persisted: found=%v err=%v", found, err)
	}
}

func gitTest(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func TestRoomWorktreeAPICloseAndReopen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	root := f.project.Folder
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "Test")
	gitTest(t, root, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", "tracked.txt")
	gitTest(t, root, "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("uncommitted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := "/api/rooms/" + f.room.ID + "/worktree"
	resp := f.request(http.MethodPost, path, map[string]any{"enabled": true}, true)
	var item store.RoomWorktree
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || item.State != "ready" || item.DirtyBaseCount != 1 || item.Path != filepath.Join(root, ".aimebu", "worktrees", f.room.ID) {
		t.Fatalf("create status=%d item=%+v", resp.StatusCode, item)
	}
	if !store.WithinFolder(root, item.Path) {
		t.Fatalf("worktree path escaped project boundary: %s", item.Path)
	}
	resp = f.request(http.MethodGet, path, nil, false)
	var listed store.RoomWorktree
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if listed.Path != item.Path {
		t.Fatalf("listed=%+v", listed)
	}
	resp = f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/close", map[string]any{"lifecycle_rev": 0}, true)
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("close=%d", resp.StatusCode)
	}
	if _, failures, err := f.db.AdvanceRoomClosures(t.Context(), 0); err != nil || len(failures) != 0 {
		t.Fatalf("advance=%v %+v", err, failures)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_worktree_guard',?,?,?)`, f.project.ID, f.machineID, item.Path); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
			VALUES ('agt_worktree_guard','guard',?,(SELECT id FROM role WHERE role_key='worker'),'fake','test','test','fake',?,'chk_worktree_guard','key','owner','now')`, f.project.ID, f.machineID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodDelete, path, map[string]any{"force": true, "confirm": "REMOVE " + f.room.ID}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("active checkout removal status=%d", resp.StatusCode)
	}
	// Nothing removes a worktree on its own, not even a closed room's merged one: the cleanup leaves it, and an active checkout
	// cannot be removed by hand either.
	f.cleanup(false)
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("a worktree was removed automatically: %v", err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id='agt_worktree_guard'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.cleanup(false)
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("a worktree was removed automatically once its agent stopped: %v", err)
	}
	if log, err := f.db.CleanupLog(t.Context(), 5); err != nil || len(log) != 0 {
		t.Fatalf("the cleanup log must be empty: %+v %v", log, err)
	}
	// Removing it is the owner's action, from the closed room's settings.
	resp = f.request(http.MethodDelete, path, map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manual removal status=%d", resp.StatusCode)
	}
	if _, err := os.Stat(item.Path); !os.IsNotExist(err) {
		t.Fatalf("clean worktree remains after the manual removal: %v", err)
	}
	resp = f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/reopen", map[string]any{"lifecycle_rev": 2}, true)
	var reopened store.Room
	if err := json.NewDecoder(resp.Body).Decode(&reopened); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || reopened.Worktree == nil || reopened.Worktree.State != "ready" {
		t.Fatalf("reopen=%d %+v", resp.StatusCode, reopened)
	}
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("restored worktree: %v", err)
	}
}
