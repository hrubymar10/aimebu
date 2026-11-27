package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestBackupV1LayoutResumesAndPreservesIndependentState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"server", "agents", "switcher", "usages"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE schema_meta(version INTEGER); INSERT INTO schema_meta VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agents/sessions", "switcher/login", "usages/config.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	partial := filepath.Join(root, v1BackupName+".partial")
	if err := os.Mkdir(partial, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v1BackupManifest{Entries: []string{"agents", "server"}})
	if err := os.WriteFile(filepath.Join(partial, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "server"), filepath.Join(partial, "server")); err != nil {
		t.Fatal(err)
	}
	if err := backupV1Layout(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"server/aimebu.sqlite", "agents/sessions"} {
		if _, err := os.Stat(filepath.Join(root, v1BackupName, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"switcher/login", "usages/config.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial backup remains: %v", err)
	}
	if err := backupV1Layout(root); err != nil {
		t.Fatalf("completed backup changed: %v", err)
	}
}

func TestV1ProbeChildDoesNotOpenHeldStore(t *testing.T) {
	root := os.Getenv("AIMEBU_TEST_V1_PROBE_ROOT")
	if root == "" {
		return
	}
	refuse := func(string) (*sql.DB, error) {
		t.Fatal("v1 probe opened the server database despite another process holding the startup lock")
		return nil, nil
	}
	v1DatabaseOpener, v1LockedDatabaseOpener = refuse, refuse
	v1, err := isV1Root(root)
	if v1 || err != nil {
		t.Fatalf("held v2 store: v1=%t err=%v", v1, err)
	}
}

func TestV1ProbeSkipsStoreHeldByAnotherProcess(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "server", "aimebu.sqlite")
	createFixtureDatabase(t, path)
	lock, err := lockServerDirectory(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockServerDirectory(lock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestV1ProbeChildDoesNotOpenHeldStore$")
	cmd.Env = append(os.Environ(), "AIMEBU_TEST_V1_PROBE_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe child: %v: %s", err, out)
	}
}

func TestBackupV1LayoutRejectsAmbiguousPartial(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, v1BackupName+".partial"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := backupV1Layout(root); err == nil {
		t.Fatal("partial without manifest was accepted")
	}
}

