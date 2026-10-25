package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestCheckConfigDir(t *testing.T) {
	base := t.TempDir()
	worldWritable := filepath.Join(base, "world-writable")
	if err := os.MkdirAll(worldWritable, 0o700); err != nil {
		t.Skipf("cannot create world-writable directory: %v", err)
	}
	if err := os.Chmod(worldWritable, 0o777); err != nil {
		t.Skipf("cannot make directory world-writable: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root; world-writable case is not observable")
	}
	tests := []struct {
		name string
		path string
		want string
	}{
		{"existing dir", base, statusOK},
		{"missing dir", filepath.Join(base, "does-not-exist"), statusFail},
		{"world-writable dir", worldWritable, statusWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := checkConfigDir(tt.path)
			if check.Status != tt.want {
				t.Fatalf("status = %q (%s), want %q", check.Status, check.Description, tt.want)
			}
		})
	}
}

// createFixtureDatabase builds a real v2 database (all migrations applied) at
// path, the way `aimebu server` does on first start.
func createFixtureDatabase(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("open fixture database %s: %v", path, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// rewriteSchemaVersion makes the fixture's schema_version table report the
// given version by deleting newer rows (and inserting the version if it is
// higher than the applied ones). Only the stored max matters to doctor.
func rewriteSchemaVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM schema_version WHERE version > ?`, version); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO schema_version(version, applied_at) VALUES (?, '2026-01-01T00:00:00Z')`, version); err != nil {
		t.Fatal(err)
	}
}

// addV1SchemaMeta mimics a v1 server started on a v2 folder: v1 creates its
// own schema_meta table next to v2's tables.
func addV1SchemaMeta(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSQLiteDB(t *testing.T) {
	base := t.TempDir()
	valid := filepath.Join(base, "valid.sqlite")
	createFixtureDatabase(t, valid)
	stale := filepath.Join(base, "stale.sqlite")
	createFixtureDatabase(t, stale)
	rewriteSchemaVersion(t, stale, 1)
	newer := filepath.Join(base, "newer.sqlite")
	createFixtureDatabase(t, newer)
	rewriteSchemaVersion(t, newer, store.LatestSchemaVersion()+1)
	corrupt := filepath.Join(base, "corrupt.sqlite")
	if err := os.WriteFile(corrupt, []byte("this is not a sqlite database\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(base, "empty.sqlite")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mixed := filepath.Join(base, "mixed.sqlite")
	createFixtureDatabase(t, mixed)
	addV1SchemaMeta(t, mixed)
	tests := []struct {
		name string
		path string
		want string
	}{
		{"v1 tables inside a v2 database", mixed, statusFail},
		{"missing database", filepath.Join(base, "absent.sqlite"), statusWarn},
		{"valid database", valid, statusOK},
		{"stale schema", stale, statusWarn},
		{"newer schema", newer, statusFail},
		{"corrupt file", corrupt, statusFail},
		{"empty file", empty, statusWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := checkSQLiteDB(tt.path)
			if check.Status != tt.want {
				t.Fatalf("status = %q (%s), want %q", check.Status, check.Description, tt.want)
			}
		})
	}
}

func TestCheckServerHealth(t *testing.T) {
	tests := []struct {
		name   string
		status int // 0 = nothing listening
		want   string
	}{
		{"unauthorized 401", 401, statusOK},
		{"other status 200", 200, statusWarn},
		{"other status 404", 404, statusWarn},
		{"connection refused", 0, statusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var port int
			if tt.status != 0 {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
				}))
				defer server.Close()
				port = testServerPort(t, server.URL)
			} else {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port = listener.Addr().(*net.TCPAddr).Port
				listener.Close()
			}
			check := checkServerHealth(context.Background(), "127.0.0.1", port)
			if check.Status != tt.want {
				t.Fatalf("status = %q (%s), want %q", check.Status, check.Description, tt.want)
			}
		})
	}
}

