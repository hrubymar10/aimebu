package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/server"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

type spawnCall struct {
	journal string
	drain   bool
}

// instanceRig is a manager whose view of the machine is whatever the test says, so each rule of the pass is read on its own.
type instanceRig struct {
	m      *detachedSupervisorManager
	dir    string
	now    time.Time
	views  []instanceView
	conns  map[string]bool
	states map[string]string
	busy   map[string]bool
	active bool
	mu     sync.Mutex
	spawns []spawnCall
	stderr bytes.Buffer
}

func newInstanceRig(t *testing.T) *instanceRig {
	t.Helper()
	root := t.TempDir()
	r := &instanceRig{dir: filepath.Join(root, "supervisor"), now: time.Unix(1_800_000_000, 0), conns: map[string]bool{}, states: map[string]string{}, busy: map[string]bool{}}
	if err := os.MkdirAll(r.dir, 0700); err != nil {
		t.Fatal(err)
	}
	r.m = &detachedSupervisorManager{
		config: serverConfig{root: root}, stderr: &r.stderr, ctx: t.Context(),
		connected:   func() bool { return r.active },
		connections: func() map[string]bool { return r.conns },
		clock:       func() time.Time { return r.now },
		scan:        func() []instanceView { return r.views },
		states:      func() map[string]string { return r.states },
		busy:        func(id string) (bool, error) { return r.busy[id], nil },
		spawn: func(journal string, drain bool) error {
			r.mu.Lock()
			r.spawns = append(r.spawns, spawnCall{journal, drain})
			r.mu.Unlock()
			return nil
		},
		startOf: func(int) string { return "start" },
	}
	// A handover the manager started (stopInstanceSignal) polls the journal's lock file in the rig's directory, which creates it, and may still be
	// at it when the test returns: wait for it before t.TempDir is removed. The test's context is cancelled by then, so each one returns at
	// its next wait.
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { r.m.stopWG.Wait(); close(done) }()
		receive(t, "the handover goroutines of the manager", done, time.Minute)
	})
	return r
}

func (r *instanceRig) pass(t *testing.T, advance time.Duration) {
	t.Helper()
	r.now = r.now.Add(advance)
	r.m.pass(t.Context())
}

func (r *instanceRig) legacy() string { return filepath.Join(r.dir, "journal.sqlite") }

func TestMismatchStartsANewInstanceNextToTheOldOne(t *testing.T) {
	r := newInstanceRig(t)
	// The old build holds the legacy journal, is connected, and the server has it retiring: nothing is active.
	r.views = []instanceView{{path: r.legacy(), held: true, pid: 4242, journalID: "old"}}
	r.conns["old"] = true
	r.states["old"] = "retiring"
	r.pass(t, 0)
	if len(r.spawns) != 1 || r.spawns[0].drain || filepath.Dir(r.spawns[0].journal) != filepath.Join(r.dir, "journals") || !strings.HasSuffix(r.spawns[0].journal, ".sqlite") {
		t.Fatalf("no new instance next to the old one: %+v", r.spawns)
	}
	// The process has not connected yet: a second pass inside the guard starts nothing, and neither does one after it while it is held and young.
	r.views = append(r.views, instanceView{path: r.spawns[0].journal, held: true, pid: 4243, journalID: "new"})
	r.pass(t, 3*time.Second)
	r.pass(t, 20*time.Second)
	if len(r.spawns) != 1 {
		t.Fatalf("a second instance was started while the first was starting: %+v", r.spawns)
	}
	// It connects and is the active one: still nothing more, and the old instance is left alone.
	r.conns["new"], r.active = true, true
	r.pass(t, 5*time.Second)
	if len(r.spawns) != 1 {
		t.Fatalf("an instance was started with one active: %+v", r.spawns)
	}
}

