package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func commandTestClient(t *testing.T) (*Client, *Journal, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := OpenJournal(t.Context(), filepath.Join(dir, "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	c := NewClient("mch_test", "key", "http://127.0.0.1:1", j, adapter.NewFake())
	c.OnError = func(error) {}
	return c, j, dir
}

func TestIdleExitWaitsForAdoptedCommand(t *testing.T) {
	c, journal, _ := commandTestClient(t)
	if c.hasActiveWork() {
		t.Fatal("idle supervisor reported work")
	}
	if _, err := journal.RecordCommandStart(t.Context(), CommandStartRecord{JobID: "job_adopted", AgentID: "agt_test", Pid: 1, Cmd: "sleep", LogPath: "log"}); err != nil {
		t.Fatal(err)
	}
	if !c.hasActiveWork() {
		t.Fatal("journaled command was ignored")
	}
	if err := journal.RecordCommandExit(t.Context(), protocol.CommandExit{JobID: "job_adopted", Lost: true}); err != nil {
		t.Fatal(err)
	}
	if c.hasActiveWork() {
		t.Fatal("finished command still held the supervisor")
	}
}

func TestIdleDisconnectExit(t *testing.T) {
	c, _, _ := commandTestClient(t)
	c.IdleDisconnectLimit = 30 * time.Millisecond
	c.ReconnectDelay = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := c.Run(ctx); err != nil || ctx.Err() != nil {
		t.Fatalf("idle disconnect exit: err=%v context=%v", err, ctx.Err())
	}
}

func waitCommandExit(t *testing.T, sink chan protocol.CommandExit) protocol.CommandExit {
	t.Helper()
	select {
	case exit := <-sink:
		return exit
	case <-time.After(30 * time.Second):
		t.Fatal("the command's end was never reported")
		return protocol.CommandExit{}
	}
}

// A long command runs outside the turn, in the agent's checkout: its end carries the exit code, the tail of its output (redacted) and the
// journal holds the evidence.
func TestCommandRunsOutsideATurnAndReportsItsEnd(t *testing.T) {
	c, j, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	checkout := t.TempDir()
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_test", AgentID: "agt_one", Generation: 1,
		CheckoutPath: checkout, Cmd: "echo 'Bearer aak_secret_value' && echo done && exit 3", TimeoutSeconds: 60})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	if result.LogPath != filepath.Join(checkout, ".aimebu", "commands", "cmd_test.log") {
		t.Fatalf("log path %q", result.LogPath)
	}
	exit := waitCommandExit(t, sink)
	if exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("exit %+v", exit)
	}
	if exit.Killed || exit.TimedOut || exit.Lost {
		t.Fatalf("a clean exit is none of those: %+v", exit)
	}
	if strings.Contains(exit.Tail, "aak_secret_value") || !strings.Contains(exit.Tail, "Bearer [redacted]") {
		t.Fatalf("the tail is not redacted: %q", exit.Tail)
	}
	if !strings.Contains(exit.Tail, "done") {
		t.Fatalf("the tail lost the output: %q", exit.Tail)
	}
	data, err := os.ReadFile(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "aak_secret_value") {
		t.Fatalf("the log kept the secret: %q", data)
	}
	if _, err := os.Stat(commandExitMarker(result.LogPath)); err != nil {
		t.Fatalf("no exit marker was written: %v", err)
	}
	open, err := j.OpenCommands(t.Context())
	if err != nil || len(open) != 0 {
		t.Fatalf("the journal still lists the command as open: %v %v", open, err)
	}
	exits, err := j.PendingCommandExits(t.Context())
	if err != nil || len(exits) != 1 || exits[0].JobID != "cmd_test" {
		t.Fatalf("pending exits %v %v", exits, err)
	}
}

// A command that never ends is killed at its timeout, and says so.
func TestCommandTimeoutKillsTheProcess(t *testing.T) {
	c, _, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_sleep", AgentID: "agt_one", Generation: 1,
		CheckoutPath: t.TempDir(), Cmd: "sleep 60", TimeoutSeconds: 1})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	exit := waitCommandExit(t, sink)
	if !exit.TimedOut || !exit.Killed {
		t.Fatalf("a timed-out command is a kill: %+v", exit)
	}
}

