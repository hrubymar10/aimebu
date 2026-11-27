package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeBackupName(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// keepFixture puts four timestamped archives in the default folder whose mtimes run opposite to their names.
func keepFixture(t *testing.T) (root string, names []string) {
	root, _ = backupFixture(t)
	dir := filepath.Join(root, "backups")
	base := time.Now().Add(-time.Hour)
	for i, stamp := range []string{"20200101T000000.000000000Z", "20200102T000000.000000000Z", "20200103T000000.000000000Z", "20200104T000000.000000000Z"} {
		name := "aimebu-v2.0.0-" + stamp + ".tar.gz"
		// Oldest name gets the newest mtime: only the name ordering keeps the right files.
		writeBackupName(t, dir, name, base.Add(-time.Duration(i)*time.Minute))
		names = append(names, name)
	}
	return root, names
}

func runBackup(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := backupCommand(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestBackupKeepDeletesAllButNewest(t *testing.T) {
	root, names := keepFixture(t)
	dir := filepath.Join(root, "backups")
	foreign := []string{"older.tar.gz", "aimebu-v2-x.tar.gz.tmp", "aimebu-note.txt", ".tmp-aimebu-1.tar.gz"}
	for _, n := range foreign {
		writeBackupName(t, dir, n, time.Now().Add(-48*time.Hour))
	}
	if err := os.Mkdir(filepath.Join(dir, "aimebu-dir.tar.gz"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "aimebu-link.tar.gz")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runBackup(t, "--keep", "2")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// New archive + the newest by name stay; the rest go.
	for _, n := range names[:2] {
		if exists(filepath.Join(dir, n)) {
			t.Fatalf("%s should be deleted", n)
		}
		if !strings.Contains(out, n) {
			t.Fatalf("deleted file %s not printed: %s", n, out)
		}
	}
	if !exists(filepath.Join(dir, names[3])) {
		t.Fatalf("%s should stay", names[3])
	}
	if exists(filepath.Join(dir, names[2])) {
		t.Fatalf("%s should be deleted: new archive plus one old stay for --keep 2", names[2])
	}
	for _, n := range append(foreign, "aimebu-dir.tar.gz", "aimebu-link.tar.gz") {
		if !exists(filepath.Join(dir, n)) {
			t.Fatalf("foreign %s was touched", n)
		}
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "keep me" {
		t.Fatalf("symlink target touched: %q %v", b, err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "aimebu-*.tar.gz"))
	count := 0
	for _, m := range matches {
		if info, err := os.Lstat(m); err == nil && info.Mode().IsRegular() {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("regular archives left = %d, want 2", count)
	}
}

func TestBackupWithoutKeepDeletesNothing(t *testing.T) {
	root, names := keepFixture(t)
	if code, _, errOut := runBackup(t); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, n := range names {
		if !exists(filepath.Join(root, "backups", n)) {
			t.Fatalf("%s deleted without --keep", n)
		}
	}
}

func TestBackupKeepFallsBackToMtime(t *testing.T) {
	root, _ := backupFixture(t)
	dir := filepath.Join(root, "backups")
	now := time.Now()
	writeBackupName(t, dir, "aimebu-odd-a.tar.gz", now.Add(-3*time.Hour))
	writeBackupName(t, dir, "aimebu-odd-b.tar.gz", now.Add(-2*time.Hour))
	if code, _, errOut := runBackup(t, "--keep", "2"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if exists(filepath.Join(dir, "aimebu-odd-a.tar.gz")) || !exists(filepath.Join(dir, "aimebu-odd-b.tar.gz")) {
		t.Fatal("mtime fallback kept the wrong file")
	}
}

func TestBackupFailureDeletesNothing(t *testing.T) {
	root, names := keepFixture(t)
	// A symlink in the data root fails the backup after the archive was started.
	if err := os.Symlink("/nonexistent", filepath.Join(root, "server", "bad-link")); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runBackup(t, "--keep", "1")
	if code == 0 {
		t.Fatal("backup should fail")
	}
	for _, n := range names {
		if !exists(filepath.Join(root, "backups", n)) {
			t.Fatalf("%s deleted after a failed backup", n)
		}
	}
	// An early failure (existing output inside the folder) deletes nothing either.
	os.Remove(filepath.Join(root, "server", "bad-link"))
	existing := filepath.Join(root, "backups", names[0])
	if code, _, _ := runBackup(t, "--keep", "1", "--out", existing); code == 0 {
		t.Fatal("existing output should fail")
	}
	for _, n := range names {
		if !exists(filepath.Join(root, "backups", n)) {
			t.Fatalf("%s deleted after an early failure", n)
		}
	}
}

func TestBackupKeepIgnoredForOutsideOut(t *testing.T) {
	root, names := keepFixture(t)
	out := filepath.Join(t.TempDir(), "elsewhere.tar.gz")
	code, stdout, errOut := runBackup(t, "--keep", "1", "--out", out)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, n := range names {
		if !exists(filepath.Join(root, "backups", n)) {
			t.Fatalf("%s deleted for an outside --out", n)
		}
	}
	if !strings.Contains(stdout, "--keep") {
		t.Fatalf("no note that --keep was not applied: %s", stdout)
	}
}

func TestBackupKeepAppliesToOutInsideFolder(t *testing.T) {
	root, names := keepFixture(t)
	out := filepath.Join(root, "backups", "aimebu-custom-20300101T000000.000000000Z.tar.gz")
	if code, _, errOut := runBackup(t, "--keep", "1", "--out", out); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !exists(out) {
		t.Fatal("new archive deleted")
	}
	for _, n := range names {
		if exists(filepath.Join(root, "backups", n)) {
			t.Fatalf("%s should be deleted", n)
		}
	}
}

func TestBackupKeepRejectsBadValues(t *testing.T) {
	root, names := keepFixture(t)
	for _, v := range []string{"0", "-1", "abc", ""} {
		code, _, errOut := runBackup(t, "--keep", v)
		if code != 2 {
			t.Fatalf("--keep %q exit %d, want 2", v, code)
		}
		if v == "0" || v == "-1" {
			if !strings.Contains(errOut, "--keep must be 1 or more") {
				t.Fatalf("--keep %s message: %s", v, errOut)
			}
		}
	}
	for _, n := range names {
		if !exists(filepath.Join(root, "backups", n)) {
			t.Fatalf("%s deleted", n)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(root, "backups", "aimebu-*.tar.gz")); len(m) != len(names) {
		t.Fatalf("a backup was written despite a bad flag: %v", m)
	}
}
