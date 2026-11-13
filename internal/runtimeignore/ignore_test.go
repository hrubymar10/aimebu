package runtimeignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCreatesIgnoreAndPreservesExistingFile(t *testing.T) {
	root := t.TempDir()
	if err := Ensure(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".aimebu", ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "*\n" {
		t.Fatalf("ignore = %q, %v", data, err)
	}
	if err := os.WriteFile(path, []byte("custom\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(root); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "custom\n" {
		t.Fatalf("existing ignore = %q, %v", data, err)
	}
}
