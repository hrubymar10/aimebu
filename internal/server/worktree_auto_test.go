package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T, root string) {
	t.Helper()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "Test")
	gitTest(t, root, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", "tracked.txt")
	gitTest(t, root, "commit", "-qm", "base")
}

// A task room's agents get their own worktree by default (worktree_mode auto); a folder that is not a Git root, and a project with the
// mode off, keep the project folder, and nothing is recorded for them.
func TestTaskRoomsGetAWorktreeByDefaultOnlyWhereItApplies(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine, err := f.db.MachineByID(t.Context(), f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	root := f.project.Folder
	// Not a Git repository: the project folder, no worktree row.
	path, item, err := f.srv.roomCheckoutPath(t.Context(), f.room, machine, false, "")
	if err != nil || item != nil || path != root {
		t.Fatalf("non-Git folder: path=%q item=%v err=%v", path, item, err)
	}
	if _, found, err := f.db.RoomWorktree(t.Context(), f.room.ID, f.machineID); err != nil || found {
		t.Fatalf("a fallback must not persist a worktree: found=%v err=%v", found, err)
	}
	initRepo(t, root)
	// The mode off keeps the folder even in a repository.
	resp := f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/settings", map[string]any{"worktree_mode": "off"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("settings: %d", resp.StatusCode)
	}
	if path, item, err = f.srv.roomCheckoutPath(t.Context(), f.room, machine, false, ""); err != nil || item != nil || path != root {
		t.Fatalf("mode off: path=%q item=%v err=%v", path, item, err)
	}
	resp = f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/settings", map[string]any{"worktree_mode": "sometimes"}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("bad mode: %d", resp.StatusCode)
	}
	resp = f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/settings", map[string]any{"worktree_mode": "auto"}, true)
	resp.Body.Close()
	// The room that already worked in the project folder is not split by turning the setting on.
	if path, item, err = f.srv.roomCheckoutPath(t.Context(), f.room, machine, false, ""); err != nil || item != nil || path != root {
		t.Fatalf("an existing room was split: path=%q item=%v err=%v", path, item, err)
	}
	fresh := f.newTaskRoom("fresh")
	path, item, err = f.srv.roomCheckoutPath(t.Context(), fresh, machine, false, "")
	want := filepath.Join(root, ".aimebu", "worktrees", fresh.ID)
	if err != nil || item == nil || item.State != "ready" || path != want {
		t.Fatalf("auto: path=%q item=%+v err=%v", path, item, err)
	}
	// The explicit settings call still works on its own, and a PUT with nothing in it is refused.
	resp = f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/settings", map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("empty settings: %d", resp.StatusCode)
	}
}

// Automatic worktrees are a convenience: a repository with no commits (or any other reason the worktree cannot be made) leaves the
// agent in the project folder and nothing pending on the room. The first spawn decides and the decision stays: a commit made
// afterwards does not give the next agent a worktree, which would split the room between two checkouts.
func TestAutoWorktreeFallsBackOnAnyFailureAndTheDecisionIsSticky(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine, err := f.db.MachineByID(t.Context(), f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	root := f.project.Folder
	gitTest(t, root, "init", "-q") // no commit: there is nothing to branch from
	path, item, err := f.srv.roomCheckoutPath(t.Context(), f.room, machine, false, "")
	if err != nil || item != nil || path != root {
		t.Fatalf("unborn HEAD: path=%q item=%v err=%v", path, item, err)
	}
	if _, found, err := f.db.RoomWorktree(t.Context(), f.room.ID, f.machineID); err != nil || found {
		t.Fatalf("a failed automatic worktree left a request behind: found=%v err=%v", found, err)
	}
	if mode, _ := f.db.RoomCheckoutMode(t.Context(), f.room.ID); mode != "project_folder" {
		t.Fatalf("the first spawn did not decide: %q", mode)
	}
	// The repository gets its first commit; the room is not split.
	gitTest(t, root, "config", "user.name", "Test")
	gitTest(t, root, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", "tracked.txt")
	gitTest(t, root, "commit", "-qm", "base")
	path, item, err = f.srv.roomCheckoutPath(t.Context(), f.room, machine, false, "")
	if err != nil || item != nil || path != root {
		t.Fatalf("second spawn after a commit must stay in the project folder: path=%q item=%v err=%v", path, item, err)
	}
	// An explicit request is the person's choice and does make the worktree.
	path, item, err = f.srv.roomCheckoutPath(t.Context(), f.room, machine, true, "")
	if err != nil || item == nil || path != filepath.Join(root, ".aimebu", "worktrees", f.room.ID) {
		t.Fatalf("explicit request: path=%q item=%v err=%v", path, item, err)
	}
	if mode, _ := f.db.RoomCheckoutMode(t.Context(), f.room.ID); mode != "worktree" {
		t.Fatalf("an explicit worktree did not update the decision: %q", mode)
	}
}

func TestExplicitWorktreeRequestOnAnUnbornRepositoryStillFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine, _ := f.db.MachineByID(t.Context(), f.machineID)
	gitTest(t, f.project.Folder, "init", "-q")
	if _, _, err := f.srv.roomCheckoutPath(t.Context(), f.room, machine, true, ""); err == nil {
		t.Fatal("an explicit worktree request on a repository with no commits must fail")
	}
}
