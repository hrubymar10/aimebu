package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
	_ "modernc.org/sqlite"
)

func init() {
	fakeAdapterHook = func(fake *adapter.Fake) {
		path := os.Getenv("AIMEBU_TEST_FAKE_GATE")
		if path == "" {
			return
		}
		gate := make(chan struct{})
		fake.SetEchoGate(gate)
		go func() {
			for {
				if _, err := os.Stat(path); err == nil {
					close(gate)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
}

func TestLocalSupervisorRejectsNonLoopbackURL(t *testing.T) {
	for _, raw := range []string{"http://example.com:9997", "http://192.0.2.1:9997", "http://localhost:9997", "http://127.0.0.1:9997/else"} {
		if err := localDialURL(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if err := localDialURL("http://127.0.0.1:9997"); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorRestartDoesNotFallBackWhileServerHoldsLock(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := os.WriteFile(filepath.Join(dir, "doctor.key"), []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	answered, err := askServerToRestartSupervisor(serverConfig{root: root, bind: "127.0.0.1", port: port})
	if !answered || err == nil || !strings.Contains(err.Error(), "try again or restart the server") {
		t.Fatalf("running but unreachable server: answered=%v err=%v", answered, err)
	}
}

func TestConnectedSupervisorSkipsJournalOwnerProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stderr bytes.Buffer
	m := detachedSupervisorManager{config: serverConfig{root: filepath.Join(t.TempDir(), "missing")}, stderr: &stderr,
		connected: func() bool { cancel(); return true }}
	m.run(ctx)
	if stderr.Len() != 0 {
		t.Fatalf("healthy connection probed the journal owner: %s", stderr.String())
	}
}

func TestMatchingSupervisorCancelsLegacyDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	m := detachedSupervisorManager{legacyDrain: &legacyDrainState{cancel: cancel}}
	m.cancelLegacyDrain()
	if m.legacyDrain != nil || ctx.Err() == nil {
		t.Fatal("legacy drain remained active after a matching hello")
	}
}

func TestDetachedSupervisorRespawnsAfterCrash(t *testing.T) {
	root, _ := daemonFixture(t)
	t.Setenv("AIMEBU_EMBEDDED_SUPERVISOR", "1")
	t.Setenv("AIMEBU_SUPERVISOR", "detached")
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start: %d %q %q", code, out, errOut)
	}
	t.Cleanup(func() {
		if pid, held, err := supervisorPID(root); err == nil && held {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_, _, _ = runServerAction("stop")
	})
	deadline := time.Now().Add(30 * time.Second)
	first := 0
	for time.Now().Before(deadline) {
		if pid, held, err := supervisorPID(root); err == nil && held {
			first = pid
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if first == 0 {
		t.Fatal("first supervisor did not start")
	}
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if pid, held, err := supervisorPID(root); err == nil && held && pid != first {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("supervisor did not respawn: %s", tailOfFile(serverLogPath(root), 25))
}

// A real detached child holds its journal and its fake turn while the server
// exits. Its answer reaches the same store after the server returns.
func TestDetachedSupervisorCompletesTurnAcrossServerRestart(t *testing.T) {
	testDetachedSupervisorAcrossServerRestart(t, false, false)
}

func TestDetachedSupervisorDrainsForEnvironmentChange(t *testing.T) {
	testDetachedSupervisorAcrossServerRestart(t, true, false)
}

func TestDetachedSupervisorRestartNowContinuesTurn(t *testing.T) {
	testDetachedSupervisorAcrossServerRestart(t, false, true)
}

func testDetachedSupervisorAcrossServerRestart(t *testing.T, changeEnvironment, restartNow bool) {
	root, _ := daemonFixture(t)
	t.Setenv("AIMEBU_SUPERVISOR", "detached")
	t.Setenv("AIMEBU_ALLOW", "192.0.2.0/24") // The host loopback address must be admitted even when people set a LAN-only list.
	gate := filepath.Join(t.TempDir(), "release-fake-answer")
	t.Setenv("AIMEBU_TEST_FAKE_GATE", gate)
	// The agent is seeded straight into the store, behind the server's back. A
	// supervisor connection binds the agents that exist when its hello is
	// read (AgentsForMachine); a real spawn binds a later agent on the live
	// connection, a row written by the test does not. Were the supervisor to
	// say hello before the seed, every delivery would end in
	// ErrStaleGeneration and nothing would run (it did, under load). So the
	// first start, without a local supervisor (daemonFixture's
	// AIMEBU_EMBEDDED_SUPERVISOR=0), only creates the store and the host key;
	// the seed is written while no supervisor can be connected, and the
	// detached supervisor starts with the server after it.
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start without a supervisor: %d %q %q", code, out, errOut)
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
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, store.Project{Name: "restart", Folder: root})
	if err != nil {
		t.Fatal(err)
	}
	room, err := db.CreateRoom(ctx, store.Room{ProjectID: project.ID, Kind: "task", Title: "restart", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_restart',?,?,?)`, project.ID, credential.MachineID, root)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateAgent(ctx, store.Agent{Slug: "worker", ProjectID: project.ID, RoleID: "role_reviewer_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: credential.MachineID, CheckoutID: "chk_restart", KeyHash: "hash", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddMember(ctx, room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_restart',?,0,'fresh_with_history',?)`, agent.ID, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	message, err := db.InsertMessage(ctx, store.Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@worker finish after restart"})
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runServerAction("stop"); code != 0 {
		t.Fatalf("stop after seeding: %d %q %q", code, out, errOut)
	}
	t.Setenv("AIMEBU_EMBEDDED_SUPERVISOR", "1")
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start: %d %q %q", code, out, errOut)
	}
	var supervisorPIDAtStart int
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pid, held, err := supervisorPID(root)
		if err == nil && held {
			supervisorPIDAtStart = pid
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if supervisorPIDAtStart == 0 {
		t.Fatalf("detached supervisor never held the lock: %s", tailOfFile(filepath.Join(root, "supervisor", "aimebu.log"), 20))
	}
	t.Cleanup(func() {
		if pid, held, err := supervisorPID(root); err == nil && held {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	probe, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var running int
		if err := probe.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, agent.ID).Scan(&running); err == nil && running > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	var running int
	if err := probe.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, agent.ID).Scan(&running); err != nil || running == 0 {
		var turns, notes string
		_ = probe.QueryRow(`SELECT COALESCE(group_concat(id||':'||exec,','),'') FROM turn WHERE agent_id=?`, agent.ID).Scan(&turns)
		_ = probe.QueryRow(`SELECT COALESCE(group_concat(state,','),'') FROM note WHERE agent_id=?`, agent.ID).Scan(&notes)
		// The supervisor's stdout and stderr are its log; whether it is still alive says if an empty log means it never ran or was never asked.
		pid, held, pidErr := supervisorPID(root)
		t.Fatalf("fake turn did not start: running=%d err=%v turns=%q notes=%q supervisor pid=%d held=%t err=%v; server log: %s; supervisor log: %s", running, err, turns, notes, pid, held, pidErr, tailOfFile(serverLogPath(root), 40), tailOfFile(filepath.Join(root, "supervisor", "aimebu.log"), 20))
	}
	if restartNow {
		var out, errOut bytes.Buffer
		if code := run([]string{"supervisor", "restart"}, &out, &errOut); code != 0 {
			t.Fatalf("restart now: %d %q %q", code, out.String(), errOut.String())
		}
	} else {
		if code, out, errOut := runServerAction("stop"); code != 0 {
			t.Fatalf("stop: %d %q %q", code, out, errOut)
		}
		if pid, held, err := supervisorPID(root); err != nil || !held || pid != supervisorPIDAtStart {
			t.Fatalf("supervisor died on server stop: pid=%d held=%t err=%v", pid, held, err)
		}
		if err := probe.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, agent.ID).Scan(&running); err != nil || running != 1 {
			t.Fatalf("active turn changed at server stop: running=%d err=%v", running, err)
		}
		if changeEnvironment {
			t.Setenv("AIMEBU_TURN_MAX", "2h")
		}
		if code, out, errOut := runServerAction("start"); code != 0 {
			t.Fatalf("restart: %d %q %q", code, out, errOut)
		}
	}
	if changeEnvironment {
		deadline = time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(tailOfFile(serverLogPath(root), 30), "local supervisor restart pending") {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !strings.Contains(tailOfFile(serverLogPath(root), 30), "local supervisor restart pending") {
			t.Fatalf("new server did not detect environment change: %s", tailOfFile(serverLogPath(root), 30))
		}
		if pid, held, err := supervisorPID(root); err != nil || !held || pid != supervisorPIDAtStart {
			t.Fatalf("supervisor retired while a turn was active: pid=%d held=%t err=%v", pid, held, err)
		}
	}
	if restartNow {
		deadline = time.Now().Add(30 * time.Second)
		replaced, continued := false, false
		for time.Now().Before(deadline) {
			if pid, held, err := supervisorPID(root); err == nil && held && pid != supervisorPIDAtStart {
				replaced = true
			}
			var count int
			if err := probe.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=?`, agent.ID).Scan(&count); err == nil && count == 2 {
				continued = true
			}
			if replaced && continued {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !replaced || !continued {
			t.Fatalf("immediate restart did not continue once: replaced=%t continued=%t; server: %s; supervisor: %s", replaced, continued, tailOfFile(serverLogPath(root), 25), tailOfFile(filepath.Join(root, "supervisor", "aimebu.log"), 25))
		}
	}
	if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		var err error
		if restartNow {
			err = probe.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='agent' AND author_id=?`, room.ID, agent.ID).Scan(&count)
		} else {
			err = probe.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='agent' AND author_id=? AND reply_to=?`, room.ID, agent.ID, message.ID).Scan(&count)
		}
		if err == nil && count == 1 {
			var turns int
			wantTurns := 1
			if restartNow {
				wantTurns = 2
			}
			if err := probe.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=?`, agent.ID).Scan(&turns); err != nil || turns != wantTurns {
				t.Fatalf("turn was restarted: count=%d err=%v", turns, err)
			}
			if changeEnvironment || restartNow {
				replaced := false
				deadline = time.Now().Add(30 * time.Second)
				for time.Now().Before(deadline) {
					if pid, held, err := supervisorPID(root); err == nil && held && pid != supervisorPIDAtStart {
						replaced = true
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
				if !replaced {
					t.Fatalf("supervisor was not replaced after the turn: %s", tailOfFile(filepath.Join(root, "supervisor", "aimebu.log"), 25))
				}
			} else if pid, held, err := supervisorPID(root); err != nil || !held || pid != supervisorPIDAtStart {
				t.Fatalf("supervisor was replaced without environment change: pid=%d held=%t err=%v", pid, held, err)
			}
			if code, out, errOut := runServerAction("stop"); code != 0 {
				t.Fatalf("second stop: %d %q %q", code, out, errOut)
			}
			var out, errOut bytes.Buffer
			if code := run([]string{"server", "stop", "--with-supervisor"}, &out, &errOut); code != 0 {
				t.Fatalf("stop supervisor after server: %d %q %q", code, out.String(), errOut.String())
			}
			if held, _, err := supervisor.JournalOwner(supervisorJournalPath(root)); err != nil || held {
				t.Fatalf("supervisor lock after explicit stop: held=%t err=%v", held, err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("answer was not committed after restart; server log: %s; supervisor log: %s", strings.TrimSpace(tailOfFile(serverLogPath(root), 25)), strings.TrimSpace(tailOfFile(filepath.Join(root, "supervisor", "aimebu.log"), 25))))
}

func TestJournalOwnerProbesFreeAndHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	if held, _, err := supervisor.JournalOwner(path); err != nil || held {
		t.Fatalf("free lock: held=%t err=%v", held, err)
	}
	j, err := supervisor.OpenJournal(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if held, pid, err := supervisor.JournalOwner(path); err != nil || !held || pid != fmt.Sprint(os.Getpid()) {
		t.Fatalf("held lock: held=%t pid=%q err=%v", held, pid, err)
	}
}
