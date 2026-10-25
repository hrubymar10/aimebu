package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/server"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
)

// watchServing returns a channel that is closed when the server a test starts next is serving. The listeners are open long before that: a
// start runs the migrations and hashes the owner's password, which on a loaded machine under the race detector takes longer than any
// wait a test could choose, so the test waits for the server to say so instead of polling against a deadline. Call it before starting
// the server; tests that start one set environment variables, so they do not run in parallel and one hook is enough.
func watchServing(t *testing.T) <-chan struct{} {
	t.Helper()
	ready := make(chan struct{})
	var once sync.Once
	hook := func() { once.Do(func() { close(ready) }) }
	servingHook.Store(&hook)
	t.Cleanup(func() { servingHook.Store(nil) })
	return ready
}

// waitServerListening waits for the server to be serving, reporting an early exit immediately, then for its first answer.
func waitServerListening(t *testing.T, url string, ready <-chan struct{}, done <-chan int, stderr *bytes.Buffer) {
	waitServerStatus(t, url, testHTTP, http.StatusUnauthorized, ready, done, stderr)
}

func waitServerStatus(t *testing.T, url string, client *http.Client, wantStatus int, ready <-chan struct{}, done <-chan int, stderr *bytes.Buffer) {
	t.Helper()
	select {
	case <-ready:
	case code := <-done:
		t.Fatalf("server exited before serving: code=%d stderr=%q", code, stderr.String())
	case <-time.After(serverStartLimit):
		dumpGoroutines("the server never started serving")
		t.Fatalf("the server was not serving within %s (stderr %q; the goroutines are on stderr)", serverStartLimit, stderr.String())
	}
	// Serving has begun, so an answer is a matter of scheduling; retry a refused or timed-out first try (a handshake that waited for
	// the scheduler under load) until the server answers or exits, within a bound meant for a hang rather than a slow machine.
	answerBy := time.Now().Add(serverStartLimit)
	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != wantStatus {
				t.Fatalf("server ready status=%d, want %d", resp.StatusCode, wantStatus)
			}
			return
		}
		select {
		case code := <-done:
			t.Fatalf("server exited while answering its first request: code=%d stderr=%q err=%v", code, stderr.String(), err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(answerBy) {
			dumpGoroutines("the server never answered")
			t.Fatalf("the server was serving but did not answer within %s (last error: %v; stderr %q)", serverStartLimit, err, stderr.String())
		}
	}
}

// useFreePort points the server at an unused loopback port. Without it a test that starts the server
// binds the default port, which fails whenever anything else (the dev server, another test run) holds it.
func useFreePort(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	t.Setenv("AIMEBU2_PORT", fmt.Sprint(port))
}

func TestCommandRouting(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	useFreePort(t)
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AIMEBU2_OWNER_LOGIN", "")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "")
	t.Setenv("AIMEBU2_URL", "")
	t.Setenv("AIMEBU2_AGENT_KEY", "")
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"version", []string{"version"}, 0, resolveVersion() + "\n", ""},
		{"missing command", nil, 2, "", "usage: aimebu <server|supervisor|agent-tools|switcher|prune|doctor|sessions|usages|fe|version|help>\n"},
		{"sessions bad flag", []string{"sessions", "--bogus"}, 2, "", "usage: aimebu sessions\n"},
		{"sessions empty", []string{"sessions"}, 0, "No agent sessions found.\n", ""},
		{"doctor bad flag", []string{"doctor", "--bogus"}, 2, "", "usage: aimebu doctor [--json] [--strict] [--machine]\n"},
		{"agent tools needs env", []string{"agent-tools"}, 1, "", "agent-tools requires AIMEBU2_URL and AIMEBU2_AGENT_KEY\n"},
		{"unknown command", []string{"bogus"}, 2, "", "unknown command \"bogus\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode || stdout.String() != tt.wantStdout || stderr.String() != tt.wantStderr {
				t.Fatalf("run(%v) = (%d, %q, %q), want (%d, %q, %q)", tt.args, code, stdout.String(), stderr.String(), tt.wantCode, tt.wantStdout, tt.wantStderr)
			}
		})
	}
}

func TestSupervisorRequiresHostKey(t *testing.T) {
	t.Setenv("AIMEBU2_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"supervisor"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "host-machine.key") {
		t.Fatalf("supervisor without key: code=%d stderr=%q", code, stderr.String())
	}
}

