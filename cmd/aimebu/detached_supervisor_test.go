package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/server"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
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

// treeParentEnv makes this test binary a stand-in for a hanging supervisor: it starts a child in a process group of its own, as a
// harness or a command is started, prints the child's pid and then ignores every signal it can.
const treeParentEnv = "AIMEBU_TEST_TREE_PARENT"

func init() {
	if os.Getenv(treeParentEnv) == "" {
		return
	}
	// The child has a child of its own in its group, as a harness that runs a tool does.
	child := exec.Command("sh", "-c", "sleep 300 & echo $!; wait")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Stdout = os.Stdout
	if err := child.Start(); err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Println(child.Process.Pid)
	signal.Ignore(syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGINT, syscall.SIGHUP)
	time.Sleep(5 * time.Minute)
	os.Exit(0)
}

// gone says a process no longer runs: it is not in the process table, or only as a zombie nobody has reaped yet.
func gone(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	state := strings.TrimSpace(string(out))
	return err != nil || state == "" || strings.HasPrefix(state, "Z")
}

// The real kill: a supervisor that ignores its stop and still holds the journal is killed together with the process group of the
// harness it started, and with what that harness started. Left alone, that group would keep running, and writing in the checkout, next
// to the turn's continuation on the replacement.
func TestStuckSupervisorIsKilledWithItsChildProcessGroups(t *testing.T) {
	parent := exec.Command(os.Args[0], "-test.run=^$")
	parent.Env = append(os.Environ(), treeParentEnv+"=1")
	parent.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // its own session, as the detached supervisor has
	stdout, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var pids []int
	t.Cleanup(func() {
		_ = parent.Process.Kill()
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		<-exited
	})
	scanner := bufio.NewScanner(stdout)
	for len(pids) < 2 && scanner.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil {
			t.Fatalf("the stand-in supervisor printed %q", scanner.Text())
		}
		pids = append(pids, pid)
	}
	go func() { _ = parent.Wait(); close(exited) }()
	if len(pids) != 2 {
		t.Fatalf("the stand-in supervisor reported %v", pids)
	}
	harness, tool := pids[0], pids[1]
	if harness > tool { // the two lines come from two processes, in either order: the group leader was started first
		harness, tool = tool, harness
	}
	if gone(harness) || gone(tool) {
		t.Fatalf("the child processes do not run: %d gone=%t, %d gone=%t", harness, gone(harness), tool, gone(tool))
	}
	if err := syscall.Kill(parent.Process.Pid, syscall.SIGTERM); err != nil { // the stop it ignores
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	m := &detachedSupervisorManager{ctx: t.Context(), stderr: &stderr,
		handover: server.SupervisorHandover{ExitWait: 200 * time.Millisecond, TermWait: 200 * time.Millisecond, Poll: 20 * time.Millisecond},
		holder:   func() (int, bool, error) { return parent.Process.Pid, true, nil }}
	m.killIfStuck(parent.Process.Pid, supervisor.ProcessStart(parent.Process.Pid))
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the supervisor that ignored its stop was not killed: %s", stderr.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(gone(harness) && gone(tool)) {
		time.Sleep(20 * time.Millisecond)
	}
	if !gone(harness) || !gone(tool) {
		t.Fatalf("the killed supervisor's child process group still runs: %d gone=%t, %d gone=%t; %s", harness, gone(harness), tool, gone(tool), stderr.String())
	}
}

