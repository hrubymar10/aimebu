package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/client"
	"github.com/hrubymar10/aimebu/internal/config"
)

func TestPruneViaServerOrLocalFallsBackOffline(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", rootDir)
	serverDir := config.ServerDir()
	agentsDir := config.AgentsDir()
	writeTestFile(t, filepath.Join(serverDir, "schema.json"), `{"version":2}`)
	writeTestFile(t, filepath.Join(serverDir, "rooms.json"), `[{"id":"general","members":["martin"]}]`)
	writeTestFile(t, filepath.Join(serverDir, "messages.json"), `[{"id":1,"room_id":"general","sender_id":"martin","body":"hi"}]`)
	writeTestFile(t, filepath.Join(serverDir, "agents.json"), `[{"id":"martin","name":"martin"}]`)
	writeTestFile(t, filepath.Join(agentsDir, "agent-sessions.json"), `[{"session_id":"s1","name":"alice"}]`)
	writeTestFile(t, filepath.Join(agentsDir, "agent-warning-acknowledged"), "yes")
	writeTestFile(t, filepath.Join(serverDir, "macros.json"), `{"macros":{"zzz_custom_only_for_test":"body"}}`)

	result, err := pruneViaServerOrLocal(&client.Client{BaseURL: unusedLoopbackURL(t)}, rootDir, true)
	if err != nil {
		t.Fatalf("pruneViaServerOrLocal returned error: %v", err)
	}
	if !strings.Contains(result, `"mode":"offline"`) {
		t.Fatalf("expected offline result, got %s", result)
	}
	if err := pruneLocalSidecars(rootDir, true); err != nil {
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
		data, err := os.ReadFile(filepath.Join(serverDir, tc.name))
		if err != nil {
			t.Fatalf("read %s: %v", tc.name, err)
		}
		if strings.TrimSpace(string(data)) != tc.want {
			t.Fatalf("%s = %s, want %s", tc.name, strings.TrimSpace(string(data)), tc.want)
		}
	}

	if _, err := os.Stat(filepath.Join(agentsDir, "agent-sessions.json")); !os.IsNotExist(err) {
		t.Fatalf("agent-sessions.json should be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "agent-warning-acknowledged")); !os.IsNotExist(err) {
		t.Fatalf("agent-warning-acknowledged should be removed by prune -a, got err=%v", err)
	}

	macrosData, err := os.ReadFile(filepath.Join(serverDir, "macros.json"))
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

func TestPruneViaServerOrLocalDoesNotMigrateReachableLegacyServer(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", rootDir)
	writeTestFile(t, filepath.Join(rootDir, "rooms.json"), `[{"id":"general"}]`)
	writeTestFile(t, filepath.Join(rootDir, "messages.json"), `[{"id":1}]`)
	writeTestFile(t, filepath.Join(rootDir, "agents.json"), `[{"id":"martin"}]`)
	writeTestFile(t, filepath.Join(rootDir, "macros.json"), `{"macros":{"zzz_custom_only_for_test":"body"}}`)
	writeTestFile(t, filepath.Join(rootDir, "agent-sessions.json"), `[{"session_id":"s1","name":"alice"}]`)
	writeTestFile(t, filepath.Join(rootDir, "agent-warning-acknowledged"), "yes")

	sawDelete := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.RequestURI() == "/all?include_settings=true" {
			sawDelete = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"cleared","mode":"server"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	result, err := pruneViaServerOrLocal(&client.Client{BaseURL: srv.URL}, rootDir, true)
	if err != nil {
		t.Fatalf("pruneViaServerOrLocal returned error: %v", err)
	}
	if !strings.Contains(result, `"mode":"server"`) {
		t.Fatalf("expected server result, got %s", result)
	}
	if !sawDelete {
		t.Fatal("expected reachable server delete path to be used")
	}
	if err := pruneLocalSidecars(rootDir, true); err != nil {
		t.Fatalf("pruneLocalSidecars returned error: %v", err)
	}

	for _, name := range []string{"rooms.json", "messages.json", "agents.json", "macros.json"} {
		if _, err := os.Stat(filepath.Join(rootDir, name)); err != nil {
			t.Fatalf("legacy %s should stay at root on reachable prune, got err=%v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rootDir, "server")); !os.IsNotExist(err) {
		t.Fatalf("server dir should not be created on reachable prune, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "agent-sessions.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy agent-sessions.json should be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "agent-warning-acknowledged")); !os.IsNotExist(err) {
		t.Fatalf("legacy agent-warning-acknowledged should be removed by prune -a, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "agents", "agent-sessions.json")); !os.IsNotExist(err) {
		t.Fatalf("new-layout agent-sessions.json should not be created, got err=%v", err)
	}
}

func TestPruneLocalSidecarsPreservesWarningMarkerWithoutIncludeSettings(t *testing.T) {
	rootDir := t.TempDir()
	legacyMarker := filepath.Join(rootDir, "agent-warning-acknowledged")
	newMarker := filepath.Join(rootDir, "agents", "agent-warning-acknowledged")

	writeTestFile(t, filepath.Join(rootDir, "agent-sessions.json"), `[{"session_id":"s1","name":"alice"}]`)
	writeTestFile(t, filepath.Join(rootDir, "agents", "agent-sessions.json"), `[{"session_id":"s2","name":"bob"}]`)
	writeTestFile(t, legacyMarker, "yes")
	writeTestFile(t, newMarker, "yes")

	if err := pruneLocalSidecars(rootDir, false); err != nil {
		t.Fatalf("pruneLocalSidecars returned error: %v", err)
	}

	for _, path := range []string{
		filepath.Join(rootDir, "agent-sessions.json"),
		filepath.Join(rootDir, "agents", "agent-sessions.json"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, got err=%v", path, err)
		}
	}
	for _, path := range []string{legacyMarker, newMarker} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s should be preserved by plain prune, got err=%v", path, err)
		}
	}
}

func TestPruneLocalSidecarsRemovesWarningMarkerWithIncludeSettings(t *testing.T) {
	rootDir := t.TempDir()
	legacyMarker := filepath.Join(rootDir, "agent-warning-acknowledged")
	newMarker := filepath.Join(rootDir, "agents", "agent-warning-acknowledged")

	writeTestFile(t, filepath.Join(rootDir, "agent-sessions.json"), `[{"session_id":"s1","name":"alice"}]`)
	writeTestFile(t, filepath.Join(rootDir, "agents", "agent-sessions.json"), `[{"session_id":"s2","name":"bob"}]`)
	writeTestFile(t, legacyMarker, "yes")
	writeTestFile(t, newMarker, "yes")

	if err := pruneLocalSidecars(rootDir, true); err != nil {
		t.Fatalf("pruneLocalSidecars returned error: %v", err)
	}

	for _, path := range []string{
		filepath.Join(rootDir, "agent-sessions.json"),
		filepath.Join(rootDir, "agents", "agent-sessions.json"),
		legacyMarker,
		newMarker,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed by prune -a, got err=%v", path, err)
		}
	}
}

func TestPrepareServerOwnershipRefusesAndDoesNotMigrateWhenLegacyDaemonAlive(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "aimebu.pid"), strconv.Itoa(os.Getpid()))
	writeTestFile(t, filepath.Join(rootDir, "rooms.json"), `[{"id":"general"}]`)
	writeTestFile(t, filepath.Join(rootDir, "messages.json"), `[{"id":1}]`)

	err := prepareServerOwnership(rootDir)
	if err == nil {
		t.Fatal("prepareServerOwnership() error = nil, want already-running error")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("prepareServerOwnership() error = %q", err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "server")); !os.IsNotExist(err) {
		t.Fatalf("server dir should not be created, got err=%v", err)
	}
	for _, name := range []string{"rooms.json", "messages.json"} {
		if _, err := os.Stat(filepath.Join(rootDir, name)); err != nil {
			t.Fatalf("legacy %s should stay at root, got err=%v", name, err)
		}
	}
}

func TestServerStartStatusRoundTrip(t *testing.T) {
	cli := buildTestCLI(t)
	rootDir := t.TempDir()
	port := freePort(t)
	env := append(os.Environ(),
		"AIMEBU_CONFIG_DIR="+rootDir,
		fmt.Sprintf("AIMEBU_PORT=%d", port),
		"AIMEBU_BIND=127.0.0.1",
	)

	start := exec.Command(cli, "server", "start")
	start.Env = env
	startOut, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("server start failed: %v\n%s", err, startOut)
	}
	t.Cleanup(func() {
		stop := exec.Command(cli, "server", "stop")
		stop.Env = env
		_, _ = stop.CombinedOutput()
	})

	status := exec.Command(cli, "server", "status")
	status.Env = env
	statusOut, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("server status failed: %v\n%s", err, statusOut)
	}
	if !strings.Contains(string(statusOut), "aimebu is running") {
		logData, _ := os.ReadFile(filepath.Join(rootDir, "server", "aimebu.log"))
		t.Fatalf("server status output = %q\nlog:\n%s", statusOut, logData)
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

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func buildTestCLI(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(t.TempDir(), "aimebu-test")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/aimebu")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build test cli failed: %v\n%s", err, out)
	}
	return binary
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
