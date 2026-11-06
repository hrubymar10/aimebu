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
	if err := os.WriteFile(filepath.Join(dir, idA+".jsonl"), []byte("continued\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(cfg, idA, to); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, idA+".jsonl")); string(again) != "continued\n" {
		t.Fatal("a copy made before must not be overwritten")
	}
	if _, err := Copy(cfg, idB, to); err != ErrNotFound {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := Copy(cfg, idA, "relative/dir"); err == nil {
		t.Fatal("a relative target must be refused")
	}
}