// Only what the supervisor started is killed: the groups its descendants lead, and descendants in its own group one by one. A group
// led by a stranger is never signalled, also when a descendant sits in it.
func TestDescendantsOfAProcess(t *testing.T) {
	row := func(pid, ppid, pgid int) processRow { return processRow{pid: pid, ppid: ppid, pgid: pgid} }
	rows := []processRow{row(1, 0, 1), row(100, 1, 100), // the supervisor, in its own session
		row(200, 100, 200), row(201, 200, 200), row(202, 201, 200), // a harness in its group, with a tool and the tool's child
		row(300, 100, 300),                                     // a command in its group
		row(400, 100, 100),                                     // a helper in the supervisor's own group
		row(500, 400, 900),                                     // a descendant in a group a stranger leads
		row(900, 1, 900), row(901, 900, 900), row(600, 1, 600), // strangers
		row(310, 100, 310), row(311, 310, 311)} // a detachable job and what it started in a group of its own
	groups, singles := descendantsOf(100, rows, func(r processRow) bool { return r.pid == 310 })
	sort.Ints(groups)
	sort.Ints(singles)
	if fmt.Sprint(groups) != "[200 300]" || fmt.Sprint(singles) != "[400 500]" {
		t.Fatalf("groups %v, singles %v", groups, singles)
	}
}

// treeJobEnv makes the stand-in supervisor also start a detachable job's leader (its own session, the wrapper's argument list with this
// log) and a plain command (its own group), and print both pids.
const treeJobEnv = "AIMEBU_TEST_TREE_JOB_LOG"

func init() {
	log := os.Getenv(treeJobEnv)
	if log == "" || os.Getenv(treeParentEnv) != "" {
		return
	}
	job := exec.Command("sh", "-c", `sleep 300; : "$4" command-log "$2"`, "sh", log+".exit", log, "true", "/bin/logger")
	job.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	plain := exec.Command("sleep", "300")
	plain.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if job.Start() != nil || plain.Start() != nil {
		os.Exit(1)
	}
	fmt.Println(job.Process.Pid, plain.Process.Pid)
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	time.Sleep(5 * time.Minute)
	os.Exit(0)
}

