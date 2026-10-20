package main

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

	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

func TestSessionsMergesLocalAndServerViews(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_BIND", "not-an-ip")
	t.Setenv("AIMEBU2_PORT", "not-a-port")
	t.Setenv("AIMEBU2_ALLOW", "bad-cidr")
	for _, dir := range []string{"supervisor", "server"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	local := fixtureDB(t, filepath.Join(root, "supervisor", "journal.sqlite"), `
		CREATE TABLE turn_start(agent_id TEXT,turn_id TEXT,generation INTEGER,exec TEXT,started_at TEXT,spec_json BLOB);
		CREATE TABLE journal_event(turn_id TEXT,kind TEXT,payload_json BLOB,created_at TEXT);
		INSERT INTO turn_start VALUES ('agt_one','turn-local',0,'exited','2026-10-15T00:00:00Z',
		'{"Agent":{"Tool":"codex","launcher_id":"launcher-one"},"NativeSessionStarted":true,"NativeSessionHandle":"thread-one"}');
		INSERT INTO turn_start VALUES ('agt_two','turn-only',0,'running','2026-10-15T01:00:00Z',
		'{"Agent":{"Tool":"pi"}}');
		INSERT INTO turn_start VALUES ('agt_three','turn-old',0,'exited','2026-10-15T00:00:00Z',
		'{"Agent":{"Tool":"codex"},"NativeSessionStarted":true,"NativeSessionHandle":"old-thread"}');
		INSERT INTO turn_start VALUES ('agt_four','turn-container',0,'exited','2026-10-15T00:00:00Z',
		'{"Agent":{"Tool":"codex","launcher_id":"launcher-container"},"LauncherContainerized":true,"NativeSessionStarted":true,"NativeSessionHandle":"container-thread"}');
		INSERT INTO turn_start VALUES ('agt_five','turn-remote',0,'exited','2026-10-15T00:00:00Z',
		'{"Agent":{"Tool":"pi"},"CheckoutPath":"/tmp/remote-project","NativeSessionStarted":true,"NativeSessionHandle":"remote-pi-thread"}');`)
	local.Close()
	server := fixtureDB(t, filepath.Join(root, "server", "aimebu.sqlite"), `
		CREATE TABLE agent(id TEXT,desired TEXT,observed TEXT,tool TEXT,variant TEXT,launcher_id TEXT,generation INTEGER,checkout_id TEXT,machine_id TEXT);
		CREATE TABLE checkout(id TEXT,canonical_path TEXT);
		CREATE TABLE machine(id TEXT,is_host INTEGER);
		CREATE TABLE machine_launcher(id TEXT,machine_id TEXT,containerized INTEGER);
		CREATE TABLE session(agent_id TEXT,generation INTEGER,ended_at TEXT,native_session_started_at TEXT,native_session_id TEXT);
		CREATE TABLE room_member(room_id TEXT,member_kind TEXT,member_id TEXT,left_at TEXT,joined_at TEXT);
		CREATE TABLE turn(id TEXT,agent_id TEXT,started_at TEXT);
		INSERT INTO machine VALUES ('host',1);
		INSERT INTO machine VALUES ('remote',0);
		INSERT INTO machine_launcher VALUES ('launcher-one','host',0);
		INSERT INTO machine_launcher VALUES ('launcher-container','host',1);
		INSERT INTO checkout VALUES ('checkout-one','/tmp/project');
		INSERT INTO agent VALUES ('agt_one','active','idle','codex','native','launcher-one',0,'checkout-one','host');
		INSERT INTO agent VALUES ('agt_three','stopped','stopped','claude','native','launcher-three',1,'checkout-one','host');
		INSERT INTO agent VALUES ('agt_four','active','idle','codex','native','launcher-container',0,'checkout-one','host');
		INSERT INTO agent VALUES ('agt_five','active','idle','pi','native',NULL,0,'checkout-one','remote');
		INSERT INTO session VALUES ('agt_one',0,NULL,'2026-10-15T00:00:01Z','thread-one');
		INSERT INTO room_member VALUES ('room-one','agent','agt_one',NULL,'2026-10-14T00:00:00Z');
		INSERT INTO turn VALUES ('turn-server','agt_one','2026-10-15T02:00:00Z');`)
	server.Close()
	var stdout, stderr bytes.Buffer
	if code := sessionsCommand(context.Background(), nil, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"AGENT", "agt_one", "local+server", "room-one", "turn-server", "CODEX_HOME=", "codex exec resume 'thread-one'", "agt_two", "turn-only", "agt_three", "stopped/stopped"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"old-thread", "container-thread", "remote-pi-thread"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("unusable resume hint %q leaked:\n%s", forbidden, got)
		}
	}
	rows, err := localSessionRows(context.Background(), filepath.Join(root, "supervisor", "journal.sqlite"))
	if err != nil || len(rows) != 5 {
		t.Fatalf("local launcher decoding: rows=%+v err=%v", rows, err)
	}
	byID := make(map[string]sessionRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	if byID["agt_one"].Launcher != "launcher-one" || !byID["agt_four"].Containerized || byID["agt_four"].Resume != "" {
		t.Fatalf("local launcher decoding: rows=%+v err=%v", rows, err)
	}
}

func TestResumeHintsIncludeSessionLocations(t *testing.T) {
	root := "/tmp/aimebu-test"
	sum := sha256.Sum256([]byte("agent"))
	hash := hex.EncodeToString(sum[:])
	for _, test := range []struct {
		row          sessionRow
		handle, want string
	}{
		{sessionRow{ID: "agent", Tool: "codex", Variant: "native"}, "thread", "CODEX_HOME='" + filepath.Join(root, "supervisor", "codex", hash) + "' codex exec resume 'thread'"},
		{sessionRow{ID: "agent", Tool: "opencode", Variant: "native"}, "ses_abc", "OPENCODE_DB='" + filepath.Join(root, "supervisor", "opencode", hash, "opencode.db") + "' opencode --session 'ses_abc'"},
		{sessionRow{ID: "agent", Tool: "pi", Checkout: "/tmp/project"}, "thread", "pi --session-dir '" + filepath.Join("/tmp/project", ".aimebu", "pi-sessions", hash) + "' --session-id 'thread'"},
	} {
		got := resumeHint(test.row, test.handle, root)
		if got != test.want {
			t.Errorf("resume hint=%q, want %q", got, test.want)
		}
	}
	if got := resumeHint(sessionRow{ID: "agent", Tool: "opencode", Variant: "native", Remote: true}, "ses_abc", root); got != "" {
		t.Fatalf("remote OpenCode path guessed: %q", got)
	}
	if got := resumeHint(sessionRow{ID: "agent", Tool: "codex", Variant: "native", Remote: true}, "thread", root); got != "" {
		t.Fatalf("remote Codex path guessed: %q", got)
	}
	if got := resumeHint(sessionRow{ID: "agent", Tool: "codex", Variant: "docker"}, "thread", root); got != "" {
		t.Fatalf("container Codex path guessed: %q", got)
	}
	if got := resumeHint(sessionRow{ID: "agent", Tool: "codex", Variant: "native", Containerized: true}, "thread", root); got != "" {
		t.Fatalf("custom container Codex path guessed: %q", got)
	}
	if got := resumeHint(sessionRow{ID: "agent", Tool: "pi", Checkout: "/tmp/project", Remote: true}, "thread", root); got != "" {
		t.Fatalf("remote Pi path guessed: %q", got)
	}
}

func fixtureDB(t *testing.T, path, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}
