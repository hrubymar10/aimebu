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

func TestDaemonStatusFallsBackToLiveLegacyPIDWhenServerPIDIsStale(t *testing.T) {
	rootDir := t.TempDir()
	serverPIDPath := filepath.Join(rootDir, "server", "aimebu.pid")
	legacyPIDPath := filepath.Join(rootDir, "aimebu.pid")

	writeDaemonFile(t, serverPIDPath, "999999")
	writeDaemonFile(t, legacyPIDPath, strconv.Itoa(os.Getpid()))

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
	if _, err := os.Stat(serverPIDPath); !os.IsNotExist(err) {
		t.Fatalf("stale server pid file should be removed, got err=%v", err)
	}
	if _, err := os.Stat(legacyPIDPath); err != nil {
		t.Fatalf("legacy pid file should remain, got err=%v", err)
	}
}

func TestDaemonInsecureSkipVerifyEnabled(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"1", true},
		{"true", true},
		{"YES", true},
		{" on ", true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("AIMEBU_INSECURE_SKIP_VERIFY", tc.raw)
			if got := daemonInsecureSkipVerifyEnabled(); got != tc.want {
				t.Fatalf("daemonInsecureSkipVerifyEnabled() = %v, want %v", got, tc.want)
			}
		})
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
