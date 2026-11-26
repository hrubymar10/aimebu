package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// startHelper starts a real short-lived helper process of the test's own, in its own process group, for the alive and dead identity cases.
// It is never a harness: sleep does nothing but stay alive until it is signalled. The test reaps it as soon as it ends (an orphan of a dead
// supervisor is reaped by the system, and a zombie would still show its start time), and cleanup kills whatever is left of its group.
func startHelper(t *testing.T) (pid int, start string) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	start = processStart(pid)
	if start == "" {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatal("the system would not say when the helper process started")
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-reaped:
		case <-time.After(waitGuard):
		}
	})
	return pid, start
}

// killHelper kills the helper and waits until the system no longer shows the pid carrying it (the test reaps it, as the system reaps an
// orphan of a dead supervisor).
func killHelper(t *testing.T, pid int, start string) {
	t.Helper()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitGuard)
	for processStart(pid) == start {
		if !time.Now().Before(deadline) {
			t.Fatal("the killed helper process did not disappear")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// helperStarted says whether the helper's pid still carries the process that started when start says it did.
func helperStarted(pid int, start string) bool { return processStart(pid) == start }

// openTestJournal opens a journal under the test's own temp dir; no real journal is ever opened.
func openTestJournal(t *testing.T) *Journal {
	t.Helper()
	journal, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	return journal
}

// recordRunningTurn records one running turn, with or without a process identity written the way RecordTurnProcess writes it.
func recordRunningTurn(t *testing.T, journal *Journal, turnID string, pid, pgid int, start string) {
	t.Helper()
	if _, err := journal.RecordStart(context.Background(), StartRecord{TurnID: turnID, AgentID: "agent_x", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if pid > 0 {
		if _, err := journal.db.Exec(`UPDATE turn_start SET pid=?,pgid=?,pid_start=? WHERE turn_id=?`, pid, pgid, start, turnID); err != nil {
			t.Fatal(err)
		}
	}
}

// assertSettled says the turn row is exited with the supervisor_lost exit.
func assertSettled(t *testing.T, journal *Journal, turnID string) {
	t.Helper()
	exits, err := journal.PendingExits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(exits) != 1 || exits[0].TurnID != turnID || exits[0].Exit.ErrorEvent != "supervisor_lost" {
		t.Fatalf("the turn was not settled as supervisor_lost: %+v", exits)
	}
}

// waitSettled waits for the settle, which a startup performs while the test's main goroutine may already be reading frames.
func waitSettled(t *testing.T, journal *Journal, turnID string) {
	t.Helper()
	deadline := time.Now().Add(waitGuard)
	for time.Now().Before(deadline) {
		exits, err := journal.PendingExits(context.Background())
		if err == nil && len(exits) == 1 && exits[0].TurnID == turnID {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the turn was never settled: %s", turnID)
}

// waitHello waits for the peer to receive the supervisor's hello and returns it.
func waitHello(t *testing.T, peer *machinePeer) protocol.Frame {
	t.Helper()
	select {
	case hello := <-peer.hellos:
		return hello
	case <-time.After(waitGuard):
		t.Fatal("no hello from the supervisor")
		return protocol.Frame{}
	}
}

// runClient runs a client the way production does and stops it when the test is done with it.
func runClient(t *testing.T, c *Client, once bool) {
	t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		if once {
			done <- c.RunOnce(runCtx)
		} else {
			done <- c.Run(runCtx)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			// A stop of the run is not a failure of it: a cancelled context ends the read loop ("failed to get reader", the run
			// context, or nothing at all).
			if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
				t.Errorf("the supervisor run failed: %v", err)
			}
		case <-time.After(waitGuard):
			t.Errorf("the supervisor did not stop")
		}
	})
}

// A hello does not report a dead predecessor's turn as running: the turn's row carries the harness process identity its start recorded,
// the process is gone, and the startup settles the turn before the first hello and replays its supervisor_lost exit (the regression of
// the production alphas, where turns stayed running for days).
func TestRunSettlesDeadPredecessorTurnBeforeHello(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	previous, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	pid, start := startHelper(t)
	killHelper(t, pid, start)
	recordRunningTurn(t, previous, "left-open", pid, pid, start)
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	peer := newMachinePeer(t, func(hello protocol.Frame) protocol.Frame {
		if len(hello.Agents) != 1 || len(hello.Agents[0].OpenTurns) != 1 || hello.Agents[0].OpenTurns[0].Exec != "exited" {
			t.Errorf("dead turn in hello: %+v", hello.Agents)
		}
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	runClient(t, c, false)
	process := peer.next(t, protocol.Process)
	if process.TurnID != "left-open" || process.Exit == nil || process.Exit.ErrorEvent != "supervisor_lost" {
		t.Fatalf("dead turn was not replayed: %+v", process)
	}
	assertSettled(t, journal, "left-open")
}

// An alive orphan whose output nobody will deliver is terminated (its group: SIGTERM, a bounded wait, SIGKILL) and its turn is settled.
func TestStartupTerminatesAnAliveOrphanAndSettlesItsTurn(t *testing.T) {
	journal := openTestJournal(t)
	pid, start := startHelper(t)
	recordRunningTurn(t, journal, "turn-orphan", pid, pid, start)
	peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	runClient(t, c, false)
	waitHello(t, peer)
	waitSettled(t, journal, "turn-orphan")
	deadline := time.Now().Add(waitGuard)
	for helperStarted(pid, start) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if helperStarted(pid, start) {
		t.Fatal("the alive orphan was not terminated")
	}
	assertSettled(t, journal, "turn-orphan")
}

// A pid that a later process reused (its start time differs from the journaled one) is never signalled, and the turn is settled as dead.
func TestStartupSettlesAReusedPidWithoutTouchingIt(t *testing.T) {
	journal := openTestJournal(t)
	pid, start := startHelper(t)
	// The row says the turn's process began at another time than the process now on the pid: the pid was reused.
	recordRunningTurn(t, journal, "turn-reused", pid, pid, start+" (never)")
	peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	runClient(t, c, false)
	waitHello(t, peer)
	waitSettled(t, journal, "turn-reused")
	if !helperStarted(pid, start) {
		t.Fatal("the helper process was signalled although its start time differs from the journaled one")
	}
	assertSettled(t, journal, "turn-reused")
}

// A legacy row without an identity is untouched at startup: a regular instance reports it in hello, and only the drain path settles it,
// the way it always has.
func TestStartupLeavesALegacyRowToTheDrain(t *testing.T) {
	journal := openTestJournal(t)
	recordRunningTurn(t, journal, "turn-legacy", 0, 0, "")
	peer := newMachinePeer(t, func(hello protocol.Frame) protocol.Frame {
		if len(hello.Agents) != 1 || len(hello.Agents[0].OpenTurns) != 1 || hello.Agents[0].OpenTurns[0].Exec != "running" {
			t.Errorf("the legacy row was not left running in hello: %+v", hello.Agents)
		}
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	runClient(t, c, false)
	waitHello(t, peer)
	turns, err := journal.RunningTurnProcesses(context.Background())
	if err != nil || len(turns) != 1 || turns[0].TurnID != "turn-legacy" {
		t.Fatalf("the legacy row was touched at startup: %+v %v", turns, err)
	}
}

// A turn of the other supervisor instance of a side-by-side upgrade has a journal of its own, and is never touched by this one.
func TestStartupNeverTouchesAnotherJournalsTurns(t *testing.T) {
	other := openTestJournal(t)
	pid, start := startHelper(t)
	recordRunningTurn(t, other, "turn-other", pid, pid, start)
	journal := openTestJournal(t)
	peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	runClient(t, c, false)
	waitHello(t, peer)
	turns, err := other.RunningTurnProcesses(context.Background())
	if err != nil || len(turns) != 1 || turns[0].TurnID != "turn-other" {
		t.Fatalf("the other journal's turn was touched: %+v %v", turns, err)
	}
	if !helperStarted(pid, start) {
		t.Fatal("the other journal's process was signalled")
	}
}

// A drain applies the same identity check in place of the old assumption that every row is dead: an alive orphan is terminated and
// settled, a row whose process is gone is settled, and a legacy row without an identity keeps the drain's old behaviour.
func TestDrainChecksIdentityBeforeEndingTurns(t *testing.T) {
	journal := openTestJournal(t)
	alivePID, aliveStart := startHelper(t)
	recordRunningTurn(t, journal, "turn-orphan", alivePID, alivePID, aliveStart)
	deadPID, deadStart := startHelper(t)
	killHelper(t, deadPID, deadStart)
	recordRunningTurn(t, journal, "turn-dead", deadPID, deadPID, deadStart)
	recordRunningTurn(t, journal, "turn-legacy", 0, 0, "")
	peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	c.DrainOnly = true
	runClient(t, c, false)
	waitHello(t, peer)
	seen := map[string]bool{}
	deadline := time.After(waitGuard)
	for len(seen) < 3 {
		select {
		case frame := <-peer.in:
			if frame.Type == protocol.Process && frame.Exit != nil {
				seen[frame.TurnID] = true
			}
		case <-deadline:
			t.Fatalf("the drain did not replay every settled turn: %+v", seen)
		}
	}
	if helperStarted(alivePID, aliveStart) {
		t.Fatal("the drain did not terminate the alive orphan")
	}
	for _, turnID := range []string{"turn-orphan", "turn-dead", "turn-legacy"} {
		exits, err := journal.PendingExits(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, exit := range exits {
			found = found || exit.TurnID == turnID && exit.Exit.ErrorEvent == "supervisor_lost"
		}
		if !found {
			t.Fatalf("the drain did not settle %s: %+v", turnID, exits)
		}
	}
}

// A delivery journals the harness process identity of the turn it started, so a later supervisor can settle the turn by that identity.
func TestDeliveryRecordsTheHarnessProcessIdentity(t *testing.T) {
	journal := openTestJournal(t)
	pid, _ := startHelper(t)
	fake := adapter.NewFake()
	fake.SetProcessIdentity(func(adapter.TurnSpec) (int, int) { return pid, pid })
	gate := make(chan struct{})
	fake.Script("turn-identity",
		adapter.Step{Kind: adapter.StepGate, Gate: gate},
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	peer := newMachinePeer(t, activeAck("agent_x"))
	c := NewClient("machine", "key", peer.server.URL, journal, fake)
	c.OnError = func(error) {}
	runClient(t, c, true)
	peer.out <- testDelivery("agent_x", "turn-identity", "fake", "test", t.TempDir())
	deadline := time.After(waitGuard)
	for {
		turns, err := journal.RunningTurnProcesses(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) == 1 && turns[0].TurnID == "turn-identity" && turns[0].Pid == pid && turns[0].Pgid == pid && turns[0].Start != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the delivery recorded no process identity: %+v", turns)
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(gate)
}

// A lookup that cannot tell (ps missing, not runnable, out of permission, anything else) proves nothing: the row stays running, no signal
// is sent, and a later pass with a working lookup settles it. Red on 462450b8, where an empty answer from the lookup was read as "gone".
func TestStartupLeavesARowUntouchedWhenTheLookupCannotTell(t *testing.T) {
	journal := openTestJournal(t)
	pid, start := startHelper(t)
	recordRunningTurn(t, journal, "turn-unclear", pid, pid, start)
	peer := newMachinePeer(t, func(hello protocol.Frame) protocol.Frame {
		if len(hello.Agents) != 1 || len(hello.Agents[0].OpenTurns) != 1 || hello.Agents[0].OpenTurns[0].Exec != "running" {
			t.Errorf("the unclear row was not left running in hello: %+v", hello.Agents)
		}
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	c := NewClient("machine", "key", peer.server.URL, journal, adapter.NewFake())
	c.ProcessStartState = func(int) (ProcessStartState, string) { return StartUnknown, "" }
	runClient(t, c, false)
	waitHello(t, peer)
	turns, err := journal.RunningTurnProcesses(context.Background())
	if err != nil || len(turns) != 1 || turns[0].TurnID != "turn-unclear" {
		t.Fatalf("the unclear row was settled: %+v %v", turns, err)
	}
	if !helperStarted(pid, start) {
		t.Fatal("the helper process was signalled although the lookup could not tell")
	}
	// A later pass with a working lookup settles it: the hook now delegates to the real system lookup (the recorded start matches while
	// the helper lives, and the SIGTERM the settle sends is answered by a StartGone), so the group's end is verified and the row settles.
	c.ProcessStartState = processStartState
	deadline := time.Now().Add(2 * orphanTermGrace)
	for time.Now().Before(deadline) {
		if _, err := c.settleDeadTurns(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		exits, err := journal.PendingExits(context.Background())
		if err == nil && len(exits) == 1 && exits[0].TurnID == "turn-unclear" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if helperStarted(pid, start) {
		t.Fatal("the orphan survived the settle")
	}
	assertSettled(t, journal, "turn-unclear")
}

// A lookup that ran and reports no such process settles the turn.
func TestStartupSettlesARowWhenTheLookupFindsNoProcess(t *testing.T) {
	journal := openTestJournal(t)
	pid, start := startHelper(t)
	killHelper(t, pid, start)
	recordRunningTurn(t, journal, "turn-gone", pid, pid, start)
	c := NewClient("machine", "key", "http://unused.invalid", journal, adapter.NewFake())
	c.ProcessStartState = func(int) (ProcessStartState, string) { return StartGone, "" }
	n, err := c.settleDeadTurns(context.Background(), false)
	if err != nil || n != 1 {
		t.Fatalf("settleDeadTurns: %d settled, err %v", n, err)
	}
	assertSettled(t, journal, "turn-gone")
}

// A journal written by a supervisor that kept no process identity opens with the new columns added and its rows read as unknown, and a
// row that carries an identity is read back exactly as it was written. A turn whose row is already gone records nothing, and is not an error.
func TestJournalMigratesTurnProcessColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The turn_start of a build before the identity existed, without the new columns.
	_, err = legacy.Exec(`CREATE TABLE turn_start(
		turn_id TEXT PRIMARY KEY,agent_id TEXT NOT NULL,generation INTEGER NOT NULL,
		desired_rev INTEGER NOT NULL,note_ids_json TEXT NOT NULL,mode TEXT NOT NULL,
		spec_json BLOB NOT NULL,started_at TEXT NOT NULL,exec TEXT NOT NULL DEFAULT 'running',
		exit_code INTEGER,terminal_event INTEGER,cancel_intent INTEGER,
		exit_acked INTEGER NOT NULL DEFAULT 0);
		INSERT INTO turn_start(turn_id,agent_id,generation,desired_rev,note_ids_json,mode,spec_json,started_at)
		VALUES ('turn-old','agent_x',0,0,'[]','normal','{}','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	turns, err := journal.RunningTurnProcesses(ctx)
	if err != nil || len(turns) != 1 || turns[0].TurnID != "turn-old" || turns[0].Pid != 0 || turns[0].Start != "" {
		t.Fatalf("the legacy row does not read as unknown: %+v %v", turns, err)
	}
	pid, start := startHelper(t)
	if err := journal.RecordTurnProcess(ctx, "turn-old", pid, pid, start); err != nil {
		t.Fatal(err)
	}
	turns, err = journal.RunningTurnProcesses(ctx)
	if err != nil || len(turns) != 1 || turns[0].Pid != pid || turns[0].Pgid != pid || turns[0].Start != start {
		t.Fatalf("the recorded identity does not read back: %+v %v", turns, err)
	}
	if err := journal.RecordExit(ctx, "turn-old", 0, true, false); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnProcess(ctx, "turn-old", pid, pid, start); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("recording an identity for an ended turn: %v", err)
	}
}
