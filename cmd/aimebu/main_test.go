package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"math/big"
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
	"time"

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
	writeTestFile(t, filepath.Join(agentsDir, "agent-logs", "alice.log"), `{"event":"wrapper_start"}`)
	writeTestFile(t, filepath.Join(serverDir, "macros.json"), `{"macros":{"zzz_custom_only_for_test":"body"}}`)
	writeTestFile(t, filepath.Join(serverDir, "fleet.json"), `{"version":1,"fleets":{"default":{"agents":[{"command":"echo hi"}]}}}`)

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

	for _, table := range []string{"rooms", "messages", "agents"} {
		if got := sqliteTableCount(t, filepath.Join(serverDir, "aimebu.sqlite"), table); got != 0 {
			t.Fatalf("%s rows after offline prune = %d, want 0", table, got)
		}
	}

	if _, err := os.Stat(filepath.Join(agentsDir, "agent-sessions.json")); !os.IsNotExist(err) {
		t.Fatalf("agent-sessions.json should be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "agent-warning-acknowledged")); !os.IsNotExist(err) {
		t.Fatalf("agent-warning-acknowledged should be removed by prune -a, got err=%v", err)
	}
	assertDirEmpty(t, filepath.Join(agentsDir, "agent-logs"))

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
	if _, err := os.Stat(filepath.Join(serverDir, "fleet.json")); !os.IsNotExist(err) {
		t.Fatalf("fleet.json should be removed by prune -a, got err=%v", err)
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
	writeTestFile(t, filepath.Join(rootDir, "agents", "agent-logs", "alice.log"), `{"event":"wrapper_start"}`)

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
	assertDirEmpty(t, filepath.Join(rootDir, "agents", "agent-logs"))
}

func TestPruneLocalSidecarsRemovesLogsAndPreservesWarningMarkerWithoutIncludeSettings(t *testing.T) {
	rootDir := t.TempDir()
	legacyMarker := filepath.Join(rootDir, "agent-warning-acknowledged")
	newMarker := filepath.Join(rootDir, "agents", "agent-warning-acknowledged")
	logDir := filepath.Join(rootDir, "agents", "agent-logs")

	writeTestFile(t, filepath.Join(rootDir, "agent-sessions.json"), `[{"session_id":"s1","name":"alice"}]`)
	writeTestFile(t, filepath.Join(rootDir, "agents", "agent-sessions.json"), `[{"session_id":"s2","name":"bob"}]`)
	writeTestFile(t, legacyMarker, "yes")
	writeTestFile(t, newMarker, "yes")
	writeTestFile(t, filepath.Join(logDir, "alice.log"), `{"event":"wrapper_start"}`)
	writeTestFile(t, filepath.Join(logDir, "_pre-register-feedbeefcafebabe.log"), `{"event":"wrapper_start"}`)

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
	assertDirEmpty(t, logDir)
}

func TestPruneLocalSidecarsRemovesLogsAndWarningMarkerWithIncludeSettings(t *testing.T) {
	rootDir := t.TempDir()
	legacyMarker := filepath.Join(rootDir, "agent-warning-acknowledged")
	newMarker := filepath.Join(rootDir, "agents", "agent-warning-acknowledged")
	logDir := filepath.Join(rootDir, "agents", "agent-logs")

	writeTestFile(t, filepath.Join(rootDir, "agent-sessions.json"), `[{"session_id":"s1","name":"alice"}]`)
	writeTestFile(t, filepath.Join(rootDir, "agents", "agent-sessions.json"), `[{"session_id":"s2","name":"bob"}]`)
	writeTestFile(t, legacyMarker, "yes")
	writeTestFile(t, newMarker, "yes")
	writeTestFile(t, filepath.Join(logDir, "alice.log"), `{"event":"wrapper_start"}`)

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
	assertDirEmpty(t, logDir)
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

func TestServerStartFailsWhenDaemonExitsDuringStartup(t *testing.T) {
	cli := buildTestCLI(t)
	rootDir := t.TempDir()
	certPath, keyPath := writeMainTestCertificatePair(t)
	tlsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on TLS port: %v", err)
	}
	defer tlsListener.Close()
	tlsPort := tlsListener.Addr().(*net.TCPAddr).Port

	env := append(os.Environ(),
		"AIMEBU_CONFIG_DIR="+rootDir,
		fmt.Sprintf("AIMEBU_PORT=%d", freePort(t)),
		"AIMEBU_BIND=127.0.0.1",
		"AIMEBU_TLS_CERT="+certPath,
		"AIMEBU_TLS_KEY="+keyPath,
		fmt.Sprintf("AIMEBU_TLS_PORT=%d", tlsPort),
	)

	start := exec.Command(cli, "server", "start")
	start.Env = env
	startOut, err := start.CombinedOutput()
	if err == nil {
		t.Fatalf("server start succeeded with occupied TLS port:\n%s", startOut)
	}
	if !strings.Contains(string(startOut), "daemon exited during startup") {
		t.Fatalf("server start output = %q, want startup-exit error", startOut)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "server", "aimebu.pid")); !os.IsNotExist(err) {
		t.Fatalf("pid file should be removed after startup exit, got err=%v", err)
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

func TestUsagesCLIEmptyRegistryServerOff(t *testing.T) {
	cli := buildTestCLI(t)
	rootDir := t.TempDir()
	env := append(os.Environ(), "AIMEBU_CONFIG_DIR="+rootDir)

	plain := exec.Command(cli, "usages")
	plain.Env = env
	plainOut, err := plain.CombinedOutput()
	if err != nil {
		t.Fatalf("usages plain failed: %v\n%s", err, plainOut)
	}
	if got := strings.TrimSpace(string(plainOut)); got != "No usage providers enabled." {
		t.Fatalf("plain output = %q", got)
	}

	jsonCmd := exec.Command(cli, "usages", "--json")
	jsonCmd.Env = env
	jsonOut, err := jsonCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("usages json failed: %v\n%s", err, jsonOut)
	}
	if got := strings.TrimSpace(string(jsonOut)); got != `{"snapshots":{},"settings":{"refresh_interval_sec":120,"min_refresh_sec":15,"env_override":false,"percent_display":"left"},"providers":[{"key":"codex","label":"Codex","enabled":false,"available":true},{"key":"claude-code","label":"Claude Code","enabled":false,"available":true},{"key":"github-copilot","label":"GitHub Copilot","enabled":false,"available":true},{"key":"ollama-cloud","label":"Ollama Cloud","enabled":false,"available":true}]}` {
		t.Fatalf("json output = %q", got)
	}

	bad := exec.Command(cli, "usages", "bogus")
	bad.Env = env
	badOut, err := bad.CombinedOutput()
	if err == nil {
		t.Fatalf("unknown provider succeeded: %s", badOut)
	}
	if !strings.Contains(string(badOut), `unknown provider "bogus"`) {
		t.Fatalf("unknown provider output = %s", badOut)
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

func sqliteTableCount(t *testing.T, dbPath, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func writeMainTestCertificatePair(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("write cert file: %v", err)
	}
	if err := certFile.Close(); err != nil {
		t.Fatalf("close cert file: %v", err)
	}
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	if err := keyFile.Close(); err != nil {
		t.Fatalf("close key file: %v", err)
	}
	return certPath, keyPath
}

func assertDirEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read dir %s: %v", path, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s should be empty, found %d entries", path, len(entries))
	}
}