// A hung supervisor's kill spares the detachable job it verifies (its group leader runs the wrapper with the job's log, in a session of
// its own, started no earlier than the job) and kills what it does not: a job whose start time does not fit, one the server does not list,
// and a plain command.
func TestHungSupervisorKillSparesVerifiedDetachableJobs(t *testing.T) {
	log := filepath.Join(t.TempDir(), "job.log")
	for name, item := range map[string]struct {
		jobs  func() []detachableJob
		spare bool
	}{
		"verified":          {func() []detachableJob { return []detachableJob{{log, time.Now().Add(-time.Minute)}} }, true},
		"started too early": {func() []detachableJob { return []detachableJob{{log, time.Now().Add(time.Hour)}} }, false},
		"not listed":        {func() []detachableJob { return nil }, false},
		"another log":       {func() []detachableJob { return []detachableJob{{log + "x", time.Now().Add(-time.Minute)}} }, false},
	} {
		t.Run(name, func(t *testing.T) {
			parent := exec.Command(os.Args[0], "-test.run=^$")
			parent.Env = append(os.Environ(), treeJobEnv+"="+log)
			parent.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			stdout, err := parent.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			var job, plain int
			t.Cleanup(func() {
				_ = parent.Process.Kill()
				_, _ = parent.Process.Wait()
				if job > 0 {
					_ = syscall.Kill(-job, syscall.SIGKILL)
				}
				if plain > 0 {
					_ = syscall.Kill(-plain, syscall.SIGKILL)
				}
			})
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := fmt.Sscan(line, &job, &plain); n != 2 {
				t.Fatalf("the stand-in supervisor printed %q", line)
			}
			// ps reads the job's argument list only once the shell has exec'd: wait until the table shows it.
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				rows, _, _ := processTable()
				found := false
				for _, row := range rows {
					found = found || (row.pid == job && strings.Contains(row.args, "command-log"))
				}
				if found {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := killSupervisorTree(parent.Process.Pid, item.jobs()); err != nil {
				t.Fatal(err)
			}
			time.Sleep(300 * time.Millisecond)
			if !gone(plain) {
				t.Fatal("the plain command was not killed")
			}
			if gone(job) == item.spare {
				t.Fatalf("job gone=%t, want spared=%t", gone(job), item.spare)
			}
		})
	}
}

func init() {
	supervisorClientHook = func(client *supervisor.Client) {
		// The supervisor child stands in for a build from before turn_interrupt.
		client.NoTurnInterrupt = os.Getenv("AIMEBU_TEST_OLD_SUPERVISOR") != ""
	}
}

// A supervisor that was told to stop and still holds the journal when the handover's waits are over is sent SIGTERM and then killed; one
// that let go of it, was replaced by another process, or whose pid now belongs to another process (its start time changed) is left alone.
func TestStuckSupervisorIsKilledAfterTheStopBound(t *testing.T) {
	for name, item := range map[string]struct {
		holderPID int
		held      bool
		start     string
		killed    bool
	}{
		"still holds the journal": {4242, true, "start-a", true},
		"released the journal":    {0, false, "start-a", false},
		"another one took over":   {4343, true, "start-a", false},
		"its pid was reused":      {4242, true, "start-b", false},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			killed := make(chan int, 1)
			var signalled []syscall.Signal
			var mu sync.Mutex
			m := &detachedSupervisorManager{ctx: t.Context(), stderr: &stderr,
				handover: server.SupervisorHandover{ExitWait: 150 * time.Millisecond, TermWait: 150 * time.Millisecond, Poll: 10 * time.Millisecond,
					Signal: func(pid int, sig syscall.Signal) error {
						mu.Lock()
						signalled = append(signalled, sig)
						mu.Unlock()
						return nil
					}},
				holder:  func() (int, bool, error) { return item.holderPID, item.held, nil },
				startOf: func(int) string { return item.start },
				kill:    func(pid int) error { killed <- pid; return nil }}
			m.killIfStuck(4242, "start-a")
			m.killIfStuck(4242, "start-a") // a second stop of the same process starts no second watch
			select {
			case pid := <-killed:
				if !item.killed || pid != 4242 {
					t.Fatalf("killed pid %d", pid)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(signalled) != 1 || signalled[0] != syscall.SIGTERM {
					t.Fatalf("signals before the kill: %v", signalled)
				}
			case <-time.After(1500 * time.Millisecond):
				if item.killed {
					t.Fatal("the supervisor that kept the journal was not killed")
				}
				mu.Lock()
				defer mu.Unlock()
				if len(signalled) != 0 {
					t.Fatalf("a process that is no longer the supervisor was signalled: %v", signalled)
				}
			}
		})
	}
}

