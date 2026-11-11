package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

const copiedLegacyID = "0f7c6a1e-1111-4222-8333-444455556666"

func legacyCopyDelivery(tool, checkout string) protocol.Delivery {
	return protocol.Delivery{AgentID: "agt_legacy_copy", Agent: adapter.AgentSpec{ID: "agt_legacy_copy", Tool: tool, Variant: "native"},
		NativeSessionHandle: copiedLegacyID, NativeSessionStarted: true, ImportLegacySession: true, CheckoutPath: checkout}
}

func legacyPrivate(config, tool string) string {
	sum := sha256.Sum256([]byte("agt_legacy_copy"))
	return filepath.Join(config, "supervisor", tool, hex.EncodeToString(sum[:]))
}

func putLegacyFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCopyLegacyNativeSessionFilesBeforeResume(t *testing.T) {
	for _, tc := range []struct {
		tool, env, source, destination, contents string
	}{
		{"codex", "CODEX_HOME", filepath.Join("sessions", "2026", "11", "11", "rollout-2026-11-11T000000-"+copiedLegacyID+".jsonl"), filepath.Join("sessions", "2026", "11", "11", "rollout-2026-11-11T000000-"+copiedLegacyID+".jsonl"), "codex history\n"},
		{"pi", "PI_CODING_AGENT_DIR", filepath.Join("sessions", "--work--", "2026-11-11T00-00-00_"+copiedLegacyID+".jsonl"), "2026-11-11T00-00-00_" + copiedLegacyID + ".jsonl", "pi history\n"},
		{"vibe", "VIBE_HOME", filepath.Join("sessions", "session_20261111_000000_"+copiedLegacyID[:8], "messages.jsonl"), filepath.Join("sessions", "session_20261111_000000_"+copiedLegacyID[:8], "messages.jsonl"), "vibe history\n"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			root := t.TempDir()
			user := filepath.Join(root, "user")
			t.Setenv(tc.env, user)
			if tc.tool == "vibe" {
				putLegacyFile(t, filepath.Join(user, "sessions", "session_20261111_000000_"+copiedLegacyID[:8], "meta.json"), `{"session_id":"`+copiedLegacyID+`"}`)
			}
			source := filepath.Join(user, tc.source)
			putLegacyFile(t, source, tc.contents)
			if err := os.Chmod(source, 0400); err != nil {
				t.Fatal(err)
			}
			config, checkout := filepath.Join(root, "config"), filepath.Join(root, "checkout")
			c := &Client{ConfigDir: config}
			d := legacyCopyDelivery(tc.tool, checkout)
			if err := c.copyLegacySession(context.Background(), d, false); err != nil {
				t.Fatal(err)
			}
			private := legacyPrivate(config, tc.tool)
			if tc.tool == "pi" {
				private = filepath.Join(checkout, ".aimebu", "pi-sessions", filepath.Base(private))
			}
			got, err := os.ReadFile(filepath.Join(private, tc.destination))
			if err != nil || string(got) != tc.contents {
				t.Fatalf("private session: %q %v", got, err)
			}
			if err := c.copyLegacySession(context.Background(), d, false); err != nil {
				t.Fatalf("redelivery must keep the original copy: %v", err)
			}
			if original, err := os.ReadFile(source); err != nil || string(original) != tc.contents {
				t.Fatalf("source changed: %q %v", original, err)
			}
		})
	}
}

func TestCopyLegacySessionWithNamedNativeLauncher(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user")
	t.Setenv("CODEX_HOME", user)
	name := "rollout-2026-11-11T000000-" + copiedLegacyID + ".jsonl"
	putLegacyFile(t, filepath.Join(user, "sessions", name), "old work\n")
	config := filepath.Join(root, "config")
	c := &Client{ConfigDir: config}
	d := legacyCopyDelivery("codex", filepath.Join(root, "checkout"))
	d.Agent.Variant = "my-launcher"
	if err := c.copyLegacySession(context.Background(), d, false); err != nil {
		t.Fatalf("named native launcher must stage the selected session: %v", err)
	}
	private := filepath.Join(legacyPrivate(config, "codex"), "sessions", name)
	if got, err := os.ReadFile(private); err != nil || string(got) != "old work\n" {
		t.Fatalf("private session: %q %v", got, err)
	}
	if err := c.copyLegacySession(context.Background(), d, true); err == nil {
		t.Fatal("containerized launcher must not stage a host session")
	}
}

