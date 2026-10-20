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
