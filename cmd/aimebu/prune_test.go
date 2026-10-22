package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/store"
)

func TestPlainPruneKeepsServerDataAndMachineKeys(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	serverDir := filepath.Join(root, "server")
	if err := os.MkdirAll(serverDir, 0700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(serverDir, "aimebu.sqlite")
	db, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writePruneFixture(t, filepath.Join(serverDir, "host-machine.key"), "host key")
	writePruneFixture(t, filepath.Join(serverDir, "attachments", "image"), "image")
	writePruneFixture(t, filepath.Join(root, "supervisor", "machine.key"), "remote key")
	writePruneFixture(t, filepath.Join(root, "supervisor", "journal.sqlite"), "journal")
	writePruneFixture(t, filepath.Join(root, "supervisor", "agent-tools", "agent.json"), "agent key")
	var out, errOut bytes.Buffer
	if got := pruneCommand(context.Background(), []string{"-y"}, strings.NewReader(""), &out, &errOut); got != 0 {
		t.Fatalf("plain prune = %d, stderr=%q", got, errOut.String())
	}
	for _, path := range []string{dbPath, filepath.Join(serverDir, "host-machine.key"), filepath.Join(serverDir, "attachments", "image"), filepath.Join(root, "supervisor", "machine.key")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("preserved %s: %v", path, err)
		}
	}
	for _, path := range []string{filepath.Join(root, "supervisor", "journal.sqlite"), filepath.Join(root, "supervisor", "agent-tools")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("removed %s: %v", path, err)
		}
	}
}

func TestPruneAllPreservesCredentialsAndOutsidePaths(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "config")
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	outside := filepath.Join(parent, "outside")
	writePruneFixture(t, outside, "outside")
	writePruneFixture(t, filepath.Join(root, "switcher", "state.json"), "switcher")
	writePruneFixture(t, filepath.Join(root, "usages", "config.json"), "usages")
	writePruneFixture(t, filepath.Join(root, "server", "backups", "aimebu-pre-migrate-1-to-2-20261020T000000.000000000Z.sqlite"), "backup")
	writePruneFixture(t, filepath.Join(root, "server", "aimebu.sqlite"), "server")
	writePruneFixture(t, filepath.Join(root, "server", "aimebu.log"), "log")
	writePruneFixture(t, filepath.Join(root, "server", "aimebu.log.1"), "older log")
	writePruneFixture(t, filepath.Join(root, "supervisor", "machine.key"), "machine")
	writePruneFixture(t, filepath.Join(root, "unknown.txt"), "unknown")
	if err := os.Symlink(outside, filepath.Join(root, "server", "outside-link")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if got := pruneCommand(context.Background(), []string{"-a", "-y"}, strings.NewReader(""), &out, &errOut); got != 0 {
		t.Fatalf("prune -a = %d, stderr=%q", got, errOut.String())
	}
	for path, want := range map[string]string{
		outside: "outside", filepath.Join(root, "switcher", "state.json"): "switcher",
		filepath.Join(root, "usages", "config.json"): "usages", filepath.Join(root, "unknown.txt"): "unknown",
		filepath.Join(root, "server", "backups", "aimebu-pre-migrate-1-to-2-20261020T000000.000000000Z.sqlite"): "backup",
		filepath.Join(root, "server", "aimebu.log"):                                                             "log", filepath.Join(root, "server", "aimebu.log.1"): "older log",
	} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Errorf("preserved %s = %q, %v", path, data, err)
		}
	}
	for _, path := range []string{filepath.Join(root, "server", "aimebu.sqlite"), filepath.Join(root, "supervisor", "machine.key"), filepath.Join(root, "server", "outside-link")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("removed %s: %v", path, err)
		}
	}
}

func TestPruneRequiresConfirmationAndRefusesLiveLocks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	path := filepath.Join(root, "server", "marker")
	writePruneFixture(t, path, "keep")
	var out, errOut bytes.Buffer
	if got := pruneCommand(context.Background(), []string{"-a"}, strings.NewReader("n\n"), &out, &errOut); got != 0 {
		t.Fatalf("declined prune = %d: %s", got, errOut.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("declined prune changed files:", err)
	}
	lock, err := pruneLock(filepath.Join(root, "server", ".writer.lock"), "server")
	if err != nil {
		t.Fatal(err)
	}
	defer unlockServerDirectory(lock)
	out.Reset()
	errOut.Reset()
	if got := pruneCommand(context.Background(), []string{"-a", "-y"}, strings.NewReader(""), &out, &errOut); got != 1 || !strings.Contains(errOut.String(), "server is running") {
		t.Fatalf("live prune = %d, stderr=%q", got, errOut.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("live prune changed files:", err)
	}
}

func TestPruneRefusesLiveSupervisor(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	path := filepath.Join(root, "supervisor", "journal.sqlite")
	writePruneFixture(t, path, "journal")
	lock, err := pruneLock(filepath.Join(root, "supervisor", "journal.sqlite.lock"), "supervisor")
	if err != nil {
		t.Fatal(err)
	}
	defer unlockServerDirectory(lock)
	var out, errOut bytes.Buffer
	if got := pruneCommand(context.Background(), []string{"-a", "-y"}, strings.NewReader(""), &out, &errOut); got != 1 || !strings.Contains(errOut.String(), "supervisor is running") {
		t.Fatalf("live supervisor prune = %d, stderr=%q", got, errOut.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("live supervisor prune changed files:", err)
	}
}

func TestPruneRejectsSymlinkRootAndDataDir(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	writePruneFixture(t, filepath.Join(outside, "marker"), "keep")
	root := filepath.Join(parent, "link")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	var out, errOut bytes.Buffer
	if got := pruneCommand(context.Background(), []string{"-a", "-y"}, strings.NewReader(""), &out, &errOut); got != 1 {
		t.Fatalf("symlink root prune = %d", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "marker")); err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(parent, "real")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "server")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	if got := pruneCommand(context.Background(), []string{"-a", "-y"}, strings.NewReader(""), &out, &errOut); got != 1 {
		t.Fatalf("symlink data dir prune = %d", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "marker")); err != nil {
		t.Fatal(err)
	}
}

func TestPruneCreatesMissingLockDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	if err := pruneRoot(context.Background(), root, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "server", ".writer.lock"),
		filepath.Join(root, "supervisor", "journal.sqlite.lock"),
	} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("lock %s: %v", path, err)
		}
	}
}

func TestPruneDoesNotCreateRootThroughSymlink(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := pruneRoot(context.Background(), filepath.Join(link, "fresh"), false); err == nil {
		t.Fatal("prune accepted symlinked parent")
	}
	if _, err := os.Lstat(filepath.Join(outside, "fresh")); !os.IsNotExist(err) {
		t.Fatalf("prune created root through symlink: %v", err)
	}
}

func writePruneFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