// A supervisor that holds the journal without a connection is handed to the server's overdue check, but only after it has been
// disconnected for a while: one between two reconnects is not asked about.
func TestDisconnectedSupervisorIsCheckedForAnOverdueReplacement(t *testing.T) {
	asked := 0
	m := &detachedSupervisorManager{replaceOverdue: func(context.Context) (bool, error) { asked++; return true, nil }}
	if replaced, err := m.overdue(t.Context(), time.Now()); replaced || err != nil || asked != 0 {
		t.Fatalf("a supervisor that just disconnected: replaced=%t err=%v asked=%d", replaced, err, asked)
	}
	if replaced, err := m.overdue(t.Context(), time.Now().Add(-supervisorDisconnectedLimit-time.Second)); !replaced || err != nil || asked != 1 {
		t.Fatalf("a supervisor disconnected for long: replaced=%t err=%v asked=%d", replaced, err, asked)
	}
	if replaced, err := (&detachedSupervisorManager{}).overdue(t.Context(), time.Now().Add(-time.Hour)); replaced || err != nil {
		t.Fatalf("no server check: replaced=%t err=%v", replaced, err)
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
	testDetachedSupervisorAcrossServerRestart(t, false, false, false)
}

func TestDetachedSupervisorDrainsForEnvironmentChange(t *testing.T) {
	testDetachedSupervisorAcrossServerRestart(t, true, false, false)
}

func TestDetachedSupervisorRestartNowContinuesTurn(t *testing.T) {
	testDetachedSupervisorAcrossServerRestart(t, false, true, false)
}

func testDetachedSupervisorAcrossServerRestart(t *testing.T, changeEnvironment, restartNow, olderBuild bool) {
	root, _ := daemonFixture(t)
	// These tests do not look at the web page, and each server start would otherwise bundle the web UI with esbuild before it listens.
	// On a 2-core CI runner, with the supervisor and leftovers of earlier tests competing for the CPU, that bundling alone pushed a start
	// past the default 30 s (alpha.6 CI). Serve the modules instead, and allow 90 s anyway: the limit is a ceiling that a healthy start
	// never reaches, so a longer one costs nothing when the server comes up.
	t.Setenv("AIMEBU_WEB_BUNDLE", "off")
	previousStartLimit := daemonStartLimit
	daemonStartLimit = 90 * time.Second
	t.Cleanup(func() { daemonStartLimit = previousStartLimit })
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
	if olderBuild {
		// Two seconds of grace: the old supervisor's turn is still held by the gate when it is over.
		if err := db.SetGlobalSettingValue(ctx, "supervisor_drain_grace_seconds", "2"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AIMEBU_TEST_OLD_SUPERVISOR", "1") // the first supervisor child inherits it; its replacement does not
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
		if olderBuild {
			t.Setenv("AIMEBU_TEST_OLD_SUPERVISOR", "")
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
		if olderBuild {
			// Its grace may already be over.
		} else if pid, held, err := supervisorPID(root); err != nil || !held || pid != supervisorPIDAtStart {
			t.Fatalf("supervisor retired while a turn was active: pid=%d held=%t err=%v", pid, held, err)
		}
	}
	// A restart now, and an older build whose grace ran out, both cut the turn: it is continued once on the replacement.
	restartNow = restartNow || olderBuild
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
		if olderBuild && !strings.Contains(tailOfFile(serverLogPath(root), 40), "the supervisor cannot interrupt a turn; restarting it") {
			t.Fatalf("the old supervisor was not replaced by the drain: %s", tailOfFile(serverLogPath(root), 40))
		}
	}
	if olderBuild {
		assertSwapLogSequence(t, serverLogPath(root))
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

// assertSwapLogSequence waits for the server log to tell how one supervisor swap went and checks the order of its steps: the mismatch,
// the end of the grace, the retire request, the new supervisor's start and its connection. The old one's exit has no fixed place next to
// the start (the manager's loop and the exit watch run apart), so it only has to come after the retire request and before the connection.
func assertSwapLogSequence(t *testing.T, logPath string) {
	t.Helper()
	steps := []string{
		"local supervisor restart pending",
		"supervisor swap: drain grace of 2s is over",
		"supervisor swap: retire requested: pid",
		"supervisor swap: started supervisor pid",
		"supervisor swap: supervisor pid",
	}
	var content string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		content = string(data)
		if retired := strings.Index(content, "supervisor swap: retire requested: pid"); retired >= 0 && strings.LastIndex(content, "supervisor swap: supervisor pid") > retired && strings.Contains(content, "supervisor swap: old supervisor pid") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	position := 0
	for _, step := range steps {
		index := strings.Index(content[position:], step)
		if index < 0 {
			t.Fatalf("swap log lacks %q after offset %d, or has it out of order; server log:\n%s", step, position, content)
		}
		position += index + len(step)
	}
	retired := strings.Index(content, "supervisor swap: retire requested: pid")
	exited := strings.Index(content, "supervisor swap: old supervisor pid")
	connected := strings.LastIndex(content, "supervisor swap: supervisor pid")
	if exited < retired || exited > connected {
		t.Fatalf("the old supervisor's exit is not between the retire request and the new connection; server log:\n%s", content)
	}
	if !strings.Contains(content, "supervisor swap: local supervisor connected: build ") {
		t.Fatalf("swap log lacks the connected build; server log:\n%s", content)
	}
}
