package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func repoFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q", root}, {"-C", root, "config", "user.name", "Test"},
		{"-C", root, "config", "user.email", "test@example.invalid"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-qm", "base"}} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	return root
}

func TestEnsureDirtyBaseAndSafeCleanup(t *testing.T) {
	root := repoFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("uncommitted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := Ensure(ctx, root, "room_test", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.DirtyBase != 1 || first.State != "ready" || first.Branch != "aimebu/room_test" {
		t.Fatalf("first=%+v", first)
	}
	data, err := os.ReadFile(filepath.Join(first.Path, "tracked.txt"))
	if err != nil || string(data) != "base\n" {
		t.Fatalf("worktree omitted committed base: %q %v", data, err)
	}
	second, err := Ensure(ctx, root, "room_test", first.BaseRef)
	if err != nil || second.Path != first.Path {
		t.Fatalf("retry=%+v %v", second, err)
	}
	exclude, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if err != nil || strings.Count(string(exclude), ".aimebu/\n") != 1 {
		t.Fatalf("exclude=%q %v", exclude, err)
	}
	if err := os.WriteFile(filepath.Join(first.Path, "untracked.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	kept, err := Remove(ctx, root, "room_test", first.BaseRef, false)
	if err != nil || kept.State != "preserved" {
		t.Fatalf("dirty cleanup=%+v %v", kept, err)
	}
	if err := os.Remove(filepath.Join(first.Path, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove(ctx, root, "room_test", first.BaseRef, false)
	if err != nil || removed.State != "removed" {
		t.Fatalf("clean cleanup=%+v %v", removed, err)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("path after remove: %v", err)
	}
	restored, err := Ensure(ctx, root, "room_test", first.BaseRef)
	if err != nil || restored.Path != first.Path {
		t.Fatalf("restore=%+v %v", restored, err)
	}
}

func TestAdapterFilesDoNotPreventCleanWorktreeRemoval(t *testing.T) {
	root := repoFixture(t)
	item, err := Ensure(t.Context(), root, "room_adapter", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(item.Path, ".aimebu", "codex-schemas"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Path, ".aimebu", "codex-schemas", "result.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove(t.Context(), root, item.Branch[len("aimebu/"):], item.BaseRef, false)
	if err != nil || removed.State != "removed" {
		t.Fatalf("remove=%+v %v", removed, err)
	}
}

func TestEnsureCreatesMissingGitInfoDirectory(t *testing.T) {
	root := repoFixture(t)
	if err := os.RemoveAll(filepath.Join(root, ".git", "info")); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(t.Context(), root, "room_missing_info", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPreserveUnmergedCommitsAndRejectOccupiedPath(t *testing.T) {
	root := repoFixture(t)
	ctx := context.Background()
	item, err := Ensure(ctx, root, "room_other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Path, "tracked.txt"), []byte("room\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-qm", "room"}} {
		if output, err := exec.Command("git", append([]string{"-C", item.Path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	kept, err := Remove(ctx, root, "room_other", item.BaseRef, false)
	if err != nil || kept.State != "preserved" || kept.Reason != "room branch has unmerged commits" {
		t.Fatalf("unmerged=%+v %v", kept, err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aimebu", "worktrees", "room_occupied"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(ctx, root, "room_occupied", ""); err == nil {
		t.Fatal("occupied path accepted")
	}
	if err := os.Symlink(item.Path, filepath.Join(root, ".aimebu", "worktrees", "room_symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(ctx, root, "room_symlink", item.BaseRef, true); err == nil {
		t.Fatal("symlink path accepted for removal")
	}
}

func TestDirtyBaseCountsRenameAndExcludesLocalBusFiles(t *testing.T) {
	root := repoFixture(t)
	if err := os.MkdirAll(filepath.Join(root, ".aimebu"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aimebu", "local.txt"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", root, "mv", "tracked.txt", "renamed.txt").CombinedOutput(); err != nil {
		t.Fatalf("rename: %v %s", err, output)
	}
	item, err := Ensure(t.Context(), root, "room_rename", "")
	if err != nil || item.DirtyBase != 1 {
		t.Fatalf("renamed base count=%+v err=%v", item, err)
	}
}

// Git refuses a folder for reasons other than "not a repository" (it does not trust the owner, it cannot read it); the answer says what
// git said, so the owner does not go looking for a repository that is there.
func TestNotUsableSaysWhatGitSaid(t *testing.T) {
	cases := map[string]struct{ code, contains string }{
		"git rev-parse: exit status 128: fatal: not a git repository (or any of the parent directories): .git": {"not_git_repo", "not a Git repository"},
		"git rev-parse: exit status 128: fatal: detected dubious ownership in repository at '/srv/repo'":       {"git_unusable", "detected dubious ownership in repository at '/srv/repo'"},
		"git rev-parse: exit status 128: fatal: cannot change to '/srv/repo': Permission denied":               {"git_unusable", "cannot change to '/srv/repo': Permission denied"},
	}
	for text, want := range cases {
		err := notUsable(errors.New(text))
		var problem *Error
		if !errors.As(err, &problem) || problem.Code != want.code || !strings.Contains(problem.Error(), want.contains) {
			t.Errorf("%q -> %v", text, err)
		}
	}
	if err := notUsable(fmt.Errorf("git rev-parse: %w", exec.ErrNotFound)); !strings.Contains(err.Error(), "not installed") {
		t.Errorf("no git: %v", err)
	}
}

// A removed worktree's branch is set aside under a name of its own, so a worktree made later under the same name starts from the base, and the work is
// still on the old branch.
func TestRetireSetsTheKeptBranchAside(t *testing.T) {
	root := repoFixture(t)
	ctx := context.Background()
	item, err := Ensure(ctx, root, "room_a-ada", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item.Path, "tracked.txt"), []byte("ada\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-qm", "ada"}} {
		if output, err := exec.Command("git", append([]string{"-C", item.Path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	if _, err := Retire(ctx, root, "room_a-ada"); err == nil {
		t.Fatal("a branch with a worktree standing was set aside")
	}
	if _, err := Remove(ctx, root, "room_a-ada", item.BaseRef, true); err != nil {
		t.Fatal(err)
	}
	first, err := Retire(ctx, root, "room_a-ada")
	if err != nil || first.Branch != "aimebu/room_a-ada-removed-1" || first.State != "removed" {
		t.Fatalf("retire = %+v %v", first, err)
	}
	if again, err := Retire(ctx, root, "room_a-ada"); err != nil || again.Branch != "" {
		t.Fatalf("a second call sets nothing aside: %+v %v", again, err)
	}
	// The work is on the old branch; the name is free and a new worktree under it starts from the base.
	if out, err := exec.Command("git", "-C", root, "log", "--format=%s", "-1", "aimebu/room_a-ada-removed-1").Output(); err != nil || strings.TrimSpace(string(out)) != "ada" {
		t.Fatalf("the work is on the set-aside branch: %q %v", out, err)
	}
	fresh, err := Ensure(ctx, root, "room_a-ada", item.BaseRef)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(fresh.Path, "tracked.txt")); err != nil || string(data) != "base\n" {
		t.Fatalf("a new worktree under the same name starts from the base: %q %v", data, err)
	}
	// A second agent with the same name that is removed in turn gets the next number.
	if _, err := Remove(ctx, root, "room_a-ada", item.BaseRef, true); err != nil {
		t.Fatal(err)
	}
	if second, err := Retire(ctx, root, "room_a-ada"); err != nil || second.Branch != "aimebu/room_a-ada-removed-2" {
		t.Fatalf("second retire = %+v %v", second, err)
	}
	if none, err := Retire(ctx, root, "room_never"); err != nil || none.Branch != "" {
		t.Fatalf("no branch, nothing to do: %+v %v", none, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Work in a submodule or an embedded repository is invisible to a status that never goes into one, and git refuses to remove a worktree with a
// populated submodule, so Remove keeps such a worktree instead of failing; no program the repositories name may run while it checks.
func TestRemovePreservesWorktreeWithNestedRepository(t *testing.T) {
	root := repoFixture(t)
	marks := plantRepoPrograms(t, root)
	ctx := t.Context()
	// The test's own plain git calls may run the planted programs; only the ones made while Remove checks count.
	reset := func() {
		t.Helper()
		for _, name := range planted(t, marks) {
			if err := os.Remove(filepath.Join(marks, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	expectKept := func(room, baseRef, reason string) {
		t.Helper()
		reset()
		kept, err := Remove(ctx, root, room, baseRef, false)
		if err != nil || kept.State != "preserved" || kept.Reason != reason {
			t.Fatalf("%s: %+v %v, want preserved %q", room, kept, err, reason)
		}
		if got := planted(t, marks); len(got) != 0 {
			t.Fatalf("%s: the check ran planted programs: %v", room, got)
		}
	}
	embeddedRepo := func(dir string) {
		t.Helper()
		runGit(t, filepath.Dir(dir), "init", "-q", dir)
		runGit(t, dir, "config", "user.name", "Test")
		runGit(t, dir, "config", "user.email", "test@example.invalid")
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "f.txt")
		runGit(t, dir, "commit", "-qm", "inner")
		filter := filepath.Join(t.TempDir(), "filter.sh")
		if err := testenv.WriteExecutable(filter, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "inner-filter")+"'\ncat\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "config", "filter.x.clean", filter)
		if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("two\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// An untracked embedded repository is one untracked directory to the status, so the dirty check keeps the worktree before nestedRepos
	// is reached; nestedRepos itself does not look there.
	embedded, err := Ensure(ctx, root, "room_embedded", "")
	if err != nil {
		t.Fatal(err)
	}
	embeddedRepo(filepath.Join(embedded.Path, "inner"))
	expectKept("room_embedded", embedded.BaseRef, "1 uncommitted or generated files")
	if why, err := nestedRepos(ctx, embedded.Path); err != nil || why != "" {
		t.Fatalf("nestedRepos outside .aimebu/=%q %v", why, err)
	}

	// Ignored files under .aimebu/ do not count as dirty, so an embedded repository there is found by the walk.
	adapter, err := Ensure(ctx, root, "room_adapter_repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(adapter.Path, ".aimebu", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	embeddedRepo(filepath.Join(adapter.Path, ".aimebu", "tool", "inner"))
	reset()
	if why, err := nestedRepos(ctx, adapter.Path); err != nil || why != "contains an embedded repository" {
		t.Fatalf("nestedRepos under .aimebu/=%q %v", why, err)
	}
	expectKept("room_adapter_repo", adapter.BaseRef, "contains an embedded repository")

	// A staged gitlink the base does not have.
	sub, err := Ensure(ctx, root, "room_submodule", "")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, sub.Path, "update-index", "--add", "--cacheinfo", "160000,"+runGit(t, root, "rev-parse", "HEAD")+",vendor/lib")
	expectKept("room_submodule", sub.BaseRef, "contains a changed submodule")

	// A forced removal is the owner's explicit choice: git removes a worktree with nested repositories when forced.
	for _, room := range []string{"room_embedded", "room_adapter_repo", "room_submodule"} {
		forced, err := Remove(ctx, root, room, "", true)
		if err != nil || forced.State != "removed" {
			t.Fatalf("forced removal of %s: %+v %v", room, forced, err)
		}
		if _, err := os.Stat(forced.Path); !os.IsNotExist(err) {
			t.Fatalf("%s still there after a forced removal: %v", room, err)
		}
	}

	clean, err := Ensure(ctx, root, "room_clean_nested", "")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := Remove(ctx, root, "room_clean_nested", clean.BaseRef, false)
	if err != nil || removed.State != "removed" {
		t.Fatalf("clean worktree: %+v %v", removed, err)
	}
}

// A repository that commits a submodule gets worktrees whose submodule is not populated (`git worktree add` never populates one): such a
// clean, merged worktree is removed. One whose submodule is populated, or whose gitlink differs from the commit, is kept.
func TestRemoveWithCommittedSubmodule(t *testing.T) {
	root := repoFixture(t)
	ctx := t.Context()
	// The commit a submodule add would make, without the clone (the tests' PATH has no transport helpers).
	lib := repoFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("[submodule \"lib\"]\n\tpath = lib\n\turl = "+lib+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".gitmodules")
	runGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+runGit(t, lib, "rev-parse", "HEAD")+",lib")
	runGit(t, root, "commit", "-qm", "add lib")
	marks := plantRepoPrograms(t, root)

	fresh, err := Ensure(ctx, root, "room_fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(fresh.Path, "lib")); err != nil || len(entries) != 0 {
		t.Fatalf("a fresh worktree has an empty submodule folder: %v %v", entries, err)
	}
	if why, err := nestedRepos(ctx, fresh.Path); err != nil || why != "" {
		t.Fatalf("nestedRepos on a fresh worktree=%q %v", why, err)
	}
	for _, name := range planted(t, marks) {
		if err := os.Remove(filepath.Join(marks, name)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := Remove(ctx, root, "room_fresh", fresh.BaseRef, false)
	if err != nil || removed.State != "removed" {
		t.Fatalf("clean worktree of a repository with a submodule: %+v %v", removed, err)
	}
	if got := planted(t, marks); len(got) != 0 {
		t.Fatalf("the check ran planted programs: %v", got)
	}
	if _, err := os.Stat(removed.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}

	populated, err := Ensure(ctx, root, "room_populated", "")
	if err != nil {
		t.Fatal(err)
	}
	// What a submodule update leaves: a repository in the gitlink's folder.
	runGit(t, populated.Path, "init", "-q", "lib")
	kept, err := Remove(ctx, root, "room_populated", populated.BaseRef, false)
	if err != nil || kept.State != "preserved" || kept.Reason != "contains a checked-out submodule" {
		t.Fatalf("populated submodule: %+v %v", kept, err)
	}

	moved, err := Ensure(ctx, root, "room_moved", "")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, moved.Path, "update-index", "--cacheinfo", "160000,"+runGit(t, root, "rev-parse", "HEAD")+",lib")
	kept, err = Remove(ctx, root, "room_moved", moved.BaseRef, false)
	if err != nil || kept.State != "preserved" || kept.Reason != "contains a changed submodule" {
		t.Fatalf("staged gitlink change: %+v %v", kept, err)
	}

	gone, err := Ensure(ctx, root, "room_gone", "")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, gone.Path, "update-index", "--force-remove", "lib")
	kept, err = Remove(ctx, root, "room_gone", gone.BaseRef, false)
	if err != nil || kept.State != "preserved" || kept.Reason != "contains a changed submodule" {
		t.Fatalf("staged gitlink removal: %+v %v", kept, err)
	}

	for _, room := range []string{"room_populated", "room_moved", "room_gone"} {
		forced, err := Remove(ctx, root, room, "", true)
		if err != nil || forced.State != "removed" {
			t.Fatalf("forced removal of %s: %+v %v", room, forced, err)
		}
		if _, err := os.Stat(forced.Path); !os.IsNotExist(err) {
			t.Fatalf("%s still there after a forced removal: %v", room, err)
		}
	}
}