func TestBackupV1LayoutWaitsForDatabaseWriter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "server")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(dir, "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE schema_meta(version INTEGER); INSERT INTO schema_meta VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if err := backupV1Layout(root); err == nil {
		t.Fatal("backup moved a database with an active writer")
	}
	if _, err := os.Stat(filepath.Join(dir, "aimebu.sqlite")); err != nil {
		t.Fatalf("database moved during write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, v1BackupName+".partial", "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("manifest written before database lock: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE last_v1_write(value TEXT); INSERT INTO last_v1_write VALUES ('committed')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := backupV1Layout(root); err != nil {
		t.Fatalf("backup after writer stopped: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, v1BackupName, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1BackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	key, err := v1SourceKey(filepath.Join(root, v1BackupName))
	if err != nil || key != manifest.SourceKey {
		t.Fatalf("source key after final commit: key=%q manifest=%q err=%v", key, manifest.SourceKey, err)
	}
}

func TestBackupV1LayoutPreservesWALSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "server")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(dir, "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE schema_meta(version INTEGER); INSERT INTO schema_meta VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := backupV1Layout(root); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, v1BackupName, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1BackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	key, err := v1SourceKey(filepath.Join(root, v1BackupName))
	if err != nil || key != manifest.SourceKey {
		t.Fatalf("WAL source changed during backup: key=%q manifest=%q err=%v", key, manifest.SourceKey, err)
	}
}

func TestPortCollisionLeavesV1RootUntouched(t *testing.T) {
	t.Setenv("AIMEBU_DEV_SKIP_V1_PROBE", "")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE schema_meta(version INTEGER); INSERT INTO schema_meta VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_PORT", strconv.Itoa(port))
	t.Setenv("AIMEBU_BIND", "127.0.0.1")
	var stdout, stderr bytes.Buffer
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 {
		t.Fatalf("collision code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "server", "aimebu.sqlite")); err != nil {
		t.Fatalf("v1 database moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, v1BackupName)); !os.IsNotExist(err) {
		t.Fatalf("backup created on port collision: %v", err)
	}
	if err := checkV1CutoverPort(root, port+1, port, io.Discard); err == nil {
		t.Fatal("live v1 listener on a separate port was ignored")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkV1CutoverPort(root, port+1, port, io.Discard); err != nil {
		t.Fatalf("stopped v1 listener: %v", err)
	}
	if err := backupV1Layout(root); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, v1BackupName, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1BackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.SourceKey == "" {
		t.Fatalf("missing stable source key: %+v %v", manifest, err)
	}
}

func TestV1CutoverDetectsWildcardListener(t *testing.T) {
	t.Setenv("AIMEBU_DEV_SKIP_V1_PROBE", "")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE schema_meta(version INTEGER); INSERT INTO schema_meta VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := checkV1CutoverPort(root, port+1, port, io.Discard); err == nil {
		t.Fatal("wildcard v1 listener was ignored")
	}
}

// AIMEBU_DEV_SKIP_V1_PROBE=1, and only that exact value, lets a dev instance
// rehearse the cutover on a copy of v1 data while a real v1 keeps listening.
func TestV1CutoverDevSkipProbeEscapeHatch(t *testing.T) {
	root := makeV1Root(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	t.Setenv("AIMEBU_BIND", "")
	for _, value := range []string{"", "0", "true", " 1"} {
		t.Setenv("AIMEBU_DEV_SKIP_V1_PROBE", value)
		var stderr bytes.Buffer
		if err := checkV1CutoverPort(root, port+1, port, &stderr); err == nil {
			t.Fatalf("AIMEBU_DEV_SKIP_V1_PROBE=%q skipped the v1 listener probe", value)
		}
		if stderr.Len() != 0 {
			t.Fatalf("AIMEBU_DEV_SKIP_V1_PROBE=%q warned: %s", value, stderr.String())
		}
	}
	t.Setenv("AIMEBU_DEV_SKIP_V1_PROBE", "1")
	var stderr bytes.Buffer
	if err := checkV1CutoverPort(root, port+1, port, &stderr); err != nil {
		t.Fatalf("escape hatch did not skip the probe: %v", err)
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "AIMEBU_DEV_SKIP_V1_PROBE") || !strings.Contains(got, "skipped") {
		t.Fatalf("missing single warning line: %q", got)
	}
}

// A minimal v1 data directory: a database with v1's schema marker and a file next to it.
func makeV1Root(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE schema_meta(version INTEGER); INSERT INTO schema_meta VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "marker.txt"), []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNonServerWritersRefuseLiveV1Root(t *testing.T) {
	root := makeV1Root(t)
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	var out, errOut bytes.Buffer
	for _, run := range []struct {
		name string
		call func() int
	}{
		{"switcher add", func() int {
			return switcherCommand([]string{"add", "codex", "test"}, strings.NewReader(""), &out, &errOut)
		}},
		{"switcher migrate", func() int { return switcherCommand([]string{"migrate-copies"}, strings.NewReader(""), &out, &errOut) }},
		{"supervisor", func() int { return supervisorCommand(context.Background(), false, &errOut) }},
		{"supervisor pair", func() int {
			return pairSupervisorCommand(context.Background(), "http://127.0.0.1:9997", "test", false, &errOut)
		}},
		{"import-v1", func() int {
			return importV1Command(context.Background(), []string{"--from", root, "--map", filepath.Join(root, "missing.json"), "--to", root}, &out, &errOut)
		}},
	} {
		out.Reset()
		errOut.Reset()
		if got := run.call(); got != 1 || !strings.Contains(errOut.String(), "v1 data") {
			t.Errorf("%s = %d, stderr=%q", run.name, got, errOut.String())
		}
		if strings.HasPrefix(run.name, "supervisor") && !strings.Contains(errOut.String(), "AIMEBU_CONFIG_DIR") {
			t.Errorf("%s did not explain the separate supervisor root: %q", run.name, errOut.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "server", "marker.txt")); err != nil {
		t.Fatal("v1 marker changed:", err)
	}
}

func TestNonServerWriterRefusesInterruptedV1Move(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, v1BackupName+".partial"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := refuseV1RootWrite(root); err == nil || !strings.Contains(err.Error(), "interrupted v1 move") {
		t.Fatalf("partial v1 move accepted: %v", err)
	}
}

// B1: a first start that is certain to fail (no owner login) must not move v1's files first. It used to move them,
// then fail, and a v1 restarted on the empty-looking folder wrote its tables into the v2 database.
func TestFirstStartWithoutOwnerLeavesV1RootUntouched(t *testing.T) {
	t.Setenv("AIMEBU_USAGES", "0")
	useFreePort(t)
	root := makeV1Root(t)
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "")
	var stdout, stderr bytes.Buffer
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "AIMEBU_OWNER_PASSWORD") || !strings.Contains(stderr.String(), "nothing has been moved") {
		t.Fatalf("the failure does not explain itself: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, v1BackupName)); !os.IsNotExist(err) {
		t.Fatalf("a backup exists after a refused start: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, "server", "marker.txt")); err != nil || string(raw) != "v1" {
		t.Fatalf("v1's files were touched: %q %v", raw, err)
	}
	if strings.Contains(stderr.String(), "moved your v1 files") {
		t.Fatalf("the moved notice is printed although nothing moved: %q", stderr.String())
	}
	// A blank login with a password refuses too, and a bad turn limit does, all before the move.
	t.Setenv("AIMEBU_OWNER_LOGIN", "")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "correct horse battery staple")
	stderr.Reset()
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "AIMEBU_OWNER_LOGIN") {
		t.Fatalf("missing login: code=%d %q", code, stderr.String())
	}
	t.Setenv("AIMEBU_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "correct horse battery staple")
	t.Setenv("AIMEBU_TURN_IDLE", "not-a-duration")
	t.Setenv("AIMEBU_EMBEDDED_SUPERVISOR", "1") // the turn limits are the local supervisor's, checked only when it runs
	stderr.Reset()
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "nothing has been moved") {
		t.Fatalf("bad turn limit: code=%d %q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, v1BackupName)); !os.IsNotExist(err) {
		t.Fatalf("a backup exists after refused starts: %v", err)
	}
}

func TestPreflightIsOnlyForTheMoveItProtects(t *testing.T) {
	root := makeV1Root(t)
	if pending, err := v1MovePending(root); err != nil || !pending {
		t.Fatalf("a v1 root with no backup: pending=%v err=%v", pending, err)
	}
	if pending, err := v1MovePending(t.TempDir()); err != nil || pending {
		t.Fatalf("an empty directory is not a v1 root: pending=%v err=%v", pending, err)
	}
	if err := backupV1Layout(root); err != nil {
		t.Fatal(err)
	}
	if pending, err := v1MovePending(root); err != nil || pending {
		t.Fatalf("after the backup nothing is pending: pending=%v err=%v", pending, err)
	}
	if os.Geteuid() != 0 {
		readOnly := t.TempDir()
		if err := os.Chmod(readOnly, 0500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(readOnly, 0700)
		err := preflightV1Cutover(readOnly, func(name string) string {
			return map[string]string{"AIMEBU_OWNER_LOGIN": "o", "AIMEBU_OWNER_PASSWORD": "p"}[name]
		})
		if err == nil || !strings.Contains(err.Error(), "not writable") {
			t.Fatalf("a read-only root passed the preflight: %v", err)
		}
	}
}

// A failure the preflight cannot foresee still says where the files went and warns off starting v1 on the folder.
func TestFailedStartAfterTheMoveNamesTheBackup(t *testing.T) {
	useFreePort(t)
	root := makeV1Root(t)
	failure := errors.New("simulated failure after the move")
	original := afterV1Move
	afterV1Move = func(string) error { return failure }
	defer func() { afterV1Move = original }()
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "correct horse battery staple")
	var stdout, stderr bytes.Buffer
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{filepath.Join(root, v1BackupName), "Do not start v1", "Roll back"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("the notice lacks %q: %q", want, stderr.String())
		}
	}
	if raw, err := os.ReadFile(filepath.Join(root, v1BackupName, "server", "marker.txt")); err != nil || string(raw) != "v1" {
		t.Fatalf("the backup is missing v1's file: %q %v", raw, err)
	}
}

// The registry of external credential targets is carried over after the move; a registry that cannot be carried is
// refused before it.
func TestPreflightRefusesAnUncarriableCredentialRegistry(t *testing.T) {
	t.Setenv("AIMEBU_USAGES", "0")
	useFreePort(t)
	root := makeV1Root(t)
	if err := os.Mkdir(filepath.Join(root, "server", credentialTargetsFile), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "correct horse battery staple")
	var stdout, stderr bytes.Buffer
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "nothing has been moved") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, v1BackupName)); !os.IsNotExist(err) {
		t.Fatalf("the files moved although the registry cannot be carried: %v", err)
	}
}

func TestV1DaemonOnAnotherPortBlocksTheMove(t *testing.T) {
	root := makeV1Root(t)
	pidPath := filepath.Join(root, "server", "aimebu.pid")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(pidPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkV1Daemon(root); err != nil {
		t.Fatalf("no pid file: %v", err)
	}
	// The pid is a live process (this test's own), so what it looks like is decided by its command line, which the test sets.
	holder := exec.Command("sleep", "30")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { holder.Process.Kill(); holder.Wait() }()
	write(strconv.Itoa(holder.Process.Pid))
	original := processInfo
	defer func() { processInfo = original }()
	// Real files stand in for executables, so the resolved executable (when there is one) can be checked on disk.
	bin := filepath.Join(t.TempDir(), "My Tools")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	exeFile := func(name string) string {
		path := filepath.Join(bin, name)
		if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	aimebu, build, vim, cat := exeFile("aimebu"), exeFile("aimebu-1a2b3c4d"), exeFile("vim"), exeFile("cat")
	for _, c := range []struct {
		name, exe, command string
		blocks             bool
	}{
		{"server serve, exe resolved", aimebu, aimebu + " server serve", true},
		{"bin/aimebu build, exe resolved", build, build + " server start", true},
		{"path with spaces, exe resolved", aimebu, aimebu + " server serve --x", true},
		{"path with spaces, exe unknown", "", "/Users/some one/My Tools/aimebu server serve", true},
		{"bin/aimebu build with spaces, exe unknown", "aimebu-1a2b3c4d", "/Users/some one/aimebu-1a2b3c4d server start --x", true},
		{"agent, not a server", aimebu, aimebu + " agent --room general -- claude", false},
		{"agent with a room called server", aimebu, aimebu + " agent --room server -- claude", false},
		{"helper that is not the executable", exeFile("aimebu-backup"), "/usr/local/bin/aimebu-backup", false},
		{"editor on an aimebu path", vim, vim + " /home/me/aimebu/notes", false},
		{"editor with an aimebu path and server as arguments", vim, vim + " /home/me/aimebu server", false},
		{"cat of an aimebu log", cat, cat + " /tmp/aimebu-log server", false},
		{"folder named aimebu, exe unknown", "", "/Users/some one/aimebu notes/vim server.txt", false},
		{"editor with the same arguments, exe unknown", "", "/usr/bin/vim /home/me/aimebu/notes", false},
	} {
		processInfo = func(int) (string, string, error) { return c.exe, c.command, nil }
		if err := checkV1Daemon(root); (err != nil) != c.blocks {
			t.Fatalf("%s: blocks=%v err=%v", c.name, c.blocks, err)
		}
	}
	// This process is v2 itself: a pid file naming it never blocks, whatever its command line says.
	processInfo = func(int) (string, string, error) { return aimebu, aimebu + " server serve", nil }
	write(strconv.Itoa(os.Getpid()))
	if err := checkV1Daemon(root); err != nil {
		t.Fatalf("a pid file that names v2 itself blocked the start: %v", err)
	}
	write(strconv.Itoa(holder.Process.Pid))
	if err := checkV1Daemon(root); err == nil {
		t.Fatal("live v1 daemon was ignored")
	}
	write("not a pid")
	if err := checkV1Daemon(root); err == nil {
		t.Fatal("an unreadable pid file must refuse")
	}
	write("999999999")
	if err := checkV1Daemon(root); err != nil {
		t.Fatalf("a dead pid is stale: %v", err)
	}
	// Once the backup exists the daemon check no longer applies: nothing is left to move.
	write(strconv.Itoa(holder.Process.Pid))
	if err := os.Mkdir(filepath.Join(root, v1BackupName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkV1Daemon(root); err != nil {
		t.Fatalf("after the move: %v", err)
	}
}

func TestPreflightRequiresTheRefresherToolchainForTokenOnlyInstalls(t *testing.T) {
	root := makeV1Root(t)
	env := map[string]string{"AIMEBU_OWNER_LOGIN": "owner", "AIMEBU_OWNER_PASSWORD": "pw"}
	getenv := func(k string) string { return env[k] }
	if err := preflightV1Cutover(root, getenv); err != nil {
		t.Fatalf("a link-based install needs no docker: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "switcher"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "switcher", "token-copies.json"), []byte(`{"version":2,"migrated":{"claude":"2026-10-17T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stubLookPath(t, false)
	if err := preflightV1Cutover(root, getenv); err == nil || !strings.Contains(err.Error(), "harness-docker-ctrl") || !strings.Contains(err.Error(), "nothing has been moved") {
		t.Fatalf("missing harness-docker-ctrl must refuse: %v", err)
	}
	stubLookPath(t, true)
	stubDocker(t, false)
	if err := preflightV1Cutover(root, getenv); err == nil || !strings.Contains(err.Error(), "docker info") {
		t.Fatalf("an unreachable docker must refuse: %v", err)
	}
	stubDocker(t, true)
	if err := preflightV1Cutover(root, getenv); err != nil {
		t.Fatalf("toolchain present: %v", err)
	}
}

// makeInterruptedV1Root is a v1 root whose move into the backup was cut short: the manifest names server and agents,
// server is already in the partial backup and agents is still in the root.
func makeInterruptedV1Root(t *testing.T) string {
	t.Helper()
	root := makeV1Root(t)
	if err := os.Mkdir(filepath.Join(root, "agents"), 0700); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(root, v1BackupName+".partial")
	if err := os.Mkdir(partial, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v1BackupManifest{Entries: []string{"agents", "server"}})
	if err := os.WriteFile(filepath.Join(partial, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "server"), filepath.Join(partial, "server")); err != nil {
		t.Fatal(err)
	}
	return root
}

// Finishing an interrupted move still moves v1's files, so it gets the same preflight and, when the start fails
// afterwards, the same notice: it used to skip both because only a root with no backup at all counted as moving.
func TestResumingAnInterruptedMoveRunsThePreflightAndTheNotice(t *testing.T) {
	t.Setenv("AIMEBU_USAGES", "0")
	useFreePort(t)
	root := makeInterruptedV1Root(t)
	if pending, err := v1MovePending(root); err != nil || !pending {
		t.Fatalf("an interrupted move is still pending: pending=%v err=%v", pending, err)
	}
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_OWNER_LOGIN", "")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "only-a-password-123")
	var stdout, stderr bytes.Buffer
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "AIMEBU_OWNER_LOGIN") || !strings.Contains(stderr.String(), "was not continued") || strings.Contains(stderr.String(), "nothing has been moved") {
		t.Fatalf("the resumed start skipped the preflight or described it wrongly: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "agents")); err != nil {
		t.Fatalf("the refused resume moved a file: %v", err)
	}
	// With an owner set, the resume finishes the move and a failure after it prints the notice.
	failure := errors.New("simulated failure after the move")
	original := afterV1Move
	afterV1Move = func(string) error { return failure }
	defer func() { afterV1Move = original }()
	t.Setenv("AIMEBU_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "correct horse battery staple")
	stderr.Reset()
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{filepath.Join(root, v1BackupName), "Do not start v1", "Roll back"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("the notice lacks %q: %q", want, stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, v1BackupName, "agents")); err != nil {
		t.Fatalf("the resume did not finish the move: %v", err)
	}
}

// A resume that fails after files were already moved (here the manifest's source key no longer matches the database)
// still tells the person where v1's files are: the notice used to be printed only when the whole backup call succeeded.
func TestAResumeThatFailsAfterFilesMovedPrintsTheNotice(t *testing.T) {
	t.Setenv("AIMEBU_USAGES", "0")
	useFreePort(t)
	root := makeInterruptedV1Root(t)
	manifest := filepath.Join(root, v1BackupName+".partial", "manifest.json")
	raw, _ := json.Marshal(v1BackupManifest{Entries: []string{"agents", "server"}, SourceKey: "v1-000000000000000000000000"})
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "correct horse battery staple")
	var stdout, stderr bytes.Buffer
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"source changed", filepath.Join(root, v1BackupName+".partial"), "not finished", "Do not start v1", "Roll back"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("the output lacks %q: %q", want, stderr.String())
		}
	}
	// A failure before anything was moved (nothing in any backup folder) must not claim a move.
	fresh := makeV1Root(t)
	t.Setenv("AIMEBU_CONFIG_DIR", fresh)
	t.Setenv("AIMEBU_OWNER_LOGIN", "")
	stderr.Reset()
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 || strings.Contains(stderr.String(), "moved your v1 files") || strings.Contains(stderr.String(), "started moving") {
		t.Fatalf("a refused start that moved nothing printed a moved notice: code=%d %q", code, stderr.String())
	}
}

// Another process opening the database (or holding it briefly) must not make the start-up check take a v2 database for an
// unidentifiable one: the check waits for the lock instead of failing at once.
func TestIsV1RootWaitsOutABriefLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "server", "aimebu.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	setup, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	if _, err := setup.Exec(`CREATE TABLE schema_version(version INTEGER); INSERT INTO schema_version VALUES (52)`); err != nil {
		t.Fatal(err)
	}
	holder, err := setup.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		_, _ = holder.ExecContext(context.Background(), `COMMIT`)
		_ = holder.Close()
	}()
	v1, err := isV1Root(root)
	if err != nil || v1 {
		t.Fatalf("isV1Root = %v, %v: a briefly locked v2 database must still be recognised", v1, err)
	}
}

// holdExclusive takes an exclusive lock on the database at path for d, in the background.
func holdExclusive(t *testing.T, path string, d time.Duration) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(d)
		_, _ = conn.ExecContext(context.Background(), `COMMIT`)
		_ = conn.Close()
		_ = db.Close()
	}()
}

// Every read-only opener in the commands waits out a brief exclusive lock instead of failing at once.
func TestReadOnlyOpenersWaitOutABriefLock(t *testing.T) {
	makeDB := func(statements ...string) string {
		path := filepath.Join(t.TempDir(), "x.sqlite")
		db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, statement := range statements {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	t.Run("doctor", func(t *testing.T) {
		path := makeDB(`CREATE TABLE schema_version(version INTEGER)`, `INSERT INTO schema_version VALUES (1)`)
		holdExclusive(t, path, 400*time.Millisecond)
		db, err := sql.Open("sqlite", sqliteutil.DSN(path, true))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if check := checkSQLiteDB(path, db); check.Status == statusFail {
			t.Fatalf("doctor failed on a briefly locked database: %s", check.Description)
		}
	})
	t.Run("sessions", func(t *testing.T) {
		path := makeDB(`CREATE TABLE turn_start(agent_id TEXT, turn_id TEXT, generation INT, exec TEXT, started_at TEXT, spec_json TEXT)`,
			`CREATE TABLE journal_event(turn_id TEXT, kind TEXT, payload_json TEXT, created_at TEXT)`)
		holdExclusive(t, path, 400*time.Millisecond)
		if _, err := localSessionRows(context.Background(), path); err != nil {
			t.Fatalf("the session listing failed on a briefly locked journal: %v", err)
		}
	})
}