// A cancel ends the process group and reports the end as killed.
func TestCommandCancelKillsTheProcessGroup(t *testing.T) {
	c, _, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_cancel", AgentID: "agt_one", Generation: 1,
		CheckoutPath: t.TempDir(), Cmd: "sleep 60", TimeoutSeconds: 300})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	ack := c.commandCancel("cmd_cancel")
	if ack.Result != "ok" {
		t.Fatalf("cancel: %+v", ack)
	}
	exit := waitCommandExit(t, sink)
	if !exit.Killed || exit.TimedOut {
		t.Fatalf("a cancel is not a timeout: %+v", exit)
	}
	if c.commands.has("cmd_cancel") {
		t.Fatal("the job is still held as running")
	}
}

// Output beyond the log's cap is dropped with a marker, and the tail the note carries is bounded.
func TestCommandLogCapsHugeOutput(t *testing.T) {
	c, _, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_huge", AgentID: "agt_one", Generation: 1,
		CheckoutPath: t.TempDir(), Cmd: "head -c 12000000 /dev/zero | tr '\\0' 'x'", TimeoutSeconds: 120})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	exit := waitCommandExit(t, sink)
	if !exit.TailTruncated {
		t.Fatalf("the output was not marked truncated: %+v", exit)
	}
	if len(exit.Tail) > commandTailBytes {
		t.Fatalf("the tail is %d bytes", len(exit.Tail))
	}
	info, err := os.Stat(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > commandLogMaxBytes {
		t.Fatalf("the log is %d bytes", info.Size())
	}
}

// A job of a dead predecessor is reconciled at startup: the exit marker says how it ended, and one with no marker and no process is lost.
func TestAdoptedCommandIsReconciledFromItsExitMarker(t *testing.T) {
	c, j, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 2)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	checkout := t.TempDir()
	logPath := filepath.Join(checkout, ".aimebu", "commands", "cmd_old.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("ran\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandExitMarker(logPath), []byte("7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordCommandStart(t.Context(), CommandStartRecord{JobID: "cmd_old", AgentID: "agt_one", Generation: 1,
		Pid: 999999, LogPath: logPath}); err != nil {
		t.Fatal(err)
	}
	lostLog := filepath.Join(checkout, ".aimebu", "commands", "cmd_lost.log")
	if err := os.WriteFile(lostLog, []byte("nothing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordCommandStart(t.Context(), CommandStartRecord{JobID: "cmd_lost", AgentID: "agt_one", Generation: 1,
		Pid: 999998, LogPath: lostLog}); err != nil {
		t.Fatal(err)
	}
	c.reconcileCommands(t.Context())
	first := waitCommandExit(t, sink)
	second := waitCommandExit(t, sink)
	exits := map[string]protocol.CommandExit{first.JobID: first, second.JobID: second}
	if code := exits["cmd_old"]; code.ExitCode == nil || *code.ExitCode != 7 || code.Lost {
		t.Fatalf("the marker's exit code was not reported: %+v", code)
	}
	if lost := exits["cmd_lost"]; !lost.Lost || lost.ExitCode == nil || *lost.ExitCode != -1 || lost.Reason != protocol.CommandEndLostInSwap {
		t.Fatalf("a job with no marker and no process is lost in the swap (exit -1): %+v", lost)
	}
	if open, err := j.OpenCommands(t.Context()); err != nil || len(open) != 0 {
		t.Fatalf("the journal still lists them as open: %v %v", open, err)
	}
}

// startPredecessorCommand stands in for a command a dead predecessor started: a live process group this supervisor did not start, and its
// journal record.
func startPredecessorCommand(t *testing.T, c *Client, j *Journal, jobID string, timeoutSeconds int, startedAt time.Time) int {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), ".aimebu", "commands", jobID+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("still running\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }() // the test reaps its own child, as init would reap a predecessor's
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); <-reaped })
	if _, err := j.RecordCommandStart(t.Context(), CommandStartRecord{JobID: jobID, AgentID: "agt_one", Generation: 1, Pid: pid,
		PidStart: c.identityOf(pid), Cmd: "sleep 60", LogPath: logPath, StartedAt: startedAt.UTC().Format(commandTimeLayout),
		TimeoutSeconds: timeoutSeconds}); err != nil {
		t.Fatal(err)
	}
	return pid
}