func TestServerBootstrapOnce(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	useFreePort(t)
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := serverCommand(ctx, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Host machine key file:") || strings.Contains(stdout.String(), "amk_") {
		t.Fatalf("first server run = %d, %q, %q", code, stdout.String(), stderr.String())
	}
	keyPath := filepath.Join(root, "server", "host-machine.key")
	keyInfo, err := os.Stat(keyPath)
	if err != nil || keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("host key file: %v, %v", keyInfo, err)
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil || !strings.Contains(string(keyBytes), "amk_") {
		t.Fatalf("host key file unreadable: %v", err)
	}
	path := filepath.Join(root, "server", "aimebu.sqlite")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database permissions: %v, %v", info, err)
	}
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count, err := db.UserCount(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("users after bootstrap = %d, %v", count, err)
	}
	if _, err := auth.NewService(db).Login(context.Background(), "owner", "correct horse battery staple", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	t.Setenv("AIMEBU2_OWNER_LOGIN", "")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "")
	// The first run was several seconds ago on a loaded machine: pick a fresh port rather than hope the earlier one
	// is still free, which another process can take in between.
	useFreePort(t)
	if code := serverCommand(ctx, &stdout, &stderr); code != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "AIMEBU2_ALLOW=127.0.0.0/8,::1/128") {
		t.Fatalf("repeat server run = %d, %q, %q", code, stdout.String(), stderr.String())
	}
}

