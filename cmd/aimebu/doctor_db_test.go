package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

var doctorNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// doctorFixture is a real migrated database under a temp config root, plus a
// write handle with foreign keys off so a test can insert the half-made rows a
// check is meant to catch.
type doctorFixture struct {
	t    *testing.T
	root string
	db   *sql.DB
}

func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "server", "aimebu.sqlite")
	createFixtureDatabase(t, path)
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false)+"&_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return &doctorFixture{t: t, root: root, db: db}
}

func (f *doctorFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

func (f *doctorFixture) machine(id string, approved, revoked bool, lastSeen time.Time) {
	f.t.Helper()
	var seen, revokedAt any
	if !lastSeen.IsZero() {
		seen = stamp(lastSeen)
	}
	if revoked {
		revokedAt = stamp(doctorNow)
	}
	f.exec(`INSERT INTO machine(id,name,key_hash,approved,revoked_at,last_seen) VALUES (?,?,?,?,?,?)`, id, "name-"+id, "hash", approved, revokedAt, seen)
}

func (f *doctorFixture) agent(id, machine, desired, observed string, desiredRev int) {
	f.t.Helper()
	f.exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,desired,desired_rev,observed,created_by,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, id, "prj", "role", "claude", "v", "m", "p", machine, "co", "h", desired, desiredRev, observed, "u", stamp(doctorNow))
}

func (f *doctorFixture) run(serverUp bool) map[string][]doctorCheck {
	f.t.Helper()
	got := map[string][]doctorCheck{}
	for _, check := range doctorServerChecks(f.root, doctorNow, serverUp, true) {
		got[check.Name] = append(got[check.Name], check)
	}
	return got
}

func only(t *testing.T, got map[string][]doctorCheck, name string) doctorCheck {
	t.Helper()
	checks := got[name]
	if len(checks) != 1 {
		t.Fatalf("check %q: got %d results %+v", name, len(checks), checks)
	}
	return checks[0]
}

func TestDoctorServerChecksFreshInstallNeverWarnsOrFails(t *testing.T) {
	f := newDoctorFixture(t)
	for name, checks := range f.run(true) {
		for _, check := range checks {
			if check.Status != statusOK {
				t.Errorf("fresh install: %s is %s: %s", name, check.Status, check.Description)
			}
			if check.Scope != scopeServer {
				t.Errorf("%s scope = %q, want %q", name, check.Scope, scopeServer)
			}
		}
	}
	// Even with no database at all (server never started) the offline checks
	// stay at ok/warn.
	for _, check := range doctorServerChecks(t.TempDir(), doctorNow, false, false) {
		if check.Status == statusFail {
			t.Errorf("no database: %s failed: %s", check.Name, check.Description)
		}
	}
}

func TestCheckDiskSpaceThresholds(t *testing.T) {
	root := t.TempDir()
	original := diskFreeBytes
	t.Cleanup(func() { diskFreeBytes = original })
	for _, tt := range []struct {
		free uint64
		want string
	}{
		{10 << 30, statusOK},
		{2 << 30, statusOK},
		{2<<30 - 1, statusWarn},
		{200 << 20, statusWarn},
		{200<<20 - 1, statusFail},
		{0, statusFail},
	} {
		diskFreeBytes = func(string) (uint64, error) { return tt.free, nil }
		check := checkDiskSpace(root)
		if check.Status != tt.want {
			t.Errorf("free %d: status %s (%s), want %s", tt.free, check.Status, check.Description, tt.want)
		}
		if tt.want != statusOK && check.Hint == "" {
			t.Errorf("free %d: no hint on %s", tt.free, check.Status)
		}
	}
	diskFreeBytes = func(string) (uint64, error) { return 0, fmt.Errorf("boom") }
	if check := checkDiskSpace(root); check.Status != statusWarn {
		t.Errorf("unreadable disk: %s, want warn", check.Status)
	}
}

func TestCheckForeignKeys(t *testing.T) {
	f := newDoctorFixture(t)
	if got := only(t, f.run(true), "foreign keys"); got.Status != statusOK {
		t.Fatalf("clean database: %+v", got)
	}
	f.agent("a1", "no-such-machine", "active", "idle", 0)
	got := only(t, f.run(true), "foreign keys")
	if got.Status != statusFail || !strings.Contains(got.Description, "agent→machine") || got.Hint == "" {
		t.Fatalf("dangling reference: %+v", got)
	}
}

