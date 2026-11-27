package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupLeavesRotatedLogsOut(t *testing.T) {
	root, archive := backupFixture(t)
	logs := []string{
		"server/aimebu.log", "server/aimebu.log.1", "server/aimebu.log.2",
		"supervisor/agent-logs/abc/stderr.log", "supervisor/agent-logs/abc/stderr.log.1",
		"supervisor/agent-logs/abc/conversation.log.3",
	}
	kept := []string{"config/audit.log.txt", "config/notes.log.old", "config/x.logo", "config/dir.log.1x"}
	for _, p := range append(append([]string{}, logs...), kept...) {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte("log"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, errOut := runBackup(t, "--out", archive); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	paths := archivePaths(t, archive)
	have := map[string]bool{}
	for _, p := range paths {
		have[p] = true
	}
	for _, p := range logs {
		if have[p] {
			t.Fatalf("%s archived", p)
		}
	}
	for _, p := range kept {
		if !have[p] {
			t.Fatalf("%s should still be archived", p)
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "supervisor")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := restoreArchive(context.Background(), root, archive, true, &out); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "server", "aimebu.sqlite")); err != nil {
		t.Fatal(err)
	}
}
