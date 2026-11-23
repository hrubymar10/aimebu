package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/worktree"
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

func TestWorktreeErrorsSeparateUnavailableMachineFromInternalFailure(t *testing.T) {
	for _, test := range []struct {
		err  error
		code int
		name string
	}{
		{ErrMachineUnavailable, 503, "machine_unavailable"},
		{ErrSlowMachine, 503, "machine_unavailable"},
		{errWorktreeUnsupported, 409, "supervisor_update_needed"},
		{errors.New("host git command failed"), 409, "worktree_failed"},
		{sql.ErrConnDone, 500, "internal_error"},
	} {
		recorder := httptest.NewRecorder()
		worktreeError(recorder, test.err)
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != test.code || body["error"] != test.name {
			t.Fatalf("%v = %d %+v", test.err, recorder.Code, body)
		}
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

func TestRemoveOpenRoomWorktreeArchivesRoomFirst(t *testing.T) {
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
	path := "/api/rooms/" + f.room.ID + "/worktree"
	resp := f.request(http.MethodPost, path, map[string]any{"enabled": true}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create worktree: %d", resp.StatusCode)
	}
	resp = f.request(http.MethodDelete, path, map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete open-room worktree: %d", resp.StatusCode)
	}
	room, err := f.db.RoomByID(t.Context(), f.room.ID)
	if err != nil || room.Lifecycle != "closed" {
		t.Fatalf("room after worktree removal: %+v %v", room, err)
	}
	item, found, err := f.db.RoomWorktree(t.Context(), f.room.ID, f.machineID)
	if err != nil || !found || item.State != "removed" {
		t.Fatalf("worktree after removal: %+v found=%v err=%v", item, found, err)
	}
}

// A viewer is told where a room's worktree is under the project, not the absolute path on the machine's disk; the people who work there see it.
func TestViewerDoesNotSeeTheWorktreesAbsolutePath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	task, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "task", Title: "Worktree room", CreatedBy: f.ownerUserID(t)})
	if err != nil {
		t.Fatal(err)
	}
	// The room asks for a worktree (a row for the room), and the machine's answer is recorded (a row for the room on that machine, which names the path).
	if _, err := f.db.RequestRoomWorktree(t.Context(), task.ID, "main"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordRoomWorktree(t.Context(), task.ID, f.machineID, "/srv/secret/disk/layout/.aimebu/worktrees/"+task.ID, "main", "base", "ready", "", 0); err != nil {
		t.Fatal(err)
	}
	viewerCookie, viewerCSRF := f.projectMember(t, "worktreeviewer", f.project.ID, "viewer")
	read := func(cookie *http.Cookie, csrf string) map[string]any {
		t.Helper()
		resp := f.requestAs(t, cookie, csrf, "GET", "/api/rooms/"+task.ID+"/worktree", nil)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	if owner := read(f.cookie, f.csrf); owner["path"] == nil || owner["path"] == "" {
		t.Fatalf("the owner should see the path: %v", owner)
	}
	viewer := read(viewerCookie, viewerCSRF)
	if p, _ := viewer["path"].(string); p != "" {
		t.Fatalf("a viewer saw the absolute path: %v", viewer)
	}
	if viewer["relative_path"] == nil || viewer["relative_path"] == "" {
		t.Fatalf("a viewer still sees where it is under the project: %v", viewer)
	}
}

// A viewer's agent card hides boundary grant paths on a copy, retaining the
// original paths for the owner.
func TestCardWithoutPathsBlanksTheAbsolutePaths(t *testing.T) {
	card := store.AgentCard{ID: "agt_x", BoundaryGrants: []store.BoundaryGrant{{MachineID: "mch_1", Path: "/srv/secret/other", ApprovedAt: "then"}}}
	shown := cardWithoutPaths(card)
	if shown.BoundaryGrants[0].Path != "" || shown.BoundaryGrants[0].MachineID != "mch_1" {
		t.Fatalf("shown=%+v", shown)
	}
	if card.BoundaryGrants[0].Path == "" {
		t.Fatal("the card the server holds lost its paths")
	}
}

// A room's worktree is made once from one base. A later spawn that names another base_ref gets that said, not a bare "conflict".
func TestSpawnBaseRefAgainstTheRoomsWorktree(t *testing.T) {
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
	gitTest(t, root, "branch", "other")
	gitTest(t, root, "tag", "v1")
	room := f.room
	host, err := f.db.HostMachine(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ref  string
		fail string
	}{
		{"", ""},                   // no base_ref: the room's own base
		{"other", "already based"}, // another spelling of a base the room has
		{"v1", "already based"},
		{"nope", "already based"},
	} {
		_, err := f.srv.ensureRoomWorktree(t.Context(), room, host, test.ref)
		if test.fail == "" {
			if err != nil {
				t.Fatalf("ref %q: %v", test.ref, err)
			}
			continue
		}
		if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), test.fail) || !strings.Contains(err.Error(), `"`+test.ref+`"`) {
			t.Fatalf("ref %q: %v", test.ref, err)
		}
		recorder := httptest.NewRecorder()
		worktreeError(recorder, err)
		if recorder.Code != 409 || !strings.Contains(recorder.Body.String(), test.fail) {
			t.Fatalf("ref %q: %d %s", test.ref, recorder.Code, recorder.Body)
		}
	}
}
