package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/opencode"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// fakeDockerLauncher writes a launcher that implements harness-docker's side of the contract (docs/design/harness-docker-run-command.md
// and harness-docker's docs/lifecycle.md) without Docker: the feature probe, the command mode with its signal handling, and the activity
// probe. features is what it prints; mode picks how it behaves in command mode:
//
//   - "run": runs the command (in the mapped cwd); a TERM ends it and the launcher exits 143, the confirmed stop;
//   - "unconfirmed": the same, but the launcher cannot confirm the stop and exits 125;
//   - "old-image": the container's image predates command runs: it refuses with 126 and runs nothing;
//   - "leftover": runs the command, but first leaves a host-side process behind that holds its output (its pids in <dir>/leftover).
//
// The command mode records what it was given in <dir>/record; every feature probe appends a line to <dir>/probes.
func fakeDockerLauncher(t *testing.T, features []string, mode string) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "claude-docker")
	stop := "143"
	if mode == "unconfirmed" {
		stop = "125"
	}
	// The launcher runs with aimebu's control environment, and the tests with their own PATH: neither reaches the host's tools by name.
	pgrep := ""
	for _, candidate := range []string{"/usr/bin/pgrep", "/bin/pgrep"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			pgrep = candidate
			break
		}
	}
	if pgrep == "" {
		t.Fatal("the fake launcher needs pgrep in /usr/bin or /bin")
	}
	refuse, leftover := "", ""
	if mode == "leftover" {
		leftover = "  sleep 120 & echo $! >> '" + dir + "/leftover'\n"
	}
	if mode == "old-image" {
		refuse = "echo \"Error: the harness container's session wrapper cannot run commands: rebuild it with bin/harness-docker-ctrl rebuild.\" >&2; exit 126\n"
	}
	source := "#!/bin/sh\n" +
		"if [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo probe >> '" + dir + "/probes'; printf '%s\\n' " + strings.Join(features, " ") + "; exit 0; fi\n" +
		"if [ -n \"${HARNESS_DOCKER_RUN_COMMAND+x}\" ]; then\n" +
		"  [ -n \"$HARNESS_DOCKER_RUN_COMMAND\" ] || { echo 'Error: HARNESS_DOCKER_RUN_COMMAND is empty.' >&2; exit 1; }\n" +
		"  { echo \"CWD=[$HARNESS_DOCKER_CWD]\"; echo \"MARKER=$HARNESS_DOCKER_EXIT_MARKER\"; echo \"TIMEOUT=$HARNESS_DOCKER_TIMEOUT_SECONDS\";" +
		" echo \"KEY=${AIMEBU_AGENT_KEY-<unset>}\"; echo \"FORWARD=${HARNESS_DOCKER_FORWARD_ENV-<unset>}\"; echo \"REPORT=${HARNESS_DOCKER_REPORT_EXEC_ID-<unset>}\";" +
		" echo \"PWD=$(pwd -P)\"; echo \"ARGS=$*\"; } > '" + dir + "/record'\n" +
		"  " + refuse +
		"  case \"$HARNESS_DOCKER_CWD\" in /*|*..*) echo 'Error: bad HARNESS_DOCKER_CWD' >&2; exit 1;; esac\n" +
		"  case \"$HARNESS_DOCKER_EXIT_MARKER\" in \"$(pwd)\"/?*) ;; *) echo 'Error: HARNESS_DOCKER_EXIT_MARKER must be inside the current directory.' >&2; exit 1;; esac\n" +
		"  cd \"./$HARNESS_DOCKER_CWD\" || exit 1\n" +
		// The stop ends the whole session, its command included (each parent before its children), before the launcher exits: a killed
		// command writes no exit marker.
		"  killtree() { kids=$(" + pgrep + " -P \"$1\"); kill -KILL \"$1\" 2>/dev/null; for kid in $kids; do killtree \"$kid\"; done; }\n" +
		leftover +
		"  sh -c \"$HARNESS_DOCKER_RUN_COMMAND\" </dev/null &\n" +
		"  child=$!\n" +
		"  trap ': > " + dir + "/termed; killtree $child; wait $child 2>/dev/null; : > " + dir + "/confirmed; exit " + stop + "' TERM\n" +
		"  : > '" + dir + "/trapped'\n" +
		"  wait $child\n" +
		"  exit $?\n" +
		"fi\n" +
		"echo 'unexpected: the fake launcher was asked to start a harness' >&2\n" +
		"exit 99\n"
	if err := testenv.WriteExecutable(path, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func dockerLauncherRow(path string) protocol.LauncherRow {
	return protocol.LauncherRow{LauncherDefinition: protocol.LauncherDefinition{ID: "ln_docker", Tool: "claude", Name: "harness-docker",
		Command: path, Containerized: true, Enabled: true}, ResolvedCommand: path, Available: true}
}

func readRecord(t *testing.T, dir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "record"))
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, _ := strings.Cut(line, "=")
		record[name] = value
	}
	return record
}