// A command adopted after a restart is held like one this supervisor started: a cancel, a stop of its agent and its timeout all reach it,
// and its end says it was killed.
func TestAdoptedCommandIsReachedByCancelStopAndTimeout(t *testing.T) {
	for _, how := range []string{"cancel", "stop", "timeout"} {
		t.Run(how, func(t *testing.T) {
			c, j, _ := commandTestClient(t)
			sink := make(chan protocol.CommandExit, 4)
			c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
			started := time.Now()
			if how == "timeout" {
				started = started.Add(-2 * time.Hour) // its hour ran out while no supervisor watched it
			}
			pid := startPredecessorCommand(t, c, j, "cmd_adopted", 3600, started)
			c.reconcileCommands(t.Context())
			if !c.runner().has("cmd_adopted") {
				t.Fatal("the adopted command is not held")
			}
			switch how {
			case "cancel":
				if result := c.commandCancel("cmd_adopted"); result.Result != "ok" {
					t.Fatalf("cancel: %+v", result)
				}
			case "stop":
				c.killAgentCommands("agt_one")
			}
			exit := waitCommandExit(t, sink)
			if exit.JobID != "cmd_adopted" || !exit.Killed || exit.Lost || exit.TimedOut != (how == "timeout") {
				t.Fatalf("exit %+v", exit)
			}
			if commandProcessAlive(pid) {
				t.Fatal("the adopted process is still running")
			}
			if c.runner().has("cmd_adopted") {
				t.Fatal("the job is still held")
			}
		})
	}
}

// A journaled pid that now belongs to another process (its start time differs) is not the command: it is reported lost and never
// signalled.
func TestAdoptedCommandWithAReusedPidIsLost(t *testing.T) {
	c, j, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 2)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	logPath := filepath.Join(t.TempDir(), "cmd_reused.log")
	if err := os.WriteFile(logPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// This test's own pid is alive, but it was not started at the journaled time.
	if _, err := j.RecordCommandStart(t.Context(), CommandStartRecord{JobID: "cmd_reused", AgentID: "agt_one", Generation: 1,
		Pid: os.Getpid(), PidStart: "Thu Jan  1 00:00:00 1970", LogPath: logPath, TimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	c.reconcileCommands(t.Context())
	exit := waitCommandExit(t, sink)
	if exit.JobID != "cmd_reused" || !exit.Lost || exit.Killed || exit.ExitCode == nil || *exit.ExitCode != -1 ||
		exit.Reason != protocol.CommandEndLostInSwap {
		t.Fatalf("exit %+v", exit)
	}
	if c.runner().has("cmd_reused") {
		t.Fatal("the job is still held")
	}
}

// Reconciling runs with every connection: a command this supervisor runs itself is not adopted a second time, and its end is reported once,
// with how it ended.
func TestReconcileLeavesTheSupervisorsOwnCommandsAlone(t *testing.T) {
	c, _, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 4)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_own", AgentID: "agt_one", Generation: 1,
		CheckoutPath: t.TempDir(), Cmd: "sleep 60", TimeoutSeconds: 300})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	own := c.runner().get("cmd_own")
	c.reconcileCommands(t.Context())
	c.reconcileCommands(t.Context())
	if c.runner().get("cmd_own") != own || own.adopted {
		t.Fatal("the supervisor's own command was adopted")
	}
	if ack := c.commandCancel("cmd_own"); ack.Result != "ok" {
		t.Fatalf("cancel: %+v", ack)
	}
	exit := waitCommandExit(t, sink)
	if !exit.Killed {
		t.Fatalf("exit %+v", exit)
	}
	select {
	case again := <-sink:
		t.Fatalf("the end was reported twice: %+v", again)
	case <-time.After(commandWaitPoll + time.Second):
	}
}

