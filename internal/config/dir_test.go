package config

import (
	"path/filepath"
	"testing"
)

func TestRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("default", func(t *testing.T) {
		t.Setenv(envConfigDir, "")
		want := filepath.Join(home, ".aimebu")
		if got := Root(); got != want {
			t.Fatalf("Root() = %q, want %q", got, want)
		}
		if got := ServerDir(); got != filepath.Join(want, "server") {
			t.Fatalf("ServerDir() = %q", got)
		}
		if got := AgentsDir(); got != filepath.Join(want, "agents") {
			t.Fatalf("AgentsDir() = %q", got)
		}
	})

	t.Run("env override", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "custom")
		t.Setenv(envConfigDir, root)
		if got := Root(); got != root {
			t.Fatalf("Root() = %q, want %q", got, root)
		}
		if got := ServerDir(); got != filepath.Join(root, "server") {
			t.Fatalf("ServerDir() = %q", got)
		}
		if got := AgentsDir(); got != filepath.Join(root, "agents") {
			t.Fatalf("AgentsDir() = %q", got)
		}
	})
}
