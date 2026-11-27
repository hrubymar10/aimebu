package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeV1Backup(t *testing.T, root string) {
	t.Helper()
	for p, body := range map[string]string{
		".old-v1-bkp/server/aimebu.sqlite":     "v1 db",
		".old-v1-bkp/server/attachments/a.bin": "v1 attachment",
		".old-v1-bkp/manifest.json":            "{}",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func archivePaths(t *testing.T, archive string) []string {
	t.Helper()
	manifest, err := extractVerified(archive, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range manifest.Files {
		paths = append(paths, f.Path)
	}
	return paths
}

func hasPrefix(paths []string, prefix string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func TestBackupExcludesV1BackupByDefault(t *testing.T) {
	root, archive := backupFixture(t)
	writeV1Backup(t, root)
	// A symlink inside would fail the walk if the folder were scanned.
	if err := os.Symlink("/nonexistent", filepath.Join(root, ".old-v1-bkp", "server", "link")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runBackup(t, "--out", archive)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if hasPrefix(archivePaths(t, archive), ".old-v1-bkp") {
		t.Fatal(".old-v1-bkp archived by default")
	}
	if !strings.Contains(out, ".old-v1-bkp") || !strings.Contains(out, "excluded") || !strings.Contains(out, "--include-v1-backup") {
		t.Fatalf("exclusion line missing: %s", out)
	}
}

func TestBackupIncludesV1BackupWithFlag(t *testing.T) {
	root, archive := backupFixture(t)
	writeV1Backup(t, root)
	code, out, errOut := runBackup(t, "--include-v1-backup", "--out", archive)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	paths := archivePaths(t, archive)
	for _, want := range []string{".old-v1-bkp/server/aimebu.sqlite", ".old-v1-bkp/server/attachments/a.bin", ".old-v1-bkp/manifest.json"} {
		if !hasPrefix(paths, want) {
			t.Fatalf("%s missing from %v", want, paths)
		}
	}
	if !strings.Contains(out, ".old-v1-bkp") || !strings.Contains(out, "included") || !strings.Contains(out, "without --include-v1-backup") {
		t.Fatalf("inclusion line missing: %s", out)
	}
}

func TestBackupSaysNothingWithoutV1Backup(t *testing.T) {
	_, archive := backupFixture(t)
	code, out, errOut := runBackup(t, "--out", archive)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, ".old-v1-bkp") {
		t.Fatalf("unexpected line: %s", out)
	}
}

func TestRestoreWorksWithAndWithoutV1Backup(t *testing.T) {
	for _, include := range []bool{false, true} {
		root, archive := backupFixture(t)
		writeV1Backup(t, root)
		args := []string{"--out", archive}
		if include {
			args = append([]string{"--include-v1-backup"}, args...)
		}
		if code, _, errOut := runBackup(t, args...); code != 0 {
			t.Fatalf("include=%v exit %d: %s", include, code, errOut)
		}
		if err := os.RemoveAll(filepath.Join(root, ".old-v1-bkp")); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := restoreArchive(context.Background(), root, archive, true, &out); err != nil {
			t.Fatalf("include=%v restore: %v", include, err)
		}
		_, err := os.Stat(filepath.Join(root, ".old-v1-bkp", "server", "attachments", "a.bin"))
		if include != (err == nil) {
			t.Fatalf("include=%v: v1 attachment present=%v", include, err == nil)
		}
		if _, err := os.Stat(filepath.Join(root, "server", "aimebu.sqlite")); err != nil {
			t.Fatalf("include=%v: v2 database missing: %v", include, err)
		}
	}
}