// A Docker launcher without run_command is refused exactly as before, with what to update; the launcher is never asked to run anything.
func TestDockerCommandWithoutTheFeatureIsRefused(t *testing.T) {
	c, _, _ := commandTestClient(t)
	path, dir := fakeDockerLauncher(t, []string{"forward_env", "stop_on_signal"}, "run")
	c.Launchers = []protocol.LauncherRow{dockerLauncherRow(path)}
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_nofeature", AgentID: "agt_one", Generation: 1,
		LauncherID: "ln_docker", CheckoutPath: t.TempDir(), Cmd: "true", TimeoutSeconds: 60})
	if result.Result != "error" || result.Message != ErrCommandDockerUnsupported.Error() {
		t.Fatalf("a Docker launcher without the feature must be refused: %+v", result)
	}
	if !strings.Contains(result.Message, "update harness-docker") || !strings.Contains(result.Message, "run_command") {
		t.Fatalf("the refusal does not say what to update: %q", result.Message)
	}
	if _, err := os.Stat(filepath.Join(dir, "record")); err == nil {
		t.Fatal("the launcher was asked to run the command")
	}
	// A launcher record this machine does not have is refused too: a command never falls back to the host.
	result = c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_nolauncher", AgentID: "agt_one", Generation: 1,
		LauncherID: "ln_gone", CheckoutPath: t.TempDir(), Cmd: "true", TimeoutSeconds: 60})
	if result.Result != "error" || !strings.Contains(result.Message, "not configured") {
		t.Fatalf("an unknown launcher must be refused: %+v", result)
	}
}

// With run_command the command runs through the launcher's command mode: the cwd relative to the checkout, the exit marker inside it, no
// agent key and nothing forwarded; the exit code passes through and the command writes its marker.
func TestDockerCommandRunsThroughTheLauncherAndEnds(t *testing.T) {
	t.Setenv("AIMEBU_AGENT_KEY", "aak_must_not_reach_the_command")
	c, j, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	path, dir := fakeDockerLauncher(t, []string{"forward_env", "stop_on_signal", "run_command", "turn_activity"}, "run")
	row := dockerLauncherRow(path)
	row.Args = []string{"--launcher-arg"}
	c.Launchers = []protocol.LauncherRow{row}
	checkout := canonicalTempDir(t)
	if err := os.Mkdir(filepath.Join(checkout, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_docker", AgentID: "agt_one", Generation: 1,
		LauncherID: "ln_docker", CheckoutPath: checkout, Cwd: "sub",
		Cmd: `echo "in $(basename "$PWD")"; echo "it's quoted"; exit 7`, TimeoutSeconds: 90})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	exit := waitCommandExit(t, sink)
	if exit.ExitCode == nil || *exit.ExitCode != 7 || exit.Killed || exit.Unconfirmed || exit.Refused != "" || exit.Lost {
		t.Fatalf("exit %+v", exit)
	}
	if !strings.Contains(exit.Tail, "in sub") || !strings.Contains(exit.Tail, "it's quoted") {
		t.Fatalf("the output did not stream into the log: %q", exit.Tail)
	}
	record := readRecord(t, dir)
	marker := commandExitMarker(filepath.Join(checkout, ".aimebu", "commands", "cmd_docker.log"))
	want := map[string]string{"CWD": "[sub]", "MARKER": marker, "TIMEOUT": "90", "KEY": "<unset>", "FORWARD": "<unset>",
		"REPORT": "<unset>", "PWD": checkout, "ARGS": "--launcher-arg"}
	for name, value := range want {
		if record[name] != value {
			t.Errorf("%s = %q, want %q", name, record[name], value)
		}
	}
	if code, found := readExitMarker(marker); !found || code != 7 {
		t.Fatalf("the command did not write its marker: %d %v", code, found)
	}
	start, err := j.CommandStart(t.Context(), "cmd_docker")
	if err != nil || !start.Container {
		t.Fatalf("the journal does not say the command runs in a container: %+v %v", start, err)
	}
}

