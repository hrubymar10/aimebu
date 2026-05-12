package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDaemonStatusUsesLegacyPIDWithoutMigratingStore(t *testing.T) {
	rootDir := t.TempDir()
	writeDaemonFile(t, filepath.Join(rootDir, "aimebu.pid"), strconv.Itoa(os.Getpid()))
	writeDaemonFile(t, filepath.Join(rootDir, "rooms.json"), `[{"id":"general"}]`)
	writeDaemonFile(t, filepath.Join(rootDir, "messages.json"), `[{"id":1}]`)

	running, pid, err := DaemonStatus(rootDir)
	if err != nil {
		t.Fatalf("DaemonStatus() error = %v", err)
	}
	if !running {
		t.Fatal("DaemonStatus() = not running, want running")
	}
	if pid != os.Getpid() {
		t.Fatalf("DaemonStatus() pid = %d, want %d", pid, os.Getpid())
	}

	for _, name := range []string{"rooms.json", "messages.json"} {
		data, err := os.ReadFile(filepath.Join(rootDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.TrimSpace(string(data)) == "" {
			t.Fatalf("%s was unexpectedly modified", name)
		}
	}
	if _, err := os.Stat(filepath.Join(rootDir, "server")); !os.IsNotExist(err) {
		t.Fatalf("server dir should not be created during status, got err=%v", err)
	}
}

func writeDaemonFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