// writeCertificate generates a self-signed certificate valid until notAfter
// plus a matching RSA key, written as <name>-cert.pem / <name>-key.pem.
func writeCertificate(t *testing.T, dir, name string, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "aimebu doctor test"},
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, name+"-cert.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, name+"-key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestCheckTLS(t *testing.T) {
	base := t.TempDir()
	certOK, keyOK := writeCertificate(t, base, "ok", time.Now().Add(365*24*time.Hour))
	certSoon, keySoon := writeCertificate(t, base, "soon", time.Now().Add(10*24*time.Hour))
	certExpired, keyExpired := writeCertificate(t, base, "expired", time.Now().Add(-1*time.Hour))
	keyOther, _ := writeCertificate(t, base, "other", time.Now().Add(365*24*time.Hour))
	unreadableKey := filepath.Join(base, "unreadable-key.pem")
	if err := os.WriteFile(unreadableKey, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "missing.pem")
	tests := []struct {
		name    string
		certEnv string
		keyEnv  string
		want    string
	}{
		{"not configured", "", "", statusOK},
		{"cert only", certOK, "", statusFail},
		{"key only", "", keyOK, statusFail},
		{"missing cert file", missing, keyOK, statusFail},
		{"missing key file", certOK, missing, statusFail},
		{"unparseable key", certOK, unreadableKey, statusFail},
		{"mismatched key", certOK, keyOther, statusFail},
		{"valid pair", certOK, keyOK, statusOK},
		{"expiring soon", certSoon, keySoon, statusWarn},
		{"expired", certExpired, keyExpired, statusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AIMEBU2_TLS_CERT", tt.certEnv)
			t.Setenv("AIMEBU2_TLS_KEY", tt.keyEnv)
			t.Setenv("AIMEBU2_TLS_PORT", "")
			check := checkTLS(9897)
			if check.Status != tt.want {
				t.Fatalf("status = %q (%s), want %q", check.Status, check.Description, tt.want)
			}
		})
	}
}

func TestCheckBindAllow(t *testing.T) {
	allow := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	tests := []struct {
		name   string
		config serverConfig
		tlsEnv bool
		want   string
	}{
		{"loopback bind", serverConfig{bind: "127.0.0.1", port: 9897, allow: allow, allowDefault: true}, false, statusOK},
		{"ipv6 loopback bind", serverConfig{bind: "::1", port: 9897, allow: allow}, false, statusOK},
		{"non-loopback without tls", serverConfig{bind: "0.0.0.0", port: 9897, allow: allow}, false, statusWarn},
		{"non-loopback with tls", serverConfig{bind: "0.0.0.0", port: 9897, allow: allow}, true, statusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.tlsEnv {
				t.Setenv("AIMEBU2_TLS_CERT", "/nonexistent/cert.pem")
				t.Setenv("AIMEBU2_TLS_KEY", "/nonexistent/key.pem")
			} else {
				t.Setenv("AIMEBU2_TLS_CERT", "")
				t.Setenv("AIMEBU2_TLS_KEY", "")
			}
			check := checkBindAllow(tt.config)
			if check.Status != tt.want {
				t.Fatalf("status = %q (%s), want %q", check.Status, check.Description, tt.want)
			}
		})
	}
}

func testServerPort(t *testing.T, rawURL string) int {
	t.Helper()
	_, portString, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestDoctorCommand(t *testing.T) {
	rootWithDB := t.TempDir()
	createFixtureDatabase(t, filepath.Join(rootWithDB, "server", "aimebu.sqlite"))
	rootWithoutDB := t.TempDir()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	upPort := testServerPort(t, server.URL)

	downPort := func() int {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		return port
	}

	tests := []struct {
		name     string
		root     string
		serverUp bool
		jsonOut  bool
		wantExit int
	}{
		{"all healthy", rootWithDB, true, false, 0},
		{"server down", rootWithDB, false, false, 1},
		{"missing database only warns", rootWithoutDB, true, false, 0},
		{"json all healthy", rootWithDB, true, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AIMEBU2_CONFIG_DIR", tt.root)
			t.Setenv("AIMEBU2_BIND", "127.0.0.1")
			if tt.serverUp {
				t.Setenv("AIMEBU2_PORT", strconv.Itoa(upPort))
			} else {
				t.Setenv("AIMEBU2_PORT", strconv.Itoa(downPort()))
			}
			t.Setenv("AIMEBU2_ALLOW", "")
			t.Setenv("AIMEBU2_TLS_CERT", "")
			t.Setenv("AIMEBU2_TLS_KEY", "")

			var stdout, stderr bytes.Buffer
			args := []string{}
			if tt.jsonOut {
				args = append(args, "--json")
			}
			code := doctorCommand(context.Background(), args, &stdout, &stderr)
			if code != tt.wantExit {
				t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.wantExit, stdout.String(), stderr.String())
			}
			output := stdout.String()
			if tt.jsonOut {
				var decoded doctorResult
				if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
					t.Fatalf("invalid JSON output: %v\n%s", err, output)
				}
				if len(decoded.Checks) < 5 || decoded.ExitCode != tt.wantExit || decoded.Version == "" || decoded.GeneratedAt == "" {
					t.Fatalf("decoded result = %+v, want at least 5 checks, a version, a timestamp and exit %d", decoded, tt.wantExit)
				}
				for _, check := range decoded.Checks {
					if check.Scope != scopeServer {
						t.Fatalf("check %q has scope %q, want %q", check.Name, check.Scope, scopeServer)
					}
				}
			} else {
				for _, name := range []string{"config dir", "sqlite database", "server", "tls", "bind/allow"} {
					if !strings.Contains(output, name) {
						t.Fatalf("output missing check %q:\n%s", name, output)
					}
				}
				if tt.wantExit == 0 && strings.Contains(output, "✗") {
					t.Fatalf("no failures expected but output shows fail:\n%s", output)
				}
			}
		})
	}
}

