package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// An older build writes only its pid into its lock file and keeps running after retire_at_idle while its journal still owes work. This test
// runs such an instance next to the current build: a supervisor of an older build string (AIMEBU_TEST_VERSION) whose lock file is cut back to
// the one pid line the previous build wrote. The manager must start the current build next to it, leave it alone while the server sees its
// work, and give it the bounded handover once the server sees it empty and it still holds the journal — on SIGTERM, not on the SIGUSR1 an
// older build knows as its restart signal.
func TestIDLessOldBuildNextToTheCurrentBuildIsWaitedForThenHandedOver(t *testing.T) {
	// The real retire-empty limit is a minute; the point of this test is the rule, not the wait, so it is shortened.
	previousEmptyLimit := retireEmptyLimit
	retireEmptyLimit = 5 * time.Second
	t.Cleanup(func() { retireEmptyLimit = previousEmptyLimit })
	root, _ := daemonFixture(t)
	t.Setenv("AIMEBU_WEB_BUNDLE", "off")
	previousStartLimit := daemonStartLimit
	daemonStartLimit = 90 * time.Second
	t.Cleanup(func() { daemonStartLimit = previousStartLimit })
	t.Setenv("AIMEBU_ALLOW", "192.0.2.0/24")
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start without a supervisor: %d %q %q", code, out, errOut)
	}
	credential, err := readHostMachineKey(filepath.Join(root, "server", "host-machine.key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(root, "server", "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	project, err := db.CreateProject(ctx, store.Project{Name: "legacy-swap", Folder: root})
	if err != nil {
		t.Fatal(err)
	}
	room, err := db.CreateRoom(ctx, store.Room{ProjectID: project.ID, Kind: "task", Title: "legacy-swap", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_legacy',?,?,?)`, project.ID, credential.MachineID, root)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateAgent(ctx, store.Agent{Slug: "oldwork", ProjectID: project.ID, RoleID: "role_reviewer_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: credential.MachineID, CheckoutID: "chk_legacy", KeyHash: "hash", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_legacy',?,0,'fresh_with_history',?)`, agent.ID, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runServerAction("stop"); code != 0 {
		t.Fatalf("stop after seeding: %d %q %q", code, out, errOut)
	}
	// The journal the old build will hold, with a turn in it that only the journal knows: this is what keeps an old build from leaving on
	// retire_at_idle, and what the server's store counts as its work while it runs.
	legacy := supervisorJournalPath(root)
	journal, err := supervisor.OpenJournal(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	journalID := journal.ID
	if _, err := journal.RecordStart(ctx, supervisor.StartRecord{TurnID: "turn_legacy", AgentID: agent.ID, Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,journal_id) VALUES ('turn_legacy',?,?,0,?,1,'normal','running',?)`,
			agent.ID, "ses_legacy", room.ID, journalID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The old build: the supervisor of an older build string, started before the server of the new build, on the legacy journal.
	childLog := filepath.Join(root, "supervisor", "old-instance.log")
	logFile, err := os.OpenFile(childLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(aimebuCopy(t), "supervisor", "--local", "--journal", legacy)
	// The environment is passed as the daemon passes its own: the test's isolated HOME and AIMEBU_* keep the child off the real machine.
	child.Env = append(os.Environ(), "AIMEBU_TEST_VERSION=build-A")
	child.Stdout, child.Stderr = logFile, logFile
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childPID := child.Process.Pid
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
		_ = logFile.Close()
	})
	// Its lock file gets the one pid line the previous build wrote: the journal ID line is taken back out.
	deadline := time.Now().Add(30 * time.Second)
	var lockID string
	for time.Now().Before(deadline) {
		if held, pid, id, err := supervisor.JournalLockInfo(legacy); err == nil && held && pid == strconv.Itoa(childPID) && id != "" {
			lockID = id
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lockID == "" {
		t.Fatalf("the old instance never took its journal lock: %s", tailOfFile(childLog, 20))
	}
	if lockID != journalID {
		t.Fatalf("the old instance reported journal ID %q, want %q", lockID, journalID)
	}
	if err := os.WriteFile(legacy+".lock", []byte(strconv.Itoa(childPID)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The new build: the server starts, whose manager sees a holder with no ID in its lock file.
	t.Setenv("AIMEBU_TEST_VERSION", "build-B")
	useLocalSupervisor(t, root)
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start build B: %d %q %q", code, out, errOut)
	}
	serverLog := func() string { return tailOfFile(serverLogPath(root), 200) }
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		log := serverLog()
		if strings.Contains(log, "next to pid "+strconv.Itoa(childPID)) && strings.Contains(log, "started supervisor instance") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if log := serverLog(); !strings.Contains(log, "next to pid "+strconv.Itoa(childPID)) || !strings.Contains(log, "started supervisor instance") {
		t.Fatalf("no instance was started next to the ID-less holder; server log: %s; old instance log: %s", log, tailOfFile(childLog, 20))
	}
	// While the server sees the old instance's turn, it is never cut, however long the pass takes: three empty-limit windows' worth.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if gone(childPID) {
			t.Fatalf("the ID-less holder was cut while the server still saw its work; server log: %s", serverLog())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if gone(childPID) {
		t.Fatalf("the ID-less holder did not survive the busy phase; server log: %s", serverLog())
	}
	if log := serverLog(); strings.Contains(log, "liveness handover: pid "+strconv.Itoa(childPID)) {
		t.Fatalf("the ID-less holder was handed over while the server still saw its work; server log: %s", log)
	}
	// The work ends in the server's view; the old build still holds its journal (its own journal still lists the turn), so the bounded
	// handover reaches it where the previous build's any-connection rule never did.
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE turn SET exec='exited',outcome='completed',ended_at=? WHERE id='turn_legacy'`, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		log := serverLog()
		if strings.Contains(log, "did not exit; handing over") &&
			strings.Contains(log, "liveness handover: pid "+strconv.Itoa(childPID)+", journal journal.sqlite, stop signal SIGTERM") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	log := serverLog()
	if !strings.Contains(log, "liveness handover: pid "+strconv.Itoa(childPID)) {
		t.Fatalf("the empty ID-less holder was never handed over; server log: %s; old instance log: %s", log, tailOfFile(childLog, 30))
	}
	if !strings.Contains(log, "liveness handover: pid "+strconv.Itoa(childPID)+", journal journal.sqlite, stop signal SIGTERM") {
		t.Fatalf("the ID-less holder was not asked to stop with SIGTERM: %s", log)
	}
	if !processGone(childPID, 90*time.Second) {
		t.Fatalf("the old build survived its handover; server log: %s; old instance log: %s", log, tailOfFile(childLog, 30))
	}
	// The current build went on taking the machine's work the whole time.
	if held := func() int {
		views, err := heldSupervisors(root)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, v := range views {
			if v.pid != childPID {
				n++
			}
		}
		return n
	}(); held == 0 {
		t.Fatalf("no current-build instance was left after the handover; server log: %s", log)
	}
}
