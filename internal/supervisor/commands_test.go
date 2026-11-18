package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
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

// A command is refused with what to update when the agent runs in Docker and its launcher cannot run one in the container: most agents run
// in Docker, so the message has to say exactly that.
func TestCommandRefusesDockerLauncherWithoutTheFeature(t *testing.T) {
	c, _, _ := commandTestClient(t)
	c.Launchers = []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "ln_docker", Tool: "claude",
		Name: "harness-docker", Containerized: true}, Available: true}}
	checkout := t.TempDir()
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_docker", AgentID: "agt_one", Generation: 1,
		LauncherID: "ln_docker", CheckoutPath: checkout, Cmd: "true", TimeoutSeconds: 60})
	if result.Result != "error" {
		t.Fatalf("a Docker launcher without the feature must be refused: %+v", result)
	}
	if result.Message != ErrCommandDockerUnsupported.Error() {
		t.Fatalf("wrong refusal: %q", result.Message)
	}
	if !strings.Contains(result.Message, "update harness-docker") || !strings.Contains(result.Message, "run_command") {
		t.Fatalf("the refusal does not say what to update: %q", result.Message)
	}
	// The same launcher with the feature is not silently run on the host: this build has no container path yet.
	c.Launchers[0].Features = []string{commandRunFeature}
	result = c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_docker2", AgentID: "agt_one", Generation: 1,
		LauncherID: "ln_docker", CheckoutPath: checkout, Cmd: "true", TimeoutSeconds: 60})
	if result.Result != "error" || result.Message != ErrCommandContainerUnsupported.Error() {
		t.Fatalf("a container command must not fall back to the host: %+v", result)
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
	if lost := exits["cmd_lost"]; !lost.Lost || lost.ExitCode != nil {
		t.Fatalf("a job with no marker and no process is lost: %+v", lost)
	}
	if open, err := j.OpenCommands(t.Context()); err != nil || len(open) != 0 {
		t.Fatalf("the journal still lists them as open: %v %v", open, err)
	}
}