func TestCheckSwitcher(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(home, ".aimebu2"))
	root := filepath.Join(home, ".aimebu2", "switcher")
	byName := func() map[string]doctorCheck {
		t.Helper()
		got := map[string]doctorCheck{}
		for _, check := range checkSwitcher() {
			got[check.Name] = check
		}
		return got
	}
	writeState := func(state string) {
		t.Helper()
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if got := checkSwitcher(); len(got) != 0 {
		t.Fatalf("unconfigured switcher reported %v", got)
	}
	writeState("{not json")
	if got := byName()["switcher"]; got.Status != statusFail {
		t.Fatalf("corrupt state: %+v", got)
	}
	writeState(`{"version":1,"enabled":false,"active":{}}`)
	if got := byName()["switcher"]; got.Status != statusOK || got.Description != "disabled" {
		t.Fatalf("disabled: %+v", got)
	}
	writeState(`{"version":1,"enabled":true,"active":{"codex":"main"}}`)
	got := byName()
	if got["switcher/claude"].Status != statusOK || got["switcher/codex"].Status != statusWarn || !strings.Contains(got["switcher/codex"].Description, "stale pointer") {
		t.Fatalf("stale pointer: %+v", got)
	}
	profile := filepath.Join(root, "profiles", "codex", "main")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backups", "codex", "main-1.json")
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := byName()["switcher/codex"]; got.Status != statusWarn || !strings.Contains(got.Description, "no stored credentials") || !strings.Contains(got.Description, backup) {
		t.Fatalf("no credentials: %+v", got)
	}
	// Another profile's backup (main-old-...) must not be offered, and the
	// newest is picked by timestamp, not by name.
	for _, name := range []string{"main-old-99999999999.json", "main-100.json"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(backup), name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newest := filepath.Join(filepath.Dir(backup), "main-100.json")
	if got := byName()["switcher/codex"]; !strings.Contains(got.Description, newest) || strings.Contains(got.Description, "main-old") {
		t.Fatalf("backup hint: %+v", got)
	}
	// Live missing but an unlinked stored copy remains: warn.
	if err := os.WriteFile(filepath.Join(profile, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := byName()["switcher/codex"]; got.Status != statusWarn || !strings.Contains(got.Description, "stored copy but no live") {
		t.Fatalf("stored only: %+v", got)
	}
	live := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := byName()["switcher/codex"]; got.Status != statusOK || got.Description != `active: "main"` {
		t.Fatalf("healthy: %+v", got)
	}
}

func TestProxyThrottleWarningAtStartupAndInDoctor(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	var output bytes.Buffer
	logProxyStartup(&output, "https://aimebu.example.com", nil)
	if !strings.Contains(output.String(), "WARNING") || !strings.Contains(output.String(), "AIMEBU2_TRUSTED_PROXIES") ||
		!strings.Contains(output.String(), "one throttle identity") {
		t.Fatalf("startup warning=%q", output.String())
	}
	for _, tc := range []struct {
		publicURL string
		trusted   []netip.Prefix
	}{{"", nil}, {"https://aimebu.example.com", trusted}} {
		output.Reset()
		logProxyStartup(&output, tc.publicURL, tc.trusted)
		if output.Len() != 0 {
			t.Fatalf("unexpected warning for %q %v: %q", tc.publicURL, tc.trusted, output.String())
		}
	}

	t.Setenv("AIMEBU2_PUBLIC_URL", "")
	if _, shown := checkProxyThrottle(serverConfig{}); shown {
		t.Fatal("proxy check shown without a public URL")
	}
	t.Setenv("AIMEBU2_PUBLIC_URL", "https://aimebu.example.com")
	if check, shown := checkProxyThrottle(serverConfig{}); !shown || check.Status != statusWarn ||
		!strings.Contains(check.Description, "AIMEBU2_TRUSTED_PROXIES") {
		t.Fatalf("untrusted proxy check=%+v shown=%v", check, shown)
	}
	if check, shown := checkProxyThrottle(serverConfig{trusted: trusted}); !shown || check.Status != statusOK {
		t.Fatalf("trusted proxy check=%+v shown=%v", check, shown)
	}
	t.Setenv("AIMEBU2_PUBLIC_URL", "https://aimebu.example.com/app")
	if check, shown := checkProxyThrottle(serverConfig{}); !shown || check.Status != statusFail {
		t.Fatalf("invalid public URL check=%+v shown=%v", check, shown)
	}
}
