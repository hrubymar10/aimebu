package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalDirectoryResolvesAliasAndRejectsMissingPath(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "project")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalDirectory(alias + "/.")
	if err != nil || got != real {
		t.Fatalf("canonical directory = %q, %v; want %q", got, err, real)
	}
	if _, err := canonicalDirectory(filepath.Join(root, "absent")); err == nil {
		t.Fatal("missing checkout was accepted")
	}
	if _, err := canonicalDirectory("relative"); err == nil {
		t.Fatal("relative checkout was accepted")
	}
}
