package pathlist

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"odd-one", "odd-two", ".odd-hidden"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "odd-file"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "odd-one"), filepath.Join(root, "odd-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "odd-file"), filepath.Join(root, "odd-file-link")); err != nil {
		t.Fatal(err)
	}
	if got := Directories(root, "odd"); len(got) != 3 || got[0] != "odd-link" || got[1] != "odd-one" || got[2] != "odd-two" {
		t.Fatalf("directories: %q", got)
	}
	if got := Directories(root, "."); len(got) != 1 || got[0] != ".odd-hidden" {
		t.Fatalf("hidden: %q", got)
	}
	if got := Directories("relative", ""); len(got) != 0 {
		t.Fatalf("relative: %q", got)
	}
	if got := Directories(filepath.Join(root, "absent"), ""); len(got) != 0 {
		t.Fatalf("absent: %q", got)
	}
	for i := 0; i < 205; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("many-%03d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if got := Directories(root, "many-"); len(got) != 200 {
		t.Fatalf("limit: %d", len(got))
	}
}