// The feature answer is kept per launcher and binary: a second command does not ask again, a replaced launcher is asked anew.
func TestDockerCommandFeatureProbeIsCachedPerBinary(t *testing.T) {
	c, _, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 2)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	path, dir := fakeDockerLauncher(t, []string{"forward_env", "run_command"}, "run")
	c.Launchers = []protocol.LauncherRow{dockerLauncherRow(path)}
	probes := func() int {
		data, _ := os.ReadFile(filepath.Join(dir, "probes"))
		return strings.Count(string(data), "probe")
	}
	for _, id := range []string{"cmd_first", "cmd_second"} {
		if result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: id, AgentID: "agt_one", Generation: 1, LauncherID: "ln_docker",
			CheckoutPath: canonicalTempDir(t), Cmd: "true", TimeoutSeconds: 60}); result.Result != "ok" {
			t.Fatalf("start %s: %+v", id, result)
		}
		waitCommandExit(t, sink)
	}
	if probes() != 1 {
		t.Fatalf("the launcher was probed %d times for two commands", probes())
	}
	// An update that drops the feature is noticed at once.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := testenv.WriteExecutable(path, []byte(strings.Replace(string(data), " run_command;", ";", 1)+"\n# updated\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_third", AgentID: "agt_one", Generation: 1, LauncherID: "ln_docker",
		CheckoutPath: canonicalTempDir(t), Cmd: "true", TimeoutSeconds: 60})
	if result.Result != "error" || result.Message != ErrCommandDockerUnsupported.Error() || probes() != 2 {
		t.Fatalf("a replaced launcher must be asked again: %+v, %d probes", result, probes())
	}
}

// A container on an image from before command runs makes the launcher refuse with 126 before anything ran: the job ends refused, with
// the rebuild named, and not with an exit code of the person's command.
func TestDockerCommandRefusedByAnOldImageAsksForARebuild(t *testing.T) {
	c, _, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	path, _ := fakeDockerLauncher(t, []string{"forward_env", "run_command"}, "old-image")
	c.Launchers = []protocol.LauncherRow{dockerLauncherRow(path)}
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_old_image", AgentID: "agt_one", Generation: 1,
		LauncherID: "ln_docker", CheckoutPath: canonicalTempDir(t), Cmd: "exit 3", TimeoutSeconds: 60})
	if result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	exit := waitCommandExit(t, sink)
	if exit.Refused != ErrCommandDockerImageTooOld.Error() || exit.ExitCode != nil {
		t.Fatalf("a refusal by an old image is not the command's exit: %+v", exit)
	}
	if !strings.Contains(exit.Refused, "rebuild the harness image") {
		t.Fatalf("the refusal does not ask for the rebuild: %q", exit.Refused)
	}
}

// A cancel sends the launcher a TERM: 143 is the confirmed stop, 125 says the command may still run in the container.
func TestDockerCommandCancelIsConfirmedOnlyBy143(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		unconfirmed bool
	}{{"run", false}, {"unconfirmed", true}} {
		t.Run(tc.mode, func(t *testing.T) {
			c, j, _ := commandTestClient(t)
			sink := make(chan protocol.CommandExit, 1)
			c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
			path, dir := fakeDockerLauncher(t, []string{"forward_env", "run_command"}, tc.mode)
			c.Launchers = []protocol.LauncherRow{dockerLauncherRow(path)}
			result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_cancel", AgentID: "agt_one", Generation: 1,
				LauncherID: "ln_docker", CheckoutPath: canonicalTempDir(t), Cmd: "sleep 60", TimeoutSeconds: 300})
			if result.Result != "ok" {
				t.Fatalf("start: %+v", result)
			}
			// The launcher traps TERM only once it runs the command (a TERM before that kills it before it started anything).
			deadline := time.Now().Add(20 * time.Second)
			for _, err := os.Stat(filepath.Join(dir, "trapped")); err != nil; _, err = os.Stat(filepath.Join(dir, "trapped")) {
				if time.Now().After(deadline) {
					t.Fatal("the launcher never started the command")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if ack := c.commandCancel("cmd_cancel"); ack.Result != "ok" {
				t.Fatalf("cancel: %+v", ack)
			}
			exit := waitCommandExit(t, sink)
			if !exit.Killed || exit.Unconfirmed != tc.unconfirmed {
				t.Fatalf("exit %+v", exit)
			}
			exits, err := j.PendingCommandExits(t.Context())
			if err != nil || len(exits) != 1 || exits[0].Unconfirmed != tc.unconfirmed {
				t.Fatalf("journaled %+v %v", exits, err)
			}
		})
	}
}

