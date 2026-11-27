package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIsV1RootLeavesNoLockFile(t *testing.T) {
	root := v1RootFixture(t)
	if v1, err := isV1Root(root); err != nil || !v1 {
		t.Fatalf("isV1Root = %v, %v", v1, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "server", ".writer.lock")); !os.IsNotExist(err) {
		t.Fatalf("a v1 check left server/.writer.lock behind: %v", err)
	}
}

func TestPruneRecheckSeesV1RootWhileHoldingTheLock(t *testing.T) {
	root := v1RootFixture(t)
	lock, err := pruneLock(filepath.Join(root, "server", ".writer.lock"), "server")
	if err != nil {
		t.Fatal(err)
	}
	defer unlockServerDirectory(lock)
	if err := refuseV1RootWriteHeld(root); !errors.Is(err, errV1RootWrite) {
		t.Fatalf("recheck under the held lock = %v, want the v1 refusal", err)
	}
}

// The probe opens a database only under a lock it holds (immutable, so no -shm or -wal appears next to a v2 file), or where no lock file
// exists at all (a pure v1 root, plain read-only open).
func TestV1ProbeOpensOnlyUnderItsOwnLockOrWithNoLockFile(t *testing.T) {
	var plain, locked int
	previousPlain, previousLocked := v1DatabaseOpener, v1LockedDatabaseOpener
	v1DatabaseOpener = func(path string) (*sql.DB, error) { plain++; return previousPlain(path) }
	v1LockedDatabaseOpener = func(path string) (*sql.DB, error) { locked++; return previousLocked(path) }
	defer func() { v1DatabaseOpener, v1LockedDatabaseOpener = previousPlain, previousLocked }()

	v1 := v1RootFixture(t)
	if got, err := isV1Root(v1); err != nil || !got {
		t.Fatalf("v1 root: %v, %v", got, err)
	}
	if plain != 1 || locked != 0 {
		t.Fatalf("a root with no lock file: plain=%d locked=%d, want 1 and 0", plain, locked)
	}

	v2 := t.TempDir()
	path := filepath.Join(v2, "server", "aimebu.sqlite")
	createFixtureDatabase(t, path)
	if err := os.WriteFile(filepath.Join(v2, "server", ".writer.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, v2)
	plain, locked = 0, 0
	if got, err := isV1Root(v2); err != nil || got {
		t.Fatalf("v2 root: %v, %v", got, err)
	}
	if plain != 0 || locked != 1 {
		t.Fatalf("a root with a lock file: plain=%d locked=%d, want 0 and 1", plain, locked)
	}
	requireTreeUnchanged(t, v2, before)
}
