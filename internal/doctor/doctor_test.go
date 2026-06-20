package doctor

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckServerReachable(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		r := checkServerReachable(srv.URL)
		if r.Status != StatusOK {
			t.Fatalf("want OK, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		r := checkServerReachable("http://127.0.0.1:1") // no listener
		if r.Status != StatusFail {
			t.Fatalf("want FAIL, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)

		r := checkServerReachable(srv.URL)
		if r.Status != StatusFail {
			t.Fatalf("want FAIL, got %s: %s", r.Status, r.Message)
		}
	})
}

func TestCheckConfigDir(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		dir := t.TempDir()
		r := checkConfigDir(dir)
		if r.Status != StatusOK {
			t.Fatalf("want OK, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("missing", func(t *testing.T) {
		r := checkConfigDir(filepath.Join(t.TempDir(), "nonexistent"))
		if r.Status != StatusFail {
			t.Fatalf("want FAIL, got %s: %s", r.Status, r.Message)
		}
	})
}

func TestCheckSQLite(t *testing.T) {
	t.Run("not found warns", func(t *testing.T) {
		dir := t.TempDir()
		r := checkSQLite(dir)
		if r.Status != StatusWarn {
			t.Fatalf("want WARN when db absent, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("exists ok", func(t *testing.T) {
		dir := t.TempDir()
		serverDir := filepath.Join(dir, "server")
		if err := os.MkdirAll(serverDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(serverDir, "aimebu.sqlite")
		if err := os.WriteFile(dbPath, []byte("SQLite format 3"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := checkSQLite(dir)
		if r.Status != StatusOK {
			t.Fatalf("want OK, got %s: %s", r.Status, r.Message)
		}
	})
}

func TestCheckDataDirs(t *testing.T) {
	t.Run("both missing warns", func(t *testing.T) {
		dir := t.TempDir()
		r := checkDataDirs(dir)
		if r.Status != StatusWarn {
			t.Fatalf("want WARN, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("both present ok", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"server", "agents"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		r := checkDataDirs(dir)
		if r.Status != StatusOK {
			t.Fatalf("want OK, got %s: %s", r.Status, r.Message)
		}
	})
}

func TestCheckSchemaVersion(t *testing.T) {
	t.Run("db absent warns", func(t *testing.T) {
		r := checkSchemaVersion(t.TempDir())
		if r.Status != StatusWarn {
			t.Fatalf("want WARN when db absent, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("current version ok", func(t *testing.T) {
		dir := t.TempDir()
		serverDir := filepath.Join(dir, "server")
		if err := os.MkdirAll(serverDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(serverDir, "aimebu.sqlite")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE schema_meta (version INTEGER NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_meta(version, applied_at) VALUES (?, ?)`, expectedSchemaVersion, "2026-01-01T00:00:00Z"); err != nil {
			db.Close()
			t.Fatal(err)
		}
		db.Close()

		r := checkSchemaVersion(dir)
		if r.Status != StatusOK {
			t.Fatalf("want OK, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("future version warns", func(t *testing.T) {
		dir := t.TempDir()
		serverDir := filepath.Join(dir, "server")
		if err := os.MkdirAll(serverDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(serverDir, "aimebu.sqlite")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE schema_meta (version INTEGER NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_meta(version, applied_at) VALUES (?, ?)`, expectedSchemaVersion+1, "2026-01-01T00:00:00Z"); err != nil {
			db.Close()
			t.Fatal(err)
		}
		db.Close()

		r := checkSchemaVersion(dir)
		if r.Status != StatusWarn {
			t.Fatalf("want WARN for future version, got %s: %s", r.Status, r.Message)
		}
	})
}

func TestCheckAllowlist(t *testing.T) {
	t.Run("default (unset) ok", func(t *testing.T) {
		t.Setenv("AIMEBU_ALLOW", "")
		r := checkAllowlist()
		if r.Status != StatusOK {
			t.Fatalf("want OK for default, got %s: %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "127.0.0.0/8") {
			t.Fatalf("expected default CIDR in message, got: %s", r.Message)
		}
	})

	t.Run("valid CIDR ok", func(t *testing.T) {
		t.Setenv("AIMEBU_ALLOW", "10.0.0.0/8,192.168.1.0/24")
		r := checkAllowlist()
		if r.Status != StatusOK {
			t.Fatalf("want OK for valid CIDRs, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("invalid entry fails", func(t *testing.T) {
		t.Setenv("AIMEBU_ALLOW", "not-an-ip")
		r := checkAllowlist()
		if r.Status != StatusFail {
			t.Fatalf("want FAIL for invalid entry, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("stray comma fails", func(t *testing.T) {
		t.Setenv("AIMEBU_ALLOW", "127.0.0.1,,::1")
		r := checkAllowlist()
		if r.Status != StatusFail {
			t.Fatalf("want FAIL for stray comma, got %s: %s", r.Status, r.Message)
		}
	})
}

func TestCheckAgentLiveness(t *testing.T) {
	t.Run("server down warns", func(t *testing.T) {
		r := checkAgentLiveness("http://127.0.0.1:1")
		if r.Status != StatusWarn {
			t.Fatalf("want WARN when server down, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("all active ok", func(t *testing.T) {
		agents := map[string]any{"agents": []map[string]any{
			{"id": "alice@test", "status": "active"},
			{"id": "bob@test", "status": "active"},
		}}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(agents)
		}))
		t.Cleanup(srv.Close)
		r := checkAgentLiveness(srv.URL)
		if r.Status != StatusOK {
			t.Fatalf("want OK for all-active agents, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("offline agent warns", func(t *testing.T) {
		agents := map[string]any{"agents": []map[string]any{
			{"id": "alice@test", "status": "active"},
			{"id": "bob@test", "status": "offline"},
		}}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(agents)
		}))
		t.Cleanup(srv.Close)
		r := checkAgentLiveness(srv.URL)
		if r.Status != StatusWarn {
			t.Fatalf("want WARN with offline agent, got %s: %s", r.Status, r.Message)
		}
	})
}

func TestCheckTLSEnv(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		t.Setenv("AIMEBU_TLS_CERT", "")
		t.Setenv("AIMEBU_TLS_KEY", "")
		r := checkTLSEnv()
		if r.Status != StatusOK {
			t.Fatalf("want OK, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("only cert set", func(t *testing.T) {
		t.Setenv("AIMEBU_TLS_CERT", "/some/cert.pem")
		t.Setenv("AIMEBU_TLS_KEY", "")
		r := checkTLSEnv()
		if r.Status != StatusFail {
			t.Fatalf("want FAIL when only cert set, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("both set and readable", func(t *testing.T) {
		dir := t.TempDir()
		cert := filepath.Join(dir, "cert.pem")
		key := filepath.Join(dir, "key.pem")
		os.WriteFile(cert, []byte("cert"), 0o600)
		os.WriteFile(key, []byte("key"), 0o600)
		t.Setenv("AIMEBU_TLS_CERT", cert)
		t.Setenv("AIMEBU_TLS_KEY", key)
		r := checkTLSEnv()
		if r.Status != StatusOK {
			t.Fatalf("want OK, got %s: %s", r.Status, r.Message)
		}
	})

	t.Run("cert not readable", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AIMEBU_TLS_CERT", filepath.Join(dir, "missing.pem"))
		t.Setenv("AIMEBU_TLS_KEY", filepath.Join(dir, "missing.pem"))
		r := checkTLSEnv()
		if r.Status != StatusFail {
			t.Fatalf("want FAIL for missing cert, got %s: %s", r.Status, r.Message)
		}
	})
}

func TestPrint(t *testing.T) {
	results := []Result{
		{"check a", StatusOK, "all good"},
		{"check b", StatusWarn, "something odd"},
		{"check c", StatusFail, "broken"},
	}
	var buf bytes.Buffer
	worst := Print(&buf, results)
	if worst != StatusFail {
		t.Fatalf("worst = %v, want FAIL", worst)
	}
	out := buf.String()
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "WARN") || !strings.Contains(out, "OK") {
		t.Fatalf("output missing expected statuses: %s", out)
	}
}