func TestFirstInstanceStartsOnTheLegacyJournalAndWaitsForAnUnconnectedHolder(t *testing.T) {
	r := newInstanceRig(t)
	r.pass(t, 0)
	if len(r.spawns) != 1 || r.spawns[0] != (spawnCall{}) {
		t.Fatalf("first start: %+v", r.spawns)
	}
	// A holder that has not connected yet (after a server restart, or just started) is given its 30 seconds before another is started next to it.
	r2 := newInstanceRig(t)
	r2.views = []instanceView{{path: r2.legacy(), held: true, pid: 99, journalID: "a"}}
	r2.pass(t, 0)
	r2.pass(t, 29*time.Second)
	if len(r2.spawns) != 0 {
		t.Fatalf("started next to a holder that is still connecting: %+v", r2.spawns)
	}
	r2.pass(t, 3*time.Second)
	if len(r2.spawns) != 1 || r2.spawns[0].journal == "" {
		t.Fatalf("no instance next to a holder that never connected: %+v", r2.spawns)
	}
}

func TestCrashedJournalWithOpenWorkGetsADrainInstance(t *testing.T) {
	r := newInstanceRig(t)
	ctx := context.Background()
	path := filepath.Join(r.dir, "journals", "crashed.sqlite")
	j, err := supervisor.OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordStart(ctx, supervisor.StartRecord{TurnID: "turn_x", AgentID: "agent_x", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil { // the holder died: its lock is free, the journal still lists the turn
		t.Fatal(err)
	}
	r.active, r.conns["live"] = true, true
	r.views = []instanceView{{path: path}}
	r.pass(t, 0)
	if len(r.spawns) != 1 || r.spawns[0] != (spawnCall{path, true}) {
		t.Fatalf("the journal with open work was not drained: %+v", r.spawns)
	}
	// With no active instance the same journal takes a regular start: it is the one that takes the machine's work again.
	r2 := newInstanceRig(t)
	r2.views = []instanceView{{path: path}}
	r2.pass(t, 0)
	if len(r2.spawns) != 1 || r2.spawns[0] != (spawnCall{path, false}) {
		t.Fatalf("no regular start on the journal when nothing is active: %+v", r2.spawns)
	}
}

func TestEmptyJournalOfAGoneInstanceIsRemovedButTheLegacyOneStays(t *testing.T) {
	r := newInstanceRig(t)
	ctx := context.Background()
	side := filepath.Join(r.dir, "journals", "empty.sqlite")
	for _, path := range []string{side, r.legacy()} {
		j, err := supervisor.OpenJournal(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		_ = j.Close()
	}
	r.active, r.conns["live"] = true, true
	r.views = []instanceView{{path: r.legacy()}, {path: side}}
	r.pass(t, 0)
	if _, err := os.Stat(side); !os.IsNotExist(err) {
		t.Fatalf("an empty journal of a gone instance was left: %v", err)
	}
	if _, err := os.Stat(r.legacy()); err != nil {
		t.Fatalf("the legacy journal was removed: %v", err)
	}
	if len(r.spawns) != 0 {
		t.Fatalf("started something for empty journals: %+v", r.spawns)
	}
}

// sleeper is a stand-in for a supervisor process: it dies on the SIGUSR1 of the exit request, as a process that does not catch it does.
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd.Process.Pid
}

func processGone(pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if gone(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return gone(pid)
}

// Liveness is the only bound on a retiring instance: one that holds its lock without a server connection for ten minutes is cut, but not before.
func TestRetiringInstanceWithoutAConnectionForTenMinutesIsHandedOver(t *testing.T) {
	r := newInstanceRig(t)
	pid := sleeper(t)
	r.active, r.conns["new"] = true, true
	r.views = []instanceView{{path: r.legacy(), held: true, pid: pid, journalID: "old"}}
	r.states["old"] = "retiring"
	r.pass(t, 0)
	r.pass(t, 9*time.Minute)
	if gone(pid) {
		t.Fatal("cut before ten minutes")
	}
	r.pass(t, 2*time.Minute)
	if !processGone(pid, 5*time.Second) {
		t.Fatalf("an instance without a connection for ten minutes was not cut; log: %s", r.stderr.String())
	}
}

// An instance that is connected and busy is never cut, however long it takes; one the server sees as empty for a minute that does not leave is.
func TestRetiringInstanceIsCutOnlyWhenTheServerSeesItEmpty(t *testing.T) {
	r := newInstanceRig(t)
	pid := sleeper(t)
	r.active, r.conns["new"], r.conns["old"] = true, true, true
	r.views = []instanceView{{path: r.legacy(), held: true, pid: pid, journalID: "old"}}
	r.states["old"] = "retiring"
	r.busy["old"] = true
	for i := 0; i < 30; i++ { // half an hour of a long job
		r.pass(t, time.Minute)
	}
	if gone(pid) {
		t.Fatal("an instance that still runs work was cut")
	}
	r.busy["old"] = false
	r.pass(t, 0)
	r.pass(t, 50*time.Second)
	if gone(pid) {
		t.Fatal("cut before it had been empty for a minute")
	}
	r.pass(t, 20*time.Second)
	if !processGone(pid, 5*time.Second) {
		t.Fatalf("an empty instance that did not exit was not cut; log: %s", r.stderr.String())
	}
}

// The active instance is not retiring, so neither rule touches it, connected or not.
func TestActiveInstanceIsNeverHandedOver(t *testing.T) {
	r := newInstanceRig(t)
	pid := sleeper(t)
	r.views = []instanceView{{path: r.legacy(), held: true, pid: pid, journalID: "a"}}
	r.states["a"] = "active"
	r.pass(t, 0)
	r.pass(t, time.Hour)
	if gone(pid) {
		t.Fatal("the active instance was cut")
	}
}

// A journal whose lock holder writes its ID after its pid: the manager reads both without opening the database.
func TestJournalLockInfoCarriesTheHolderPidAndJournalID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journals", "x.sqlite")
	j, err := supervisor.OpenJournal(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	held, pid, id, err := supervisor.JournalLockInfo(path)
	if err != nil || !held || pid != strconv.Itoa(os.Getpid()) || id != j.ID {
		t.Fatalf("held=%t pid=%q id=%q err=%v, want pid %d id %s", held, pid, id, err, os.Getpid(), j.ID)
	}
	if held, owner, _ := supervisor.JournalOwner(path); !held || owner != strconv.Itoa(os.Getpid()) {
		t.Fatalf("JournalOwner pid %q", owner)
	}
}

// A supervisor started while another instance holds the default journal takes a journal of its own: that is a remote host's whole upgrade.
func TestSupervisorTakesAFreshJournalWhenTheDefaultOneIsHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "supervisor")
	defaultPath := filepath.Join(dir, "journal.sqlite")
	var stderr bytes.Buffer
	if got, err := chooseJournalPath(supervisorInstanceOptions{}, defaultPath, time.Now(), &stderr); err != nil || got != defaultPath {
		t.Fatalf("free default journal: %q %v", got, err)
	}
	held, err := supervisor.OpenJournal(t.Context(), defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	got, err := chooseJournalPath(supervisorInstanceOptions{}, defaultPath, time.Now(), &stderr)
	if err != nil || filepath.Dir(got) != filepath.Join(dir, "journals") || !strings.HasSuffix(got, ".sqlite") {
		t.Fatalf("held default journal: %q %v", got, err)
	}
	if again, _ := chooseJournalPath(supervisorInstanceOptions{}, defaultPath, time.Now(), &stderr); again == got {
		t.Fatalf("two starts picked the same journal %q", got)
	}
	if explicit, _ := chooseJournalPath(supervisorInstanceOptions{Journal: "/x/y.sqlite"}, defaultPath, time.Now(), &stderr); explicit != "/x/y.sqlite" {
		t.Fatalf("--journal was overridden: %q", explicit)
	}
	if !strings.Contains(stderr.String(), "starting next to it") {
		t.Fatalf("nothing logged: %q", stderr.String())
	}
}

// newLogCapture collects what the package logs during a test, safe to read while a handover goroutine still writes.
func newLogCapture(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	previous := log.Writer()
	log.SetOutput(c)
	t.Cleanup(func() { log.SetOutput(previous) })
	return c
}

type logCapture struct {
	mu    sync.Mutex
	lines bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lines.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lines.String()
}

// An older build writes only its pid into the lock file, so its holder has no journal ID. While it is connected, its is the one connection
// whose journal no lock file names: it is that instance, and the same rules apply to it — busy is never cut, empty for a minute is.
func TestIDLessRetiringHolderIsKnownByItsConnectionAndFollowsTheSameRules(t *testing.T) {
	logged := newLogCapture(t)
	r := newInstanceRig(t)
	pid := sleeper(t)
	// The connection named old is the ID-less holder's own: its hello carries its journal ID even though its lock file does not.
	r.conns["old"] = true
	r.views = []instanceView{{path: r.legacy(), held: true, pid: pid}}
	r.states["old"] = "retiring"
	r.busy["old"] = true
	r.pass(t, 0) // the manager starts the current build next to the holder, as on a real upgrade
	if len(r.spawns) != 1 || r.spawns[0].drain {
		t.Fatalf("no instance was started next to the ID-less holder: %+v", r.spawns)
	}
	r.views = append(r.views, instanceView{path: r.spawns[0].journal, held: true, pid: 4244, journalID: "new"})
	r.conns["new"] = true
	for i := 0; i < 30; i++ { // half an hour of a long job
		r.pass(t, time.Minute)
	}
	if gone(pid) {
		t.Fatal("an ID-less holder that still runs work was cut")
	}
	if n := strings.Count(logged.String(), "leaving it alone"); n != 0 {
		t.Fatalf("the holder was left unidentified although its connection named it (%d times): %s", n, logged.String())
	}
	r.busy["old"] = false
	r.pass(t, 0)
	r.pass(t, 50*time.Second)
	if gone(pid) {
		t.Fatal("cut before it had been empty for a minute")
	}
	r.pass(t, 20*time.Second)
	if !processGone(pid, 5*time.Second) {
		t.Fatalf("an empty ID-less holder that did not exit was not cut; log: %s", logged.String())
	}
}

// The disconnect bound applies to an identified ID-less holder too: its own connection is the one that counts, not just any connection. The
// current build's instance is connected and its lock names its ID, so nothing accounts for the holder: the server's retiring record is what
// identifies it.
func TestIDLessRetiringHolderWithoutItsOwnConnectionForTenMinutesIsHandedOver(t *testing.T) {
	logged := newLogCapture(t)
	r := newInstanceRig(t)
	kill(t, r)
	db := rigStore(t)
	r.m.store, r.m.machineID = db, "mch_test"
	recordInstance(t, db, "old", "an older build", "retiring")
	pid := sleeper(t)
	r.states["old"] = "retiring"
	r.active, r.conns["new"] = true, true
	r.views = []instanceView{
		{path: r.legacy(), held: true, pid: pid},
		{path: filepath.Join(r.dir, "journals", "new.sqlite"), held: true, pid: 4244, journalID: "new"},
	}
	r.pass(t, 0)
	r.pass(t, 9*time.Minute)
	if gone(pid) {
		t.Fatal("cut before ten minutes")
	}
	r.pass(t, 2*time.Minute)
	if !processGone(pid, 5*time.Second) {
		t.Fatalf("an ID-less holder without its own connection for ten minutes was not cut; log: %s", logged.String())
	}
}

// rigStore opens a store under a fresh temporary server directory, the way the manager reads the server's instance records in tests.
// kill makes the handover of a unit-test rig end at once: a `sleep` child dies on the signal the stop sends, but the rig has no journal for
// the handover to watch, so its real waits would end in a SIGKILL of a process that already went, logged through the default logger.
func kill(t *testing.T, r *instanceRig) {
	t.Helper()
	r.m.kill = func(int) error { return nil }
	// The handover polls who holds the journal; the rig has none, so it must not be asked (its probe would create a lock file the
	// temporary directory's cleanup races with).
	r.m.holder = func() (int, bool, error) { return 0, false, nil }
	r.m.handover = server.SupervisorHandover{ExitWait: 50 * time.Millisecond, TermWait: 50 * time.Millisecond, Poll: 10 * time.Millisecond}
}

func rigStore(t *testing.T) *store.Store {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(root, "server", "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO machine(id,name,key_hash,approved) VALUES ('mch_test','rig','hash',1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func recordInstance(t *testing.T, db *store.Store, id, build, state string) {
	t.Helper()
	if err := db.RecordSupervisorInstance(context.Background(), "mch_test", id, build, "env"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSupervisorInstanceState(context.Background(), "mch_test", id, state); err != nil {
		t.Fatal(err)
	}
}

// A holder that is not connected is matched to the server's records: the one retiring instance of an older build that no lock file claims
// and no connection accounts for. A record of the current build is never a match: such a holder writes its ID, so the record belongs to an
// instance whose journal is gone, and cutting the holder on it could cut the wrong one.
func TestIDLessDisconnectedHolderIsKnownFromTheServersRetiringRecord(t *testing.T) {
	logged := newLogCapture(t)
	r := newInstanceRig(t)
	kill(t, r)
	db := rigStore(t)
	r.m.store, r.m.machineID = db, "mch_test"
	recordInstance(t, db, "old", "an older build", "retiring")
	pid := sleeper(t)
	r.states["old"] = "retiring"
	r.views = []instanceView{{path: r.legacy(), held: true, pid: pid}}
	r.pass(t, 0)
	r.pass(t, 9*time.Minute)
	if gone(pid) {
		t.Fatal("cut before ten minutes")
	}
	r.pass(t, 2*time.Minute)
	if !processGone(pid, 5*time.Second) {
		t.Fatalf("a disconnected ID-less holder was not matched to its retiring record; log: %s", logged.String())
	}
	r2 := newInstanceRig(t)
	db2 := rigStore(t)
	r2.m.store, r2.m.machineID = db2, "mch_test"
	recordInstance(t, db2, "phantom", version, "retiring")
	pid2 := sleeper(t)
	r2.views = []instanceView{{path: r2.legacy(), held: true, pid: pid2}}
	for i := 0; i < 40; i++ { // over an hour of passes
		r2.pass(t, 2*time.Minute)
	}
	if gone(pid2) {
		t.Fatal("a holder was cut on a record of the current build")
	}
	if n := strings.Count(logged.String(), "leaving it alone"); n != 1 {
		t.Fatalf("the refusal was logged %d times, want once: %s", n, logged.String())
	}
}

// When the mapping is ambiguous the holder is left alone and told so once: two connections no lock names, two such records, two holders
// without an ID. A guess could cut the wrong instance, and no rule here may.
func TestAmbiguousIDLessHolderIsLeftAloneAndToldSoOnce(t *testing.T) {
	for name, item := range map[string]struct {
		arrange  func(t *testing.T, r *instanceRig)
		wantLogs int
	}{
		"two connections no lock names": {
			arrange: func(t *testing.T, r *instanceRig) {
				r.conns["old1"], r.conns["old2"] = true, true
			},
			wantLogs: 1,
		},
		"two retiring records of older builds": {
			arrange: func(t *testing.T, r *instanceRig) {
				db := rigStore(t)
				r.m.store, r.m.machineID = db, "mch_test"
				recordInstance(t, db, "old1", "an older build", "retiring")
				recordInstance(t, db, "old2", "an older build", "retiring")
			},
			wantLogs: 1,
		},
		"two holders without an ID": {
			arrange: func(t *testing.T, r *instanceRig) {
				r.views = append(r.views, instanceView{path: filepath.Join(r.dir, "journals", "other.sqlite"), held: true, pid: 4243})
			},
			wantLogs: 2, // one line per unidentified holder
		},
	} {
		t.Run(name, func(t *testing.T) {
			logged := newLogCapture(t)
			r := newInstanceRig(t)
			kill(t, r)
			item.arrange(t, r)
			pid := sleeper(t)
			r.views = append([]instanceView{{path: r.legacy(), held: true, pid: pid}}, r.views...)
			r.active = true
			for i := 0; i < 40; i++ { // over an hour of passes
				r.pass(t, 2*time.Minute)
			}
			if processGone(pid, 2*time.Second) {
				t.Fatalf("an ambiguous holder was cut anyway; log: %s", logged.String())
			}
			if n := strings.Count(logged.String(), "leaving it alone"); n != item.wantLogs {
				t.Fatalf("the refusal was logged %d times, want %d: %s", n, item.wantLogs, logged.String())
			}
		})
	}
}

// writeStuckJournal leaves a journal of the confirmed A13-13 shape: one turn of an older generation, exited and never acknowledged by the
// server, and nothing else owed.
func writeStuckJournal(t *testing.T, path, turnID string) {
	t.Helper()
	ctx := context.Background()
	j, err := supervisor.OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordStart(ctx, supervisor.StartRecord{TurnID: turnID, AgentID: "agent_x", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordExit(ctx, turnID, -1, false, false); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
}

// A drain that ends and leaves the journal owing exactly what it did before is not progress: the manager backs off for an hour, logs once
// what is owed, records the one system notice, and tries one more drain when the back-off is over.
func TestDrainThatLeavesTheSameDebtBacksOff(t *testing.T) {
	logged := newLogCapture(t)
	r := newInstanceRig(t)
	var notices []string
	r.m.stuckNotice = func(journal, debt string) { notices = append(notices, journal+"/"+debt) }
	path := filepath.Join(r.dir, "journals", "stuck.sqlite")
	writeStuckJournal(t, path, "turn_stale")
	r.active = true
	r.views = []instanceView{{path: path}}
	r.pass(t, 0)
	if len(r.spawns) != 1 || !r.spawns[0].drain {
		t.Fatalf("the stuck journal was not drained: %+v", r.spawns)
	}
	if !strings.Contains(logged.String(), "(1 open turn, 1 pending exit); starting a drain instance") {
		t.Fatalf("the start line does not say what is owed: %s", logged.String())
	}
	// The drain ran and ended: the journal is free again and owes the same thing. No second drain inside the back-off.
	r.pass(t, 20*time.Second)
	if len(r.spawns) != 1 {
		t.Fatalf("another drain was started for the same debt: %+v", r.spawns)
	}
	if n := strings.Count(logged.String(), "still owes the same work a drain instance was left"); n != 1 {
		t.Fatalf("the back-off was logged %d times, want once: %s", n, logged.String())
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "stuck.sqlite/1 open turn, 1 pending exit") {
		t.Fatalf("the stuck-journal notice: %v", notices)
	}
	r.pass(t, 30*time.Minute)
	if len(r.spawns) != 1 || len(notices) != 1 {
		t.Fatalf("the back-off did not hold: spawns=%+v notices=%v", r.spawns, notices)
	}
	// The hour is over: one more drain is tried, and the rule starts over with it.
	r.pass(t, 31*time.Minute)
	if len(r.spawns) != 2 || !r.spawns[1].drain {
		t.Fatalf("no drain after the back-off: %+v", r.spawns)
	}
	r.pass(t, 20*time.Second)
	if len(r.spawns) != 2 {
		t.Fatalf("drain after the second back-off began: %+v", r.spawns)
	}
	if n := strings.Count(logged.String(), "still owes the same work a drain instance was left"); n != 2 {
		t.Fatalf("the second episode was not logged once: %s", logged.String())
	}
	if len(notices) != 2 {
		t.Fatalf("the second episode's notice: %v", notices)
	}
}

// A drain that made progress (the debt it leaves is not the one it was given) is answered with another drain, not with the back-off.
func TestDrainThatMadeProgressIsDrainedAgain(t *testing.T) {
	logged := newLogCapture(t)
	r := newInstanceRig(t)
	var notices []string
	r.m.stuckNotice = func(journal, debt string) { notices = append(notices, journal+"/"+debt) }
	ctx := context.Background()
	path := filepath.Join(r.dir, "journals", "moving.sqlite")
	writeStuckJournal(t, path, "turn_stale")
	r.active = true
	r.views = []instanceView{{path: path}}
	r.pass(t, 0)
	if len(r.spawns) != 1 {
		t.Fatalf("first drain: %+v", r.spawns)
	}
	// The drain's work shows: the turn it ended also got an answer the server has not receipted, so the debt changed.
	j, err := supervisor.OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordAnswer(ctx, "turn_stale", 1, "digest", json.RawMessage(`{"answer":true}`), false); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	r.pass(t, 20*time.Second)
	if len(r.spawns) != 2 || !r.spawns[1].drain {
		t.Fatalf("progress was not answered with another drain: %+v", r.spawns)
	}
	if strings.Contains(logged.String(), "backing off") || len(notices) != 0 {
		t.Fatalf("progress was treated as a stuck journal: %s %v", logged.String(), notices)
	}
}