// writeBackup stores a verified-looking backup: a real SQLite file named the
// way the migration writer names it.
func writeBackup(t *testing.T, dir string, at time.Time, from, to int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("aimebu-pre-migrate-%d-to-%d-%s.sqlite", from, to, at.UTC().Format("20060102T150405.000000000Z")))
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(x); INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckBackups(t *testing.T) {
	t.Run("no directory is ok", func(t *testing.T) {
		check := checkBackups(filepath.Join(t.TempDir(), "backups"), doctorNow)
		if check.Status != statusOK {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("valid newest backup", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "backups")
		writeBackup(t, dir, doctorNow.Add(-72*time.Hour), 1, 2)
		writeBackup(t, dir, doctorNow.Add(-2*time.Hour), 2, 3)
		check := checkBackups(dir, doctorNow)
		if check.Status != statusOK || !strings.Contains(check.Description, "2 backup(s)") || !strings.Contains(check.Description, "quick_check ok") || !strings.Contains(check.Description, "2h0m0s ago") {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("corrupt newest backup fails", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "backups")
		path := writeBackup(t, dir, doctorNow.Add(-time.Hour), 1, 2)
		if err := os.WriteFile(path, []byte(strings.Repeat("not sqlite ", 100)), 0o600); err != nil {
			t.Fatal(err)
		}
		check := checkBackups(dir, doctorNow)
		if check.Status != statusFail || check.Hint == "" {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("oversized backup is not verified", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "backups")
		writeBackup(t, dir, doctorNow.Add(-time.Hour), 1, 2)
		original := backupQuickCheckMaxBytes
		backupQuickCheckMaxBytes = 1
		t.Cleanup(func() { backupQuickCheckMaxBytes = original })
		check := checkBackups(dir, doctorNow)
		if check.Status != statusWarn || !strings.Contains(check.Description, "not verified") {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("interrupted partial file warns", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "backups")
		path := writeBackup(t, dir, doctorNow.Add(-time.Hour), 1, 2)
		if err := os.WriteFile(path+".partial", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		check := checkBackups(dir, doctorNow)
		if check.Status != statusWarn || !strings.Contains(check.Description, ".partial") {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("partial file alone", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "backups")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "aimebu-pre-migrate-1-to-2-20261025T100000.000000000Z.sqlite.partial"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if check := checkBackups(dir, doctorNow); check.Status != statusWarn {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("directory readable by others warns", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "backups")
		writeBackup(t, dir, doctorNow.Add(-time.Hour), 1, 2)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		check := checkBackups(dir, doctorNow)
		if check.Status != statusWarn || !strings.Contains(check.Hint, "chmod 700") {
			t.Fatalf("%+v", check)
		}
	})
}

func TestCheckMachines(t *testing.T) {
	t.Run("none paired is ok", func(t *testing.T) {
		f := newDoctorFixture(t)
		if got := only(t, f.run(true), "machines"); got.Status != statusOK || !strings.Contains(got.Description, "none") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("recent and stale machines", func(t *testing.T) {
		f := newDoctorFixture(t)
		f.machine("m-fresh", true, false, doctorNow.Add(-time.Minute))
		f.machine("m-stale", true, false, doctorNow.Add(-3*time.Hour))
		f.machine("m-never", true, false, time.Time{})
		f.machine("m-pending", false, false, time.Time{})
		got := only(t, f.run(true), "machines")
		if got.Status != statusWarn || !strings.Contains(got.Description, "m-stale") || !strings.Contains(got.Description, "unseen for 3h0m0s") ||
			!strings.Contains(got.Description, "m-never") || strings.Contains(got.Description, "m-fresh") || strings.Contains(got.Description, "m-pending") || got.Hint == "" {
			t.Fatalf("%+v", got)
		}
		// With the server down nothing says a machine is late: nothing is
		// there to receive its pings.
		if got := only(t, f.run(false), "machines"); got.Status != statusOK || !strings.Contains(got.Description, "liveness is not judged") {
			t.Fatalf("server down: %+v", got)
		}
	})
	t.Run("revoked machine with live agents", func(t *testing.T) {
		f := newDoctorFixture(t)
		f.machine("m-gone", true, true, doctorNow.Add(-time.Hour))
		f.agent("a-live", "m-gone", "active", "idle", 0)
		f.agent("a-stopped", "m-gone", "stopped", "stopped", 1)
		got := f.run(true)
		if only(t, got, "machines").Status != statusOK {
			t.Fatalf("revoked machine alone must not warn about liveness: %+v", got["machines"])
		}
		revoked := only(t, got, "revoked machines")
		if revoked.Status != statusWarn || !strings.Contains(revoked.Description, "1 agent(s)") {
			t.Fatalf("%+v", revoked)
		}
	})
}

func TestCheckPendingStops(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		f := newDoctorFixture(t)
		if got := only(t, f.run(true), "pending stops"); got.Status != statusOK {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("stale machine holds a stop", func(t *testing.T) {
		f := newDoctorFixture(t)
		f.machine("m-stale", true, false, doctorNow.Add(-2*time.Hour))
		f.agent("a1", "m-stale", "stopped", "idle", 3)
		got := only(t, f.run(true), "pending stops")
		if got.Status != statusWarn || !strings.Contains(got.Description, "409") || !strings.Contains(got.Description, "m-stale") || got.Hint == "" {
			t.Fatalf("%+v", got)
		}
		// Server down: a machine cannot be judged late, so the stop is just in flight.
		if got := only(t, f.run(false), "pending stops"); got.Status != statusOK {
			t.Fatalf("server down: %+v", got)
		}
	})
	t.Run("connected machine has it in flight", func(t *testing.T) {
		f := newDoctorFixture(t)
		f.machine("m-ok", true, false, doctorNow.Add(-5*time.Second))
		f.agent("a1", "m-ok", "stopped", "in_turn", 3)
		got := only(t, f.run(true), "pending stops")
		if got.Status != statusOK || !strings.Contains(got.Description, "in flight") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("revoked machine holds a stop", func(t *testing.T) {
		f := newDoctorFixture(t)
		f.machine("m-gone", true, true, doctorNow.Add(-time.Minute))
		f.agent("a1", "m-gone", "stopped", "idle", 3)
		if got := only(t, f.run(true), "pending stops"); got.Status != statusWarn || !strings.Contains(got.Description, "revoked") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("applied stop is not pending", func(t *testing.T) {
		f := newDoctorFixture(t)
		f.machine("m-stale", true, false, doctorNow.Add(-2*time.Hour))
		f.agent("a1", "m-stale", "stopped", "stopped", 3)
		f.exec(`INSERT INTO event(id,seq,kind,agent_id,payload_json,created_at) VALUES ('e1',1,'agent_control_applied','a1','{"desired_rev":3}',?)`, stamp(doctorNow))
		if got := only(t, f.run(true), "pending stops"); got.Status != statusOK || got.Description != "none waiting" {
			t.Fatalf("%+v", got)
		}
	})
}

func TestCheckLaunchers(t *testing.T) {
	launcher := func(f *doctorFixture, id string, enabled bool) {
		f.exec(`INSERT INTO machine_launcher(id,machine_id,tool,name,label,command,enabled) VALUES (?,?,?,?,?,?,?)`, id, "m", "claude", id, "Label "+id, "claude", enabled)
	}
	model := func(f *doctorFixture, id, slug string, isDefault bool) {
		f.exec(`INSERT INTO harness_model(launcher_id,slug,label,is_default,position) VALUES (?,?,?,?,0)`, id, slug, slug, isDefault)
	}
	t.Run("none is ok", func(t *testing.T) {
		f := newDoctorFixture(t)
		if got := only(t, f.run(true), "launchers"); got.Status != statusOK {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("healthy", func(t *testing.T) {
		f := newDoctorFixture(t)
		launcher(f, "good", true)
		model(f, "good", "sonnet", true)
		launcher(f, "off", false) // disabled launchers are not judged
		if got := only(t, f.run(true), "launchers"); got.Status != statusOK {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("no models and no default", func(t *testing.T) {
		f := newDoctorFixture(t)
		launcher(f, "empty", true)
		launcher(f, "nodef", true)
		model(f, "nodef", "sonnet", false)
		got := f.run(true)["launchers"]
		if len(got) != 2 || got[0].Status != statusWarn || got[1].Status != statusWarn ||
			!strings.Contains(got[0].Description, "no models: Label empty") || !strings.Contains(got[1].Description, "no default: Label nodef") {
			t.Fatalf("%+v", got)
		}
	})
}

func TestCheckProviderLimits(t *testing.T) {
	hold := func(f *doctorFixture, agent, tool string, reset any) {
		f.exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,reset_at,tool) VALUES (?,?,?,?,?)`, agent, "t-"+agent, stamp(doctorNow), reset, tool)
	}
	t.Run("none", func(t *testing.T) {
		f := newDoctorFixture(t)
		if got := only(t, f.run(true), "provider limits"); got.Status != statusOK {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("active and expired holds", func(t *testing.T) {
		f := newDoctorFixture(t)
		hold(f, "a1", "claude", stamp(doctorNow.Add(3*time.Hour)))
		hold(f, "a2", "claude", stamp(doctorNow.Add(-time.Hour))) // already reset
		hold(f, "a3", "codex", nil)
		got := only(t, f.run(true), "provider limits")
		if got.Status != statusWarn || !strings.Contains(got.Description, "1 claude agent(s) until 2026-10-25 15:00 UTC") || !strings.Contains(got.Description, "1 codex agent(s) (no reset time known)") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("only expired holds is ok", func(t *testing.T) {
		f := newDoctorFixture(t)
		hold(f, "a1", "claude", stamp(doctorNow.Add(-time.Hour)))
		if got := only(t, f.run(true), "provider limits"); got.Status != statusOK {
			t.Fatalf("%+v", got)
		}
	})
}

// treeDigest lists every file under root with its mode, content hash and
// modification time, so a test can prove a run changed nothing. Directory
// times and SQLite's -wal/-shm sidecars are left out: opening a WAL-mode
// database read-only creates the (empty) sidecars even when nothing is written
// to the database itself.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") {
			return nil
		}
		if entry.IsDir() {
			fmt.Fprintf(&out, "%s %v\n", path, info.Mode())
			return nil
		}
		content := ""
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			content = fmt.Sprintf("%x", sha256.Sum256(data))
		}
		fmt.Fprintf(&out, "%s %v %d %d %s\n", path, info.Mode(), info.Size(), info.ModTime().UnixNano(), content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestDoctorNeverWrites(t *testing.T) {
	f := newDoctorFixture(t)
	f.machine("m1", true, false, doctorNow.Add(-time.Minute))
	f.db.Close()
	writeBackup(t, filepath.Join(f.root, "server", "backups"), doctorNow.Add(-time.Hour), 1, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	t.Setenv("AIMEBU2_CONFIG_DIR", f.root)
	t.Setenv("AIMEBU2_BIND", "127.0.0.1")
	t.Setenv("AIMEBU2_PORT", strconv.Itoa(testServerPort(t, server.URL)))
	t.Setenv("AIMEBU2_ALLOW", "")
	t.Setenv("AIMEBU2_TLS_CERT", "")
	t.Setenv("AIMEBU2_TLS_KEY", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), ".codex"))
	before := treeDigest(t, f.root)
	var stdout, stderr bytes.Buffer
	doctorCommand(context.Background(), nil, &stdout, &stderr)
	if after := treeDigest(t, f.root); after != before {
		t.Fatalf("doctor changed the config root\nbefore:\n%s\nafter:\n%s\noutput:\n%s", before, after, stdout.String())
	}
	if !strings.Contains(stdout.String(), "disk space") || !strings.Contains(stdout.String(), "backups") {
		t.Fatalf("offline checks missing from output:\n%s", stdout.String())
	}
}

func TestDoctorStrictTurnsWarningsIntoFailures(t *testing.T) {
	f := newDoctorFixture(t)
	f.machine("m-stale", true, false, time.Now().Add(-3*time.Hour))
	f.db.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	t.Setenv("AIMEBU2_CONFIG_DIR", f.root)
	t.Setenv("AIMEBU2_BIND", "127.0.0.1")
	t.Setenv("AIMEBU2_PORT", strconv.Itoa(testServerPort(t, server.URL)))
	t.Setenv("AIMEBU2_ALLOW", "")
	t.Setenv("AIMEBU2_TLS_CERT", "")
	t.Setenv("AIMEBU2_TLS_KEY", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), ".codex"))
	run := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := doctorCommand(context.Background(), args, &stdout, &stderr)
		return code, stdout.String()
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "→ Start the supervisor") {
		t.Fatalf("plain run: exit %d, want 0 with the hint printed\n%s", code, out)
	}
	if code, _ := run("--strict"); code != 1 {
		t.Fatalf("--strict with a warning: exit %d, want 1", code)
	}
	code, out := run("--strict", "--json")
	var decoded doctorResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil || code != 1 || decoded.ExitCode != 1 {
		t.Fatalf("--strict --json: exit %d, decoded exit %d, err %v\n%s", code, decoded.ExitCode, err, out)
	}
	hinted := false
	for _, check := range decoded.Checks {
		hinted = hinted || (check.Name == "machines" && check.Hint != "")
	}
	if !hinted {
		t.Fatalf("JSON has no hint on the machines warning:\n%s", out)
	}
}
