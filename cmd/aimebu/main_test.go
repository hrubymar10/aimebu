package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/client"
)

func TestPruneViaServerOrLocalFallsBackOffline(t *testing.T) {
	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(dataDir, "rooms.json"), `[{"id":"general","members":["martin"]}]`)
	writeTestFile(t, filepath.Join(dataDir, "messages.json"), `[{"id":1,"room_id":"general","sender_id":"martin","body":"hi"}]`)
	writeTestFile(t, filepath.Join(dataDir, "agents.json"), `[{"id":"martin","name":"martin"}]`)
	writeTestFile(t, filepath.Join(dataDir, "agent-sessions.json"), `[{"session_id":"s1","name":"alice"}]`)
	writeTestFile(t, filepath.Join(dataDir, "macros.json"), `{"macros":{"zzz_custom_only_for_test":"body"}}`)

	result, err := pruneViaServerOrLocal(&client.Client{BaseURL: unusedLoopbackURL(t)}, dataDir, true)
	if err != nil {
		t.Fatalf("pruneViaServerOrLocal returned error: %v", err)
	}
	if !strings.Contains(result, `"mode":"offline"`) {
		t.Fatalf("expected offline result, got %s", result)
	}
	if err := pruneLocalSidecars(dataDir); err != nil {
		t.Fatalf("pruneLocalSidecars returned error: %v", err)
	}

	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "rooms.json", want: "[]"},
		{name: "messages.json", want: "null"},
		{name: "agents.json", want: "[]"},
	} {
		data, err := os.ReadFile(filepath.Join(dataDir, tc.name))
		if err != nil {
			t.Fatalf("read %s: %v", tc.name, err)
		}
		if strings.TrimSpace(string(data)) != tc.want {
			t.Fatalf("%s = %s, want %s", tc.name, strings.TrimSpace(string(data)), tc.want)
		}
	}

	if _, err := os.Stat(filepath.Join(dataDir, "agent-sessions.json")); !os.IsNotExist(err) {
		t.Fatalf("agent-sessions.json should be removed, got err=%v", err)
	}

	macrosData, err := os.ReadFile(filepath.Join(dataDir, "macros.json"))
	if err != nil {
		t.Fatalf("read macros.json: %v", err)
	}
	if strings.Contains(string(macrosData), "zzz_custom_only_for_test") {
		t.Fatalf("macros.json still contains custom macro after prune -a: %s", macrosData)
	}
	if !strings.Contains(string(macrosData), `"macros"`) {
		t.Fatalf("macros.json should remain a macros envelope, got %s", macrosData)
	}
}

func TestPruneCanUseOfflineFallback(t *testing.T) {
	cases := []struct {
		baseURL string
		want    bool
	}{
		{"http://localhost:9997", true},
		{"http://127.0.0.1:9997", true},
		{"http://[::1]:9997", true},
		{"http://host.docker.internal:9997", false},
		{"http://172.28.47.1:9997", false},
		{"://bad url", false},
	}
	for _, tc := range cases {
		if got := pruneCanUseOfflineFallback(tc.baseURL); got != tc.want {
			t.Errorf("pruneCanUseOfflineFallback(%q) = %v, want %v", tc.baseURL, got, tc.want)
		}
	}
}

func unusedLoopbackURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