// The launcher's exit is the command's end, even when a host-side process it left behind still holds its output: the end is reported
// within the output grace, with the launcher's exit code.
func TestDockerCommandEndIsNotHeldByALeftoverProcess(t *testing.T) {
	c, _, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	path, dir := fakeDockerLauncher(t, []string{"forward_env", "run_command"}, "leftover")
	c.Launchers = []protocol.LauncherRow{dockerLauncherRow(path)}
	t.Cleanup(func() {
		if data, err := os.ReadFile(filepath.Join(dir, "leftover")); err == nil {
			for _, field := range strings.Fields(string(data)) {
				if pid, err := strconv.Atoi(field); err == nil && pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	})
	for _, tc := range []struct {
		job  string
		cmd  string
		code int
	}{{"cmd_leftover_ok", "echo done", 0}, {"cmd_leftover_fail", "exit 5", 5}} {
		started := time.Now()
		if result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: tc.job, AgentID: "agt_one", Generation: 1, LauncherID: "ln_docker",
			CheckoutPath: canonicalTempDir(t), Cmd: tc.cmd, TimeoutSeconds: 300}); result.Result != "ok" {
			t.Fatalf("start: %+v", result)
		}
		exit := waitCommandExit(t, sink)
		if exit.ExitCode == nil || *exit.ExitCode != tc.code || exit.Lost || exit.Killed {
			t.Fatalf("%s: exit %+v", tc.job, exit)
		}
		if elapsed := time.Since(started); elapsed > dockerOutputGrace+10*time.Second {
			t.Fatalf("%s: the end took %s", tc.job, elapsed)
		}
	}
}