// A cwd may not leave the checkout, however it is spelled.
func TestCommandCwdMayNotLeaveTheCheckout(t *testing.T) {
	c, _, _ := commandTestClient(t)
	checkout := t.TempDir()
	for i, cwd := range []string{"..", "./..", "a/../..", "../x", "/tmp"} {
		result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_cwd" + strconv.Itoa(i), AgentID: "agt_one", Generation: 1,
			CheckoutPath: checkout, Cmd: "true", Cwd: cwd, TimeoutSeconds: 60})
		if result.Result != "error" || !strings.Contains(result.Message, "cwd") {
			t.Fatalf("cwd %q: %+v", cwd, result)
		}
	}
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_cwd_ok", AgentID: "agt_one", Generation: 1,
		CheckoutPath: checkout, Cmd: "true", Cwd: "./sub/..", TimeoutSeconds: 60})
	if result.Result != "ok" || result.VisibleCwd != checkout {
		t.Fatalf("a cwd inside the checkout: %+v", result)
	}
	waitCommandExit(t, sink)
}

// A supervisor that is being replaced does not wait for a long command: its exit ends the command and reports the end with exit code -1
// and the reason supervisor_replaced (journaled, so a replacement replays it if this connection is gone), which wakes the agent.
func TestReplacedSupervisorEndsItsCommandsAsSupervisorReplaced(t *testing.T) {
	c, j, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_gate", AgentID: "agt_one", Generation: 1,
		CheckoutPath: t.TempDir(), Cmd: "sleep 600", TimeoutSeconds: 7200})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	c.MarkRestartInterruption()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	began := time.Now()
	if err := c.StopAcceptingAndDrain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if waited := time.Since(began); waited > 20*time.Second {
		t.Fatalf("the exit waited %s for the command", waited)
	}
	exit := waitCommandExit(t, sink)
	if exit.JobID != "cmd_gate" || exit.ExitCode == nil || *exit.ExitCode != -1 || exit.Reason != protocol.CommandEndSupervisorReplaced ||
		!exit.Killed || exit.TimedOut || exit.Lost {
		t.Fatalf("exit %+v", exit)
	}
	if c.hasActiveWork() {
		t.Fatal("the command still holds the supervisor")
	}
	exits, err := j.PendingCommandExits(t.Context())
	if err != nil || len(exits) != 1 || exits[0].Reason != protocol.CommandEndSupervisorReplaced || exits[0].ExitCode == nil || *exits[0].ExitCode != -1 {
		t.Fatalf("journaled exits %+v %v", exits, err)
	}
}

// A command adopted from a dead predecessor is not this process's to end when it is replaced in turn: it is left running, still open in
// the journal, for the next supervisor to adopt.
func TestReplacedSupervisorLeavesAnAdoptedCommandRunning(t *testing.T) {
	c, j, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	pid := startPredecessorCommand(t, c, j, "cmd_adopted", 3600, time.Now())
	c.reconcileCommands(t.Context())
	c.MarkRestartInterruption()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	if err := c.StopAcceptingAndDrain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	select {
	case exit := <-sink:
		t.Fatalf("a command left to the replacement was reported: %+v", exit)
	case <-time.After(3 * commandWaitPoll):
	}
	if !commandProcessAlive(pid) {
		t.Fatal("the adopted process was ended")
	}
	if open, err := j.OpenCommands(t.Context()); err != nil || len(open) != 1 {
		t.Fatalf("the command is not open in the journal: %v %v", open, err)
	}
}

// detachableClient is a supervisor whose long commands write their own log through this test binary (TestMain runs it as
// `command-log`), as the aimebu binary does for a real supervisor.
func detachableClient(t *testing.T, journalPath string) (*Client, *Journal, chan protocol.CommandExit) {
	t.Helper()
	j, err := OpenJournal(t.Context(), journalPath)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient("mch_test", "key", "http://127.0.0.1:1", j, adapter.NewFake())
	c.OnError = func(error) {}
	c.CommandLogger = func() (string, error) { return filepath.Abs(os.Args[0]) }
	sink := make(chan protocol.CommandExit, 4)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	return c, j, sink
}