func TestServerBootstrapRejectsExistingHostKeyWithoutOwner(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	useFreePort(t)
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	dir := filepath.Join(root, "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "host-machine.key")
	if err := os.WriteFile(keyPath, []byte("old key"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := serverCommand(ctx, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "orphaned host machine key") || !strings.Contains(stderr.String(), "safe to delete") {
		t.Fatalf("server = %d, %q, %q", code, stdout.String(), stderr.String())
	}
	key, err := os.ReadFile(keyPath)
	if err != nil || string(key) != "old key" {
		t.Fatalf("existing key changed: %q, %v", key, err)
	}
	db, err := store.Open(context.Background(), filepath.Join(dir, "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count, err := db.UserCount(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("users after failed bootstrap: %d, %v", count, err)
	}
}

func TestServerCommandServesHTTP(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"test-only"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	t.Setenv("AIMEBU2_PORT", fmt.Sprint(port))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	ready := watchServing(t)
	go func() { done <- serverCommand(ctx, &stdout, &stderr) }()
	url := fmt.Sprintf("http://127.0.0.1:%d/api/me", port)
	waitServerListening(t, url, ready, done, &stderr)
	if _, err := os.Stat(filepath.Join(root, "usages", "cache.json")); !os.IsNotExist(err) {
		t.Fatalf("disabled usage reader touched HOME credentials: cache stat = %v", err)
	}
	journalPath := filepath.Join(root, "supervisor", "journal.sqlite")
	credential, err := readHostMachineKey(filepath.Join(root, "server", "host-machine.key"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var lastSeen sql.NullString
		err := probe.QueryRow(`SELECT last_seen FROM machine WHERE id=?`, credential.MachineID).Scan(&lastSeen)
		if err == nil && lastSeen.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("embedded supervisor did not connect: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("embedded journal: %v", err)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve code=%d stderr=%q", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestServerCanDisableEmbeddedSupervisor(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "0")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	t.Setenv("AIMEBU2_PORT", fmt.Sprint(port))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	ready := watchServing(t)
	go func() { done <- serverCommand(ctx, &stdout, &stderr) }()
	waitServerListening(t, fmt.Sprintf("http://127.0.0.1:%d/api/me", port), ready, done, &stderr)
	cancel()
	if code := receive(t, "the server stopping", done, serverStopLimit); code != 0 {
		t.Fatalf("serve code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "supervisor", "journal.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("disabled embedded supervisor created journal: %v", err)
	}
}

func TestEmbeddedSupervisorReconcilesCrashedTurn(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	t.Setenv("AIMEBU2_PORT", fmt.Sprint(port))
	var stdout, stderr bytes.Buffer
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if code := serverCommand(stopped, &stdout, &stderr); code != 0 {
		t.Fatalf("bootstrap code=%d stderr=%q", code, stderr.String())
	}
	credential, err := readHostMachineKey(filepath.Join(root, "server", "host-machine.key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "server", "aimebu.sqlite")
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project, err := db.CreateProject(ctx, store.Project{Name: "crash", Folder: root})
	if err != nil {
		t.Fatal(err)
	}
	room, err := db.CreateRoom(ctx, store.Room{ProjectID: project.ID, Kind: "task", Title: "crash", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	checkoutID := "chk_crash"
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES (?,?,?,?)`, checkoutID, project.ID, credential.MachineID, root)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateAgent(ctx, store.Agent{Slug: "crash", ProjectID: project.ID, RoleID: "role_reviewer_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: credential.MachineID, CheckoutID: checkoutID, KeyHash: "hash", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddMember(ctx, room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_crash',?,0,'fresh_with_history','2026-10-13T00:00:00.000000000Z')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	turn, err := db.RecordTurn(ctx, store.Turn{AgentID: agent.ID, SessionID: "ses_crash", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: credential.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if err := db.AckTurn(ctx, turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(root, "supervisor", "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RecordStart(ctx, supervisor.StartRecord{TurnID: turn.ID, AgentID: agent.ID, Generation: 0, DesiredRev: 0, Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	effectPath := filepath.Join(root, "effect.txt")
	if err := os.WriteFile(effectPath, []byte("once"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Keep one read-only WAL reader for the restart probe. Reopening and
	// migrating the store on every poll contends with the server's startup.
	probe, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	probe.SetMaxOpenConns(1)
	stdout.Reset()
	stderr.Reset()
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serverCommand(runCtx, &stdout, &stderr) }()
	// Observe the actual reconciliation edge. Opening the server and its
	// embedded supervisor can take longer under race-enabled parallel suites.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var observed string
		var blocked bool
		err := probe.QueryRowContext(ctx, `SELECT observed,recovery_block FROM agent WHERE id=?`, agent.ID).Scan(&observed, &blocked)
		if err == nil && observed == "lost" && blocked {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("server exited before reconciliation: code=%d stderr=%q", code, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			receive(t, "the server stopping", done, serverStopLimit)
			t.Fatalf("crashed turn was not reconciled: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := receive(t, "the server stopping", done, serverStopLimit); code != 0 {
		t.Fatalf("restart code=%d stderr=%q", code, stderr.String())
	}
	contents, err := os.ReadFile(effectPath)
	if err != nil || string(contents) != "once" {
		t.Fatalf("effect reran: %q %v", contents, err)
	}
	var turns int
	if err := probe.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=?`, agent.ID).Scan(&turns); err != nil || turns != 1 {
		t.Fatalf("duplicate turn count=%d %v", turns, err)
	}
	var recoveryItems int
	if err := probe.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE turn_id=? AND kind='recovery' AND state='open'`, turn.ID).Scan(&recoveryItems); err != nil || recoveryItems != 1 {
		t.Fatalf("restart recovery inbox items=%d %v", recoveryItems, err)
	}
}

func TestV2DefaultsIgnoreV1EnvironmentAndData(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, ".aimebu")
	if err := os.MkdirAll(filepath.Join(legacy, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	legacyDB := filepath.Join(legacy, "server", "aimebu.sqlite")
	sentinel := []byte("v1 database sentinel")
	if err := os.WriteFile(legacyDB, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU_CONFIG_DIR", legacy)
	t.Setenv("AIMEBU_BIND", "not-an-ip")
	t.Setenv("AIMEBU_PORT", "1")
	t.Setenv("AIMEBU_ALLOW", "not-an-ip")
	t.Setenv("AIMEBU_OWNER_LOGIN", "v1-owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "v1-password")
	t.Setenv("AIMEBU2_CONFIG_DIR", "")
	t.Setenv("AIMEBU2_BIND", "")
	t.Setenv("AIMEBU2_PORT", "")
	t.Setenv("AIMEBU2_ALLOW", "")
	t.Setenv("AIMEBU2_OWNER_LOGIN", "v2-owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "v2-password-long")
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.root != filepath.Join(home, ".aimebu2") || config.bind != "127.0.0.1" || config.port != 9897 {
		t.Fatalf("v2 defaults: %+v", config)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	t.Setenv("AIMEBU2_PORT", fmt.Sprint(port))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := serverCommand(ctx, &stdout, &stderr); code != 0 {
		t.Fatalf("v2 start=%d stderr=%q", code, stderr.String())
	}
	got, err := os.ReadFile(legacyDB)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("v1 data touched: %q %v", got, err)
	}
	v2DB := filepath.Join(home, ".aimebu2", "server", "aimebu.sqlite")
	db, err := store.Open(context.Background(), v2DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UserByLogin(context.Background(), "v2-owner"); err != nil {
		t.Fatalf("v2 owner missing: %v", err)
	}
	if _, err := db.UserByLogin(context.Background(), "v1-owner"); err == nil {
		t.Fatal("v1 credentials leaked into v2")
	}
}

func TestResolveServerConfigAllowAndStartupWarning(t *testing.T) {
	t.Setenv("AIMEBU2_BIND", "0.0.0.0")
	t.Setenv("AIMEBU2_ALLOW", "")
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logAllowStartup(&output, config, "0.0.0.0:9897")
	if !strings.Contains(output.String(), "AIMEBU2_ALLOW=127.0.0.0/8,::1/128") || !strings.Contains(output.String(), "remote clients will receive 403") {
		t.Fatalf("default allowlist startup log: %q", output.String())
	}
	t.Setenv("AIMEBU2_ALLOW", "127.0.0.1,172.28.47.0/24")
	config, err = resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	logAllowStartup(&output, config, "0.0.0.0:9897")
	if !strings.Contains(output.String(), "AIMEBU2_ALLOW=127.0.0.1/32,172.28.47.0/24") || strings.Contains(output.String(), "WARNING") {
		t.Fatalf("custom allowlist startup log: %q", output.String())
	}
	t.Setenv("AIMEBU2_ALLOW", "localhost")
	if _, err := resolveServerConfig(); err == nil {
		t.Fatal("accepted hostname allowlist")
	}
}

func TestAllowEmbeddedSupervisorAtItsDialAddress(t *testing.T) {
	for _, test := range []struct {
		bind, want string
	}{
		{"0.0.0.0", "127.0.0.1"},
		{"::", "::1"},
		{"192.0.2.10", "192.0.2.10"},
	} {
		t.Run(test.bind, func(t *testing.T) {
			t.Setenv("AIMEBU2_BIND", test.bind)
			t.Setenv("AIMEBU2_ALLOW", "127.0.0.0/8,::1/128")
			config, err := resolveServerConfig()
			if err != nil {
				t.Fatal(err)
			}
			if got := allowEmbeddedSupervisor(&config); got != test.want {
				t.Fatalf("dial address=%q want %q", got, test.want)
			}
			ip := netip.MustParseAddr(test.want)
			allowed := false
			for _, prefix := range config.allow {
				allowed = allowed || prefix.Contains(ip)
			}
			if !allowed {
				t.Fatalf("embedded source %s excluded by %v", ip, config.allow)
			}
		})
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{arg}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("run(%s) = (%d, %q)", arg, code, stderr.String())
		}
		for _, command := range []string{"server", "supervisor", "agent-tools", "doctor", "sessions", "usages", "switcher", "prune", "version", "help", "AIMEBU2_CONFIG_DIR"} {
			if !strings.Contains(stdout.String(), command) {
				t.Fatalf("help (%s) lacks %q:\n%s", arg, command, stdout.String())
			}
		}
	}
}

func TestResolveVersionKeepsLdflagsOverride(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })
	version = "aimebu v9.9.9"
	if got := resolveVersion(); got != "aimebu v9.9.9" {
		t.Fatalf("resolveVersion() = %q", got)
	}
	version = saved
	if got := resolveVersion(); !strings.HasPrefix(got, saved) {
		t.Fatalf("resolveVersion() = %q, want prefix %q", got, saved)
	}
}

// A start that fails after the token-copy goroutines began must still cancel and join them: the early returns used to
// leave them running, with their claims, behind a process that was exiting.
func TestAFailedStartJoinsTheTokenCopyGoroutines(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	useFreePort(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	// The embedded supervisor's journal cannot be opened under a regular file: the start fails after the server wiring.
	if err := os.WriteFile(filepath.Join(root, "supervisor"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	joined := 0
	original := joinTokenCopies
	joinTokenCopies = func(s *server.Server) { joined++; original(s) }
	defer func() { joinTokenCopies = original }()
	var stdout, stderr bytes.Buffer
	if code := serverCommand(t.Context(), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if joined == 0 {
		t.Fatalf("the failed start returned without joining the token-copy goroutines: %q", stderr.String())
	}
}

func TestListenErrorDoesNotBlameV1ForAnyBusyPort(t *testing.T) {
	t.Parallel()
	busy := listenError("127.0.0.1", 9997, &net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}})
	for _, want := range []string{"port 9997 is already in use by another process", "If it is the v1 aimebu server", "AIMEBU2_PORT"} {
		if !strings.Contains(busy, want) {
			t.Errorf("busy-port message %q lacks %q", busy, want)
		}
	}
	if strings.HasPrefix(busy, "stop the v1") {
		t.Errorf("a busy port is not necessarily v1: %q", busy)
	}
	other := listenError("127.0.0.1", 80, &net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EACCES}})
	if strings.Contains(other, "v1") {
		t.Errorf("a permission error must not mention v1: %q", other)
	}
}
