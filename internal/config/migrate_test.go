package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateServerMovesKnownFiles(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, filepath.Join(root, "rooms.json"), "[]")
	writeConfigFile(t, filepath.Join(root, "macros.json"), "{}")
	writeConfigFile(t, filepath.Join(root, "fleet.json"), "{}")
	writeConfigFile(t, filepath.Join(root, "notes.txt"), "leave me")

	if err := MigrateServer(root); err != nil {
		t.Fatalf("MigrateServer() error = %v", err)
	}

	assertConfigFile(t, filepath.Join(root, "server", "rooms.json"), "[]")
	assertConfigFile(t, filepath.Join(root, "server", "macros.json"), "{}")
	assertConfigFile(t, filepath.Join(root, "server", "fleet.json"), "{}")
	assertConfigFile(t, filepath.Join(root, "notes.txt"), "leave me")
	if _, err := os.Stat(filepath.Join(root, "rooms.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy rooms.json should be gone, err=%v", err)
	}
}

func TestMigrateServerMixedLayoutSkipsConflicts(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, filepath.Join(root, "messages.json"), "legacy")
	writeConfigFile(t, filepath.Join(root, "agents.json"), "legacy agents")
	writeConfigFile(t, filepath.Join(root, "server", "messages.json"), "new")

	if err := MigrateServer(root); err != nil {
		t.Fatalf("MigrateServer() error = %v", err)
	}

	assertConfigFile(t, filepath.Join(root, "server", "messages.json"), "new")
	assertConfigFile(t, filepath.Join(root, "messages.json"), "legacy")
	assertConfigFile(t, filepath.Join(root, "server", "agents.json"), "legacy agents")
}

func TestMigrateAgentsMovesKnownFiles(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, filepath.Join(root, "agent-sessions.json"), "[]")
	writeConfigFile(t, filepath.Join(root, "agent-warning-acknowledged"), "yes")

	if err := MigrateAgents(root); err != nil {
		t.Fatalf("MigrateAgents() error = %v", err)
	}

	assertConfigFile(t, filepath.Join(root, "agents", "agent-sessions.json"), "[]")
	assertConfigFile(t, filepath.Join(root, "agents", "agent-warning-acknowledged"), "yes")
}

func writeConfigFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertConfigFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, string(data), want)
	}
}
