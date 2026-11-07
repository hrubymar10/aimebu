package ccsessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyPutsTheSessionWhereClaudeLooksForItInAnotherFolder(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	original := transcript(t, cfg, "/work/proj", idA)
	// A session's own directory (tool results, subagent transcripts) comes along.
	nested := filepath.Join(filepath.Dir(original), idA, "subagents")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "agent-x.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	to := "/work/proj/.aimebu/worktrees/room_a-bert"
	dir, err := Copy(cfg, idA, to)
	if err != nil || dir != filepath.Join(cfg, "projects", FolderName(to)) {
		t.Fatalf("dir=%q err=%v", dir, err)
	}
	copied, err := os.ReadFile(filepath.Join(dir, idA+".jsonl"))
	want, _ := os.ReadFile(original)
	if err != nil || string(copied) != string(want) {
		t.Fatalf("the transcript should be copied as it is: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, idA, "subagents", "agent-x.jsonl")); err != nil {
		t.Fatalf("the session's directory should come along: %v", err)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("the original stays: %v", err)
	}
	// A second copy leaves the first alone, and an id that is not a session, or a relative target, is refused.
	// (A copy that was carried on, as Claude does when it resumes it, is the original and more.)
	if err := os.WriteFile(filepath.Join(dir, idA+".jsonl"), append(append([]byte(nil), want...), "continued\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(cfg, idA, to); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, idA+".jsonl")); string(again) != string(want)+"continued\n" {
		t.Fatal("a copy made before must not be overwritten")
	}
	if _, err := Copy(cfg, idB, to); err != ErrNotFound {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := Copy(cfg, idA, "relative/dir"); err == nil {
		t.Fatal("a relative target must be refused")
	}
}

// The copy is written where nothing planted can redirect it: a link where the folder, a file or the leftover of an earlier copy should be is
// refused or never followed, and a copy made before is reused only when it agrees with the original.
func TestCopyDoesNotFollowLinksAndChecksAReusedCopy(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	original := transcript(t, cfg, "/work/proj", idA)
	want, _ := os.ReadFile(original)
	to := "/work/other"
	dir := filepath.Join(cfg, "projects", FolderName(to))
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A link where the folder of the copy should be.
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(cfg, idA, to); err == nil {
		t.Fatal("a copy into a linked folder must be refused")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("something was written through the link: %v", entries)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}

	// A link where the leftover of an interrupted copy would be: it is replaced, not written through.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, idA+".jsonl.partial")); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(cfg, idA, to); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "untouched" {
		t.Fatalf("the transcript was written through a planted link: %q", data)
	}
	if copied, _ := os.ReadFile(filepath.Join(dir, idA+".jsonl")); string(copied) != string(want) {
		t.Fatalf("the copy is %q", copied)
	}

	// A link where the copy itself is, and a copy that is not of this session, are not reused.
	if err := os.Remove(filepath.Join(dir, idA+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, idA+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(cfg, idA, to); err == nil {
		t.Fatal("a link where the copy should be must be refused")
	}
	if err := os.Remove(filepath.Join(dir, idA+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, idA+".jsonl"), []byte("something else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(cfg, idA, to); err == nil {
		t.Fatal("a copy that is not of this session must not be reused")
	}
}