// A Docker command whose start cannot be journaled is ended through the launcher, before the start is reported failed: the launcher gets the
// TERM that ends the command in the container, not only a host SIGKILL that would leave it running there.
func TestDockerCommandUnjournaledStartIsStoppedThroughTheLauncher(t *testing.T) {
	c, j, _ := commandTestClient(t)
	path, dir := fakeDockerLauncher(t, []string{"forward_env", "run_command"}, "run")
	c.Launchers = []protocol.LauncherRow{dockerLauncherRow(path)}
	checkout := canonicalTempDir(t)
	// The job is journaled already, so the start's own record is refused; the command is running in the "container" by then.
	if _, err := j.RecordCommandStart(t.Context(), CommandStartRecord{JobID: "cmd_unjournaled", AgentID: "agt_one", Generation: 1,
		Cmd: "true", LogPath: filepath.Join(checkout, "other.log")}); err != nil {
		t.Fatal(err)
	}
	c.ProcessStart = func(int) string {
		deadline := time.Now().Add(20 * time.Second)
		for _, err := os.Stat(filepath.Join(dir, "inner")); err != nil && time.Now().Before(deadline); _, err = os.Stat(filepath.Join(dir, "inner")) {
			time.Sleep(20 * time.Millisecond)
		}
		return "start"
	}
	result := c.commandStart(t.Context(), protocol.CommandSpec{JobID: "cmd_unjournaled", AgentID: "agt_one", Generation: 1,
		LauncherID: "ln_docker", CheckoutPath: checkout, Cmd: "echo $$ > '" + dir + "/inner.tmp'; mv '" + dir + "/inner.tmp' '" + dir +
			"/inner'; exec sleep 60", TimeoutSeconds: 300})
	if result.Result != "error" || !strings.Contains(result.Message, "already journaled") {
		t.Fatalf("start: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "termed")); err != nil {
		t.Fatal("the launcher was not sent the TERM that ends the command in the container")
	}
	if _, err := os.Stat(filepath.Join(dir, "confirmed")); err != nil {
		t.Fatal("the launcher did not finish ending its command")
	}
	data, err := os.ReadFile(filepath.Join(dir, "inner"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	// Killed, it may stay a zombie for a moment until whoever inherited it reaps it.
	for deadline := time.Now().Add(5 * time.Second); commandProcessAlive(pid) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if commandProcessAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the command in the container still runs (pid %d)", pid)
	}
}

// An agent without a launcher record but on the Docker variant runs its command through that tool's Docker harness, never on the host.
func TestDockerVariantWithoutALauncherRecordUsesItsHarness(t *testing.T) {
	c, _, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	path, dir := fakeDockerLauncher(t, []string{"forward_env", "run_command"}, "run")
	spec := protocol.CommandSpec{JobID: "cmd_variant", AgentID: "agt_one", Generation: 1, Tool: "claude", Variant: "docker",
		CheckoutPath: canonicalTempDir(t), Cmd: "exit 4", TimeoutSeconds: 60}
	if result := c.commandStart(t.Context(), spec); result.Result != "error" || !strings.Contains(result.Message, "Docker harness is unavailable") {
		t.Fatalf("an unavailable Docker harness must refuse, not run on the host: %+v", result)
	}
	c.Harnesses = []protocol.HarnessRow{{HarnessSetting: protocol.HarnessSetting{Tool: "claude", Variant: "docker"}, LauncherPath: path, Available: true}}
	if !c.commandInContainer(spec) {
		t.Fatal("a Docker variant's command is not answered aside")
	}
	spec.JobID = "cmd_variant2"
	if result := c.commandStart(t.Context(), spec); result.Result != "ok" {
		t.Fatalf("start: %+v", result)
	}
	exit := waitCommandExit(t, sink)
	if exit.ExitCode == nil || *exit.ExitCode != 4 {
		t.Fatalf("exit %+v", exit)
	}
	if _, err := os.Stat(filepath.Join(dir, "record")); err != nil {
		t.Fatal("the command did not run through the Docker harness")
	}
}

// A Docker command a restarted supervisor adopts and kills has no launcher exit status to read: without the command's marker, the kill is
// unconfirmed.
func TestAdoptedDockerCommandKillIsUnconfirmed(t *testing.T) {
	c, j, _ := commandTestClient(t)
	sink := make(chan protocol.CommandExit, 1)
	c.commandSink = func(exit protocol.CommandExit) { sink <- exit }
	startPredecessorCommand(t, c, j, "cmd_adopted_docker", 3600, time.Now())
	if _, err := j.db.ExecContext(t.Context(), `UPDATE command_start SET container=1 WHERE job_id='cmd_adopted_docker'`); err != nil {
		t.Fatal(err)
	}
	c.reconcileCommands(t.Context())
	if result := c.commandCancel("cmd_adopted_docker"); result.Result != "ok" {
		t.Fatalf("cancel: %+v", result)
	}
	exit := waitCommandExit(t, sink)
	if !exit.Killed || !exit.Unconfirmed {
		t.Fatalf("exit %+v", exit)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// runSilentDockerTurn starts an OpenCode turn through a fake Docker launcher whose harness says nothing for a minute, under a supervisor
// with a short idle limit. The launcher prints the given features; with turn_activity it prints its exec ID on request and its probe
// answers busy for that ID. It returns the probe log and a channel closed when the turn has ended.
func runSilentDockerTurn(t *testing.T, features string) (probeLog string, stderr *syncBuffer, finished <-chan struct{}) {
	t.Helper()
	root := t.TempDir()
	launcher := filepath.Join(root, "opencode-docker")
	probeLog = filepath.Join(root, "probes")
	source := "#!/bin/sh\n" +
		"if [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then printf '%s\\n' " + features + "; exit 0; fi\n" +
		"if [ -n \"${HARNESS_DOCKER_PROBE+x}\" ]; then\n" +
		"  echo \"$HARNESS_DOCKER_PROBE $HARNESS_DOCKER_SINCE ${AIMEBU_AGENT_KEY-nokey}\" >> '" + probeLog + "'\n" +
		"  [ \"$HARNESS_DOCKER_PROBE\" = ctr1:sess-1 ] || { echo 'Error: no running session for this token.' >&2; exit 1; }\n" +
		"  echo busy; exit 0\n" +
		"fi\n" +
		"[ \"$HARNESS_DOCKER_REPORT_EXEC_ID\" = 1 ] && echo 'HARNESS_DOCKER_EXEC_ID=ctr1:sess-1' >&2\n" +
		"cat > /dev/null\n" +
		"exec sleep 60\n"
	if err := testenv.WriteExecutable(launcher, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	checkout := canonicalTempDir(t)
	a := &opencode.Adapter{Command: launcher, Variant: "docker"}
	stderr = &syncBuffer{}
	spec := adapter.TurnSpec{TurnID: "turn-docker-activity", Agent: adapter.AgentSpec{ID: "agt_test", Tool: "opencode", Variant: "docker",
		Model: "ollama-cloud/gemma4:31b"}, CheckoutPath: checkout, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`),
		AgentTools: &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_test", Command: "/usr/bin/aimebu"},
		StderrSink: stderr}
	turn, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = turn.Cancel(); _, _ = turn.Wait() })

	ctx := context.Background()
	journal, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RecordStart(ctx, StartRecord{TurnID: spec.TurnID, AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient("machine", "key", "http://127.0.0.1:1", journal, adapter.NewFake())
	client.OnError = func(error) {}
	client.TurnLimits = TurnLimitOverrides{Idle: durationPtr(150 * time.Millisecond), Max: durationPtr(0)}
	client.active["agent"] = spec.TurnID
	client.running["agent"] = turn
	client.turns.Add(1)
	go client.runTurn(ctx, protocol.Delivery{AgentID: "agent", TurnID: spec.TurnID, Mode: adapter.ModeNormal, Agent: spec.Agent}, turn)
	done := make(chan struct{})
	go func() { client.turns.Wait(); close(done) }()
	t.Cleanup(func() { _ = turn.Cancel(); <-done; journal.Close() })
	return probeLog, stderr, done
}

// A silent OpenCode turn in Docker is kept past its idle deadline while its launcher's probe says a tool runs in its session, and the
// launcher is handed back the exec ID it printed; the same turn on a launcher without turn_activity dies as idle, as before.
func TestDockerTurnActivityKeepsASilentTurnAlive(t *testing.T) {
	t.Run("turn_activity", func(t *testing.T) {
		probeLog, stderr, finished := runSilentDockerTurn(t, "forward_env stop_on_signal run_command turn_activity")
		select {
		case <-finished:
			t.Fatal("a Docker turn whose launcher says a tool runs was killed as idle")
		case <-time.After(1500 * time.Millisecond):
		}
		data, _ := os.ReadFile(probeLog)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) == 0 || !strings.HasPrefix(lines[0], "ctr1:sess-1 ") || !strings.HasSuffix(lines[0], " nokey") {
			t.Fatalf("the launcher was not handed back the exec ID it printed (and nothing else): %q", data)
		}
		// Asked when the idle deadline fell due, then again at each poll while the answer is busy: every 30 seconds, or every idle limit
		// when that is shorter (150 ms here), never more often.
		if len(lines) < 2 || len(lines) > 12 {
			t.Fatalf("the probe was asked %d times in 1.5s at a 150 ms idle limit", len(lines))
		}
		if strings.Contains(stderr.String(), "HARNESS_DOCKER_EXEC_ID") {
			t.Fatalf("the exec ID line reached the agent's output log: %q", stderr.String())
		}
	})
	t.Run("without turn_activity", func(t *testing.T) {
		probeLog, _, finished := runSilentDockerTurn(t, "forward_env stop_on_signal run_command")
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Fatal("a silent Docker turn without turn_activity was not killed as idle")
		}
		if _, err := os.Stat(probeLog); err == nil {
			t.Fatal("a launcher without turn_activity was probed")
		}
	})
}

// syncBuffer is an output log a turn writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
