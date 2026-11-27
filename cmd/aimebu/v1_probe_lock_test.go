package main

import (
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
