package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