// A command run in its own session, with its own log writer, still reports its end, the redacted tail and the log to the supervisor that
// started it.
func TestDetachableCommandReportsItsEnd(t *testing.T) {
	c, j, sink := detachableClient(t, filepath.Join(t.TempDir(), "journal.sqlite"))
	defer j.Close()
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_own", AgentID: "agt_one", Generation: 1,
		CheckoutPath: t.TempDir(), Cmd: "echo 'Bearer aak_secret_value'; echo done; exit 3", TimeoutSeconds: 60})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	exit := waitCommandExit(t, sink)
	if exit.ExitCode == nil || *exit.ExitCode != 3 || exit.Killed || exit.Lost || !strings.Contains(exit.Tail, "done") {
		t.Fatalf("exit %+v", exit)
	}
	data, err := os.ReadFile(result.LogPath)
	if err != nil || strings.Contains(string(data), "aak_secret_value") || !strings.Contains(string(data), "Bearer [redacted]") {
		t.Fatalf("log %q %v", data, err)
	}
	if code, ok := readExitMarker(commandExitMarker(result.LogPath)); !ok || code != 3 {
		t.Fatalf("exit marker %d %t", code, ok)
	}
}

// The swap: a command started on supervisor A survives A's exit for its replacement (it is not ended, and A reports nothing), and B,
// opening the same journal, adopts it and reports its real end, exit code and output, which wakes the agent. A cancel reaches a command B
// adopted.
func TestCommandSurvivesTheSwapAndIsAdoptedByTheReplacement(t *testing.T) {
	for _, how := range []string{"finishes", "cancelled"} {
		t.Run(how, func(t *testing.T) {
			journalPath := filepath.Join(t.TempDir(), "journal.sqlite")
			a, ja, sinkA := detachableClient(t, journalPath)
			script := "sleep 2; echo 'output after the swap'; exit 7"
			if how == "cancelled" {
				script = "sleep 600"
			}
			result := a.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_gate", AgentID: "agt_one", Generation: 1,
				CheckoutPath: t.TempDir(), Cmd: script, TimeoutSeconds: 3600})
			if result.Result != "ok" {
				t.Fatalf("start: %+v", result)
			}
			pid := a.runner().get("cmd_gate").pid
			t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
			a.MarkRestartInterruption()
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			began := time.Now()
			if err := a.StopAcceptingAndDrain(ctx); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if waited := time.Since(began); waited > 5*time.Second {
				t.Fatalf("A's exit waited %s for the command", waited)
			}
			if a.hasRetireBlockingWork() {
				t.Fatal("the command still holds A")
			}
			if err := ja.Close(); err != nil {
				t.Fatal(err)
			}
			if !commandProcessAlive(pid) {
				t.Fatal("the command did not survive A's exit")
			}
			b, jb, sinkB := detachableClient(t, journalPath)
			defer jb.Close()
			b.reconcileCommands(t.Context())
			if !b.runner().has("cmd_gate") {
				t.Fatal("B did not adopt the command")
			}
			if how == "cancelled" {
				if r := b.commandCancel("cmd_gate"); r.Result != "ok" {
					t.Fatalf("cancel: %+v", r)
				}
			}
			exit := waitCommandExit(t, sinkB)
			switch how {
			case "finishes":
				if exit.ExitCode == nil || *exit.ExitCode != 7 || exit.Lost || exit.Killed || exit.Reason != "" ||
					!strings.Contains(exit.Tail, "output after the swap") {
					t.Fatalf("exit %+v", exit)
				}
			case "cancelled":
				if !exit.Killed || exit.Lost || exit.Reason != "" {
					t.Fatalf("exit %+v", exit)
				}
				if commandProcessAlive(pid) {
					t.Fatal("the cancelled command still runs")
				}
			}
			select {
			case got := <-sinkA:
				t.Fatalf("A reported the command it left: %+v", got)
			default:
			}
		})
	}
}