func TestCopyLegacyOpenCodeDatabaseKeepsOnlySelectedSession(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "user", "opencode.db")
	putLegacyFile(t, source, "")
	t.Setenv("OPENCODE_DB", source)
	db, err := sql.Open("sqlite", sqliteutil.DSN(source, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE project(id TEXT PRIMARY KEY)`,
		`CREATE TABLE session(id TEXT PRIMARY KEY,project_id TEXT REFERENCES project(id) ON DELETE CASCADE)`,
		`CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT REFERENCES session(id) ON DELETE CASCADE,body TEXT)`,
		`CREATE TABLE credential(secret TEXT)`,
		`INSERT INTO project(id) VALUES ('keep'),('other')`,
		`INSERT INTO session(id,project_id) VALUES ('` + copiedLegacyID + `','keep'),('other-session','other')`,
		`INSERT INTO message(id,session_id,body) VALUES ('wanted','` + copiedLegacyID + `','old work'),('foreign','other-session','secret work')`,
		`INSERT INTO credential(secret) VALUES ('private credential')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0400); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config")
	c := &Client{ConfigDir: config}
	d := legacyCopyDelivery("opencode", filepath.Join(root, "checkout"))
	if err := c.copyLegacySession(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	privateDB, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(legacyPrivate(config, "opencode"), "opencode.db"), true))
	if err != nil {
		t.Fatal(err)
	}
	defer privateDB.Close()
	for _, tc := range []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM session`, 1}, {`SELECT COUNT(*) FROM message`, 1}, {`SELECT COUNT(*) FROM project`, 1}, {`SELECT COUNT(*) FROM credential`, 0},
	} {
		var got int
		if err := privateDB.QueryRow(tc.query).Scan(&got); err != nil || got != tc.want {
			t.Fatalf("%s: %d want %d, %v", tc.query, got, tc.want, err)
		}
	}
	if err := c.copyLegacySession(context.Background(), d, false); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	privateBytes, err := os.ReadFile(filepath.Join(legacyPrivate(config, "opencode"), "opencode.db"))
	if err != nil || bytes.Contains(privateBytes, []byte("private credential")) || bytes.Contains(privateBytes, []byte("secret work")) {
		t.Fatalf("private snapshot retained unrelated content: %v", err)
	}
	original, err := sql.Open("sqlite", sqliteutil.DSN(source, true))
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	var originalSessions int
	if err := original.QueryRow(`SELECT COUNT(*) FROM session`).Scan(&originalSessions); err != nil || originalSessions != 2 {
		t.Fatalf("source sessions: %d %v", originalSessions, err)
	}
}

func TestCopyLegacyRejectsUnsafeSessionHandle(t *testing.T) {
	for _, handle := range []string{"*", "../other", "part..other", "name/other", strings.Repeat("a", 129)} {
		t.Run(handle, func(t *testing.T) {
			root := t.TempDir()
			c := &Client{ConfigDir: filepath.Join(root, "config")}
			d := legacyCopyDelivery("vibe", filepath.Join(root, "checkout"))
			d.NativeSessionHandle = handle
			if err := c.copyLegacySession(context.Background(), d, false); err == nil {
				t.Fatal("unsafe native handle was accepted")
			}
			if _, err := os.Stat(c.ConfigDir); !os.IsNotExist(err) {
				t.Fatalf("unsafe handle reached private runtime: %v", err)
			}
		})
	}
}
