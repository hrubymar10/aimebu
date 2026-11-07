package usages

import (
	"os"
	"path/filepath"
	"testing"
)

// The usages folder holds credentials and tokens, so it is the person's alone, also when an earlier version made it open to others.
func TestUsagesFolderIsPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usages")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireNamedLock(root, ".test.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("an existing folder: %v %v", info, err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh", "usages")
	lock2, err := acquireNamedLock(fresh, ".test.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock2.Unlock()
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("a new folder: %v %v", info, err)
	}
}
