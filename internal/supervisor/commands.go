package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

const (
	// commandTailBytes is the tail of a job's output its wake note carries; the whole output stays in the log.
	commandTailBytes = 8 << 10
	// commandLogMaxBytes is what one job's log holds before the rest is dropped (and the note says so).
	commandLogMaxBytes = 10 << 20
	// commandKillGrace is how long a cancelled or timed-out command gets after SIGTERM before it is SIGKILLed.
	commandKillGrace = 8 * time.Second
	// commandWaitPoll is how often a job this supervisor adopted after a restart is checked for its exit marker.
	commandWaitPoll = 2 * time.Second
	// commandTimeLayout is the timestamp format the journal and the protocol share.
	commandTimeLayout = "2006-01-02T15:04:05.000000000Z"
)

// commandRunFeature is the harness-docker capability a Docker launcher must report before a command runs in its container.
const commandRunFeature = adapter.FeatureRunCommand

// Exit statuses of a Docker launcher in command mode (harness-docker's docs/lifecycle.md). 143 follows a TERM only once the launcher has
// confirmed the session in the container is gone; 125 says it could not confirm that. 126 without an exit marker is the launcher refusing
// the command before it ran, because the container still runs an image from before command runs.
const (
	dockerStopConfirmed = 128 + int(syscall.SIGTERM)
	dockerRefusedOld    = 126
)

// ErrCommandDockerImageTooOld is the end of a command a Docker launcher refused because its container runs an image from before command
// runs: nothing of the person's command ran.
var ErrCommandDockerImageTooOld = errors.New("the command never ran: the agent's harness container runs an image from before commands " +
	"outside a turn; rebuild the harness image (`bin/harness-docker-ctrl rebuild` in harness-docker), then retry")

// dockerOutputGrace is how long a Docker launcher's output is still read once it has exited, before what it left behind on the host is cut
// off from the log.
const dockerOutputGrace = 5 * time.Second

// commandFeatureTimeout bounds the feature probe of a Docker launcher: it prints its features before it touches Docker, but Codex's also
// reads the running container's configuration.
const commandFeatureTimeout = 10 * time.Second

// ErrCommandDockerUnsupported is what a supervisor says when the agent runs in Docker and its launcher cannot run a command in the
// container. It names the update, because that is what the person has to do.
var ErrCommandDockerUnsupported = errors.New("a command outside a turn for an agent in Docker needs a Docker launcher with the run_command " +
	"feature: run `HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version`, and it must print `run_command`; update harness-docker, then retry")

// commandJob is one long command this supervisor runs for an agent, outside any turn. Its flags are written by the kill path and read when
// the process ends, so they are guarded.
type commandJob struct {
	mu       sync.Mutex
	spec     protocol.CommandSpec
	pid      int
	pidStart string
	started  time.Time
	logPath  string
	marker   string
	log      *commandLog
	timer    *time.Timer
	killed   bool
	timedOut bool
	// replaced says the kill was this supervisor's own exit while it is being replaced: the end is reported with exit code -1 and the
	// reason supervisor_replaced, so the agent is woken with it and can run the command again.
	replaced bool
	// adopted is a job of a dead predecessor: not this process's child, so it is watched rather than waited for, and its pid is checked
	// against pidStart before it is signalled (the pid may belong to another process by now).
	adopted bool
	// notOurs is set when a kill finds an adopted job's pid owned by another process: its watcher then reports it lost.
	notOurs bool
	// detachable says the command runs without this process (CommandLogger): a session of its own, its own log writer and exit marker.
	// A supervisor that exits leaves it running for its replacement to adopt. detached says this process let go of it: nothing of it is
	// reported or journaled here any more.
	detachable bool
	detached   bool
	// container says the process is a Docker launcher running the command in the agent's container: a kill is confirmed only by the
	// launcher's own exit status (or the command's exit marker), and a launcher that refused the command is not the command's end.
	container bool
}

func (j *commandJob) markKilled(timedOut bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.killed = true
	j.timedOut = j.timedOut || timedOut
}

func (j *commandJob) markDetached() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.detached = true
	if j.timer != nil {
		j.timer.Stop()
	}
}

func (j *commandJob) isDetached() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.detached
}

// leavable says the job does not hold a retiring supervisor: it runs without this process (see detachable), or it is a predecessor's this
// process only watches.
func (j *commandJob) leavable() bool {
	return j.detachable || j.adopted
}

func (j *commandJob) markReplaced() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.replaced = true
}

// settleReplaced marks the end of a command this supervisor killed on its way out while being replaced.
func (j *commandJob) settleReplaced(exit *protocol.CommandExit) {
	j.mu.Lock()
	replaced := j.replaced
	j.mu.Unlock()
	if !replaced || !exit.Killed || exit.TimedOut || exit.Refused != "" {
		return
	}
	code := -1
	exit.ExitCode, exit.Lost, exit.Reason = &code, false, protocol.CommandEndSupervisorReplaced
}

func (j *commandJob) markNotOurs() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.notOurs = true
}

func (j *commandJob) isNotOurs() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.notOurs
}

func (j *commandJob) flags() (killed, timedOut bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.killed, j.timedOut
}

// commandRunner holds the commands this supervisor runs now. It is process state; the journal holds the durable evidence.
type commandRunner struct {
	mu   sync.Mutex
	jobs map[string]*commandJob
}

func newCommandRunner() *commandRunner {
	return &commandRunner{jobs: map[string]*commandJob{}}
}

func (c *Client) hasActiveWork() bool {
	c.mu.Lock()
	active := len(c.active) > 0 || len(c.running) > 0
	runner := c.commands
	c.mu.Unlock()
	if active {
		return true
	}
	if runner != nil {
		runner.mu.Lock()
		active = len(runner.jobs) > 0
		runner.mu.Unlock()
	}
	if active || c.Journal == nil {
		return active
	}
	// A replacement supervisor also watches commands left by its predecessor;
	// those are journaled but are not children in its in-memory runner.
	open, err := c.Journal.OpenCommands(context.Background())
	return err != nil || len(open) > 0
}

// hasRetireBlockingWork is hasActiveWork for a supervisor that retires or is replaced: a turn holds it until its process frame was sent
// (a turn frees its agent's admission just before that send), a command only when it cannot be left to the replacement (its output passes
// through this process). The journal's open commands do not: the replacement adopts them.
func (c *Client) hasRetireBlockingWork() bool {
	c.mu.Lock()
	active := len(c.active) > 0 || len(c.running) > 0 || c.unreported.Load() > 0
	runner := c.commands
	c.mu.Unlock()
	if active || runner == nil {
		return active
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, job := range runner.jobs {
		if !job.leavable() {
			return true
		}
	}
	return false
}

// detachCommands lets go of every command this supervisor can leave to its replacement: its timer stops, its end is no longer reported or
// journaled here, and it stays open in the journal, where the replacement adopts it (reconcileCommands) and reports its real end.
func (c *Client) detachCommands() {
	runner := c.runner()
	runner.mu.Lock()
	var left []string
	for id, job := range runner.jobs {
		if job.leavable() {
			job.markDetached()
			delete(runner.jobs, id)
			left = append(left, id)
		}
	}
	runner.mu.Unlock()
	if len(left) > 0 {
		log.Printf("supervisor exit: %d command(s) left running for the replacement to adopt", len(left))
	}
}

// runner is the client's command table; a client built without NewClient gets one on first use.
func (c *Client) runner() *commandRunner {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commands == nil {
		c.commands = newCommandRunner()
	}
	return c.commands
}

func (r *commandRunner) has(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, found := r.jobs[jobID]
	return found
}

func (r *commandRunner) put(jobID string, job *commandJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[jobID] = job
}

func (r *commandRunner) drop(jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, jobID)
}

// putNew holds a job unless one with its id is already held: an adoption never replaces a command this process runs.
func (r *commandRunner) putNew(jobID string, job *commandJob) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, found := r.jobs[jobID]; found {
		return false
	}
	r.jobs[jobID] = job
	return true
}

// take lets go of exactly this job, and says whether it was still held: only the one that takes it reports its end.
func (r *commandRunner) take(jobID string, job *commandJob) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobs[jobID] != job {
		return false
	}
	delete(r.jobs, jobID)
	return true
}

func (r *commandRunner) get(jobID string) *commandJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[jobID]
}

// forAgent lists the ids of one agent's running commands.
func (r *commandRunner) forAgent(agentID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := []string{}
	for id, job := range r.jobs {
		if job.spec.AgentID == agentID {
			ids = append(ids, id)
		}
	}
	return ids
}

// commandStart starts one long command outside a turn. The result frame says whether it was accepted; the end arrives later as a
// CommandEnded frame.
func (c *Client) commandStart(ctx context.Context, spec protocol.CommandSpec) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.CommandStarted, ReqID: spec.JobID, JobID: spec.JobID,
		AgentID: spec.AgentID, Generation: spec.Generation}
	fail := func(err error) protocol.Frame {
		result.Result, result.Message = "error", err.Error()
		return result
	}
	c.mu.Lock()
	stopping := c.stopping
	c.mu.Unlock()
	if stopping {
		return fail(errors.New("supervisor is retiring"))
	}
	if spec.JobID == "" || !strings.HasPrefix(spec.JobID, "cmd_") || spec.AgentID == "" || spec.Cmd == "" {
		return fail(errors.New("job id, agent and cmd are required"))
	}
	if spec.CheckoutPath == "" || !filepath.IsAbs(spec.CheckoutPath) {
		return fail(errors.New("the agent's checkout is required"))
	}
	if strings.TrimSpace(spec.Cwd) == "" {
		spec.Cwd = "."
	}
	if filepath.IsAbs(spec.Cwd) || strings.Contains(spec.Cwd, "\x00") || !withinDir(spec.CheckoutPath, filepath.Join(spec.CheckoutPath, spec.Cwd)) {
		return fail(errors.New("cwd must stay inside the agent's checkout"))
	}
	spec.Cwd = filepath.Clean(spec.Cwd)
	if spec.TimeoutSeconds < 1 {
		spec.TimeoutSeconds = 3600
	}
	if spec.TimeoutSeconds > 6*3600 {
		spec.TimeoutSeconds = 6 * 3600
	}
	if spec.LogPath == "" {
		spec.LogPath = filepath.Join(spec.CheckoutPath, ".aimebu", "commands", spec.JobID+".log")
	}
	if !withinDir(spec.CheckoutPath, spec.LogPath) {
		return fail(errors.New("the job log must live inside the agent's checkout"))
	}
	// A command runs where the agent's own Bash runs: in its container for a Docker launcher, on this machine for a native one. The
	// container path is the wrapper's to provide, so a launcher that cannot run one is refused with what to update, and never falls back
	// to the host.
	visible := spec.CheckoutPath
	docker, err := c.commandLauncher(spec)
	if err != nil {
		return fail(err)
	}
	if docker != nil {
		probe, cancel := context.WithTimeout(ctx, commandFeatureTimeout)
		features, err := adapter.CachedDockerFeatures(probe, docker.command, docker.args, spec.CheckoutPath, adapter.DockerControlEnv(docker.refs))
		cancel()
		// A probe that fails now falls back to what the launcher said when its row was made; a launcher that cannot run the command still
		// fails it by itself, in the container, never on the host.
		if err != nil && !slices.Contains(docker.features, commandRunFeature) || err == nil && !features[commandRunFeature] {
			return fail(ErrCommandDockerUnsupported)
		}
		visible = docker.visible
	} else if spec.LauncherID != "" {
		if mapped, err := launcherCheckout(c.launcherRow(spec.LauncherID).LauncherDefinition, spec.CheckoutPath); err == nil {
			visible = mapped
		}
	}
	if c.runner().has(spec.JobID) {
		return fail(errors.New("that command is already running here"))
	}
	logger := ""
	if docker == nil && c.CommandLogger != nil {
		if path, err := c.CommandLogger(); err == nil && filepath.IsAbs(path) {
			logger = path
		} else {
			c.reportLocal(fmt.Errorf("job %s will not survive a supervisor swap: its log writer cannot be found: %v", spec.JobID, err))
		}
	}
	detachable := logger != ""
	var log *commandLog
	if detachable {
		err = createCommandLog(spec.LogPath)
	} else {
		log, err = openCommandLog(spec.LogPath)
	}
	if err != nil {
		return fail(fmt.Errorf("the job log could not be opened: %w", err))
	}
	specPath := adapter.TurnSpec{CheckoutPath: spec.CheckoutPath, VisibleCheckoutPath: visible}
	visibleLog, _ := adapter.VisiblePath(specPath, spec.LogPath)
	visibleCwd, _ := adapter.VisiblePath(specPath, filepath.Join(spec.CheckoutPath, spec.Cwd))
	marker := commandExitMarker(spec.LogPath)
	visibleMarker, _ := adapter.VisiblePath(specPath, marker)
	// The wrapper writes its exit code beside the log before it exits: a supervisor that restarts (and cannot wait for a process it did not
	// start) still learns how the command ended.
	var cmd *exec.Cmd
	switch {
	case detachable:
		// The command's output goes to its own log writer, and the exit marker is put in place only after that writer has written the
		// last of it: a supervisor that reads the marker finds the whole log.
		wrapped := `{ ( eval "$3" ); printf %s "$?" > "$1.part"; } 2>&1 | "$4" command-log "$2"; mv -f "$1.part" "$1"; ` +
			`rc=$(cat "$1" 2>/dev/null); exit "${rc:-1}"`
		cmd = exec.Command("sh", "-c", wrapped, "sh", marker, spec.LogPath, spec.Cmd, logger)
		cmd.Dir = filepath.Join(spec.CheckoutPath, spec.Cwd)
	case docker == nil:
		wrapped := `( eval "$2" ); rc=$?; printf %s "$rc" > "$1"; exit $rc`
		cmd = exec.Command("sh", "-c", wrapped, "sh", visibleMarker, spec.Cmd)
		cmd.Dir = filepath.Join(spec.CheckoutPath, spec.Cwd)
	default:
		cmd = dockerCommand(docker, spec, visibleMarker)
		// The launcher's exit is the command's end: a host-side process it left in its group (a `docker exec` client that outlived it) may
		// still hold the log's pipe, and must not hold the end with it.
		cmd.WaitDelay = dockerOutputGrace
	}
	if detachable {
		// A session of its own: the command is no part of this supervisor's, and outlives it.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	} else {
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		if log != nil {
			log.CloseTail()
		}
		return fail(fmt.Errorf("the command could not be started: %w", err))
	}
	job := &commandJob{spec: spec, pid: cmd.Process.Pid, started: time.Now(), logPath: spec.LogPath, marker: marker, log: log,
		container: docker != nil, detachable: detachable}
	// The start time tells this process from a later one that reuses its pid, for a supervisor that adopts the job after a restart.
	job.pidStart = c.identityOf(job.pid)
	already, err := c.Journal.RecordCommandStart(ctx, CommandStartRecord{JobID: spec.JobID, AgentID: spec.AgentID,
		Generation: spec.Generation, Pid: job.pid, PidStart: job.pidStart, Cmd: spec.Cmd, LogPath: spec.LogPath,
		StartedAt: job.started.UTC().Format(commandTimeLayout), TimeoutSeconds: spec.TimeoutSeconds, Container: job.container})
	if err != nil || already {
		// No command runs without its journal record: kill it now rather than leave one this machine cannot account for.
		stopUnjournaled(job, cmd)
		if log != nil {
			log.CloseTail()
		}
		if err != nil {
			return fail(fmt.Errorf("the command's journal record could not be written: %w", err))
		}
		return fail(errors.New("that command is already journaled"))
	}
	c.runner().put(spec.JobID, job)
	job.timer = time.AfterFunc(time.Duration(spec.TimeoutSeconds)*time.Second, func() {
		c.commandKill(spec.JobID, true)
	})
	go func() {
		defer telemetry.Recover(ctx)
		c.waitCommand(ctx, job, cmd)
	}()
	result.Result = "ok"
	result.LogPath, result.VisibleLogPath, result.VisibleCwd = spec.LogPath, visibleLog, visibleCwd
	return result
}

// waitCommand reaps one command, journals how it ended and reports it to the server.
func (c *Client) waitCommand(ctx context.Context, job *commandJob, cmd *exec.Cmd) {
	err := cmd.Wait()
	if job.isDetached() {
		return // left to the replacement, which reports its end from the exit marker
	}
	killed, timedOut := job.flags()
	exit := protocol.CommandExit{JobID: job.spec.JobID, AgentID: job.spec.AgentID, Generation: job.spec.Generation,
		StartedAt: job.started.UTC().Format(commandTimeLayout), Killed: killed, TimedOut: timedOut}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		code := 0
		exit.ExitCode = &code
	case errors.As(err, &exitErr):
		code := exitErr.ExitCode()
		exit.ExitCode = &code
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		// The process ended, but something it left behind still held its output (a Docker launcher's WaitDelay): its status stands.
		code := cmd.ProcessState.ExitCode()
		exit.ExitCode = &code
	default:
		exit.Lost = true
	}
	if job.container {
		settleContainerExit(job, &exit)
	}
	job.settleReplaced(&exit)
	c.finishCommand(ctx, job, exit)
}

// settleContainerExit reads a Docker launcher's exit status for what it says about the command in the container. A kill is confirmed only
// by 143 (the launcher ended the session and saw it gone) or by the command's own exit marker; anything else (125, a SIGKILL after the
// grace, a launcher killed before it could trap the signal) may have left the command running. 126 with no marker is the launcher refusing
// the command before it ran, not the command's exit code.
func settleContainerExit(job *commandJob, exit *protocol.CommandExit) {
	_, marked := readExitMarker(job.marker)
	if exit.Killed {
		if !marked && (exit.ExitCode == nil || *exit.ExitCode != dockerStopConfirmed) {
			exit.Unconfirmed = true
		}
		return
	}
	if !marked && exit.ExitCode != nil && *exit.ExitCode == dockerRefusedOld {
		exit.ExitCode = nil
		exit.Refused = ErrCommandDockerImageTooOld.Error()
	}
}

// finishCommand writes the end of one command this process started and reports it. The end is journaled before the job is let go, so a
// reconnect in between finds it either held or ended, never open and unheld (which would adopt it a second time).
func (c *Client) finishCommand(ctx context.Context, job *commandJob, exit protocol.CommandExit) {
	exit.EndedAt = time.Now().UTC().Format(commandTimeLayout)
	if job.log != nil {
		tail, written, truncated := job.log.CloseTail()
		exit.LogBytes, exit.TailTruncated, exit.Tail = written, truncated, adapter.RedactSecrets(tail)
	} else if tail, size, _, err := tailFile(job.logPath, commandTailBytes); err == nil {
		// Its own log writer wrote the log: the tail is read from the file, and the log was cut when it reached the cap.
		exit.LogBytes, exit.TailTruncated, exit.Tail = size, size >= commandLogMaxBytes, adapter.RedactSecrets(tail)
	}
	if job.timer != nil {
		job.timer.Stop()
	}
	if err := c.Journal.RecordCommandExit(context.WithoutCancel(ctx), exit); err != nil {
		c.report("a command's end could not be journaled", fmt.Errorf("job %s: %w", exit.JobID, err))
	}
	c.runner().take(exit.JobID, job)
	c.reportCommand(exit)
}

// recordCommandEnd journals one end and sends it; the journal keeps it until the server acknowledges it.
func (c *Client) recordCommandEnd(ctx context.Context, exit protocol.CommandExit) {
	if err := c.Journal.RecordCommandExit(ctx, exit); err != nil {
		c.report("a command's end could not be journaled", fmt.Errorf("job %s: %w", exit.JobID, err))
	}
	c.reportCommand(exit)
}

// reportCommand sends one command's end to the server.
func (c *Client) reportCommand(exit protocol.CommandExit) {
	if c.commandSink != nil {
		c.commandSink(exit)
		return
	}
	if err := c.sendCurrent(context.Background(), protocol.Frame{V: protocol.Version, Type: protocol.CommandEnded,
		AgentID: exit.AgentID, Generation: exit.Generation, JobID: exit.JobID, CommandExit: &exit}); err != nil {
		c.report("a command's end could not be sent", fmt.Errorf("job %s: %w", exit.JobID, err))
	}
}

// commandKill ends one running command: SIGTERM for its process group, SIGKILL after a grace. A timeout is a kill too, and says so. An
// adopted job whose pid now belongs to another process is not signalled: its watcher reports it lost.
func (c *Client) commandKill(jobID string, timedOut bool) {
	job := c.runner().get(jobID)
	if job == nil {
		return
	}
	if job.adopted && !c.sameCommandProcess(job) {
		job.markNotOurs()
		return
	}
	job.markKilled(timedOut)
	notReaped := func() bool { return false }
	if job.container {
		// The launcher alone gets the TERM: it ends the session in the container and exits 143 only once that is confirmed. Its own
		// `docker exec` client must stay up meanwhile; the SIGKILL below, after the grace, takes the whole group.
		_ = syscall.Kill(job.pid, syscall.SIGTERM)
	} else {
		_ = adapter.SignalGroup(job.pid, syscall.SIGTERM, notReaped)
	}
	deadline := time.Now().Add(commandKillGrace)
	for commandProcessAlive(job.pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if commandProcessAlive(job.pid) {
		_ = adapter.SignalGroup(job.pid, syscall.SIGKILL, notReaped)
	}
}

// commandReplace ends one running command because this supervisor is being replaced: the kill of commandKill, with an end that says so
// (exit code -1, reason supervisor_replaced).
func (c *Client) commandReplace(jobID string) {
	if job := c.runner().get(jobID); job != nil {
		job.markReplaced()
	}
	c.commandKill(jobID, false)
}

// stopUnjournaled ends a command whose start could not be journaled, before the start is reported failed. A native one is killed with its
// group. A Docker launcher may already have started the command in the container, out of reach of a host signal: it gets the TERM alone
// first, as on a cancel, so it ends the session there, and its group is killed only after the grace.
func stopUnjournaled(job *commandJob, cmd *exec.Cmd) {
	notReaped := func() bool { return false }
	if !job.container {
		_ = adapter.SignalGroup(job.pid, syscall.SIGKILL, notReaped)
		_ = cmd.Wait()
		return
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	_ = syscall.Kill(job.pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(commandKillGrace):
		_ = adapter.SignalGroup(job.pid, syscall.SIGKILL, notReaped)
		<-done
	}
}

// commandCancel answers a CommandCancel frame: the process is signalled, and its end arrives as a CommandEnded frame like any other.
func (c *Client) commandCancel(jobID string) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.CommandCancelResult, ReqID: jobID, JobID: jobID}
	if !c.runner().has(jobID) {
		result.Result, result.Message = "error", "no such command is running here"
		return result
	}
	go func() {
		defer telemetry.Recover(context.Background())
		c.commandKill(jobID, false)
	}()
	result.Result = "ok"
	return result
}

// commandLogTail answers a CommandLogRead frame with the end of one job's log, bounded.
func (c *Client) commandLogTail(reqID, logPath string, maxBytes int) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.CommandLogResult, ReqID: reqID, LogPath: logPath}
	if maxBytes < 1 || maxBytes > 1<<20 {
		maxBytes = 64 << 10
	}
	data, _, truncated, err := tailFile(logPath, maxBytes)
	if err != nil {
		result.Result, result.Message = "error", err.Error()
		return result
	}
	result.Result = "ok"
	result.Log, result.LogTruncated = adapter.RedactSecrets(data), truncated
	return result
}

// reconcileCommands adopts the commands the journal still lists as open and this process does not run: those of a dead predecessor. It
// runs with every connection, so a command this process runs (or one it already adopted) is left alone. An adopted job is held like any
// other, so a cancel, a stop of its agent and its timeout reach it; it is watched rather than waited for (it is not this process's child).
// One whose exit marker says how it ended is reported with that exit code; one whose process is gone, or whose pid now belongs to another
// process (its start time differs from the journaled one), and left no marker is reported lost.
func (c *Client) reconcileCommands(ctx context.Context) {
	open, err := c.Journal.OpenCommandStarts(ctx)
	if err != nil {
		c.report("the journal's open commands could not be read", err)
		return
	}
	for _, item := range open {
		if c.runner().has(item.JobID) {
			continue
		}
		job := &commandJob{spec: protocol.CommandSpec{JobID: item.JobID, AgentID: item.AgentID, Generation: item.Generation,
			TimeoutSeconds: item.TimeoutSeconds}, pid: item.Pid, pidStart: item.PidStart, marker: commandExitMarker(item.LogPath),
			logPath: item.LogPath, adopted: true, container: item.Container}
		if started, err := time.Parse(commandTimeLayout, item.StartedAt); err == nil {
			job.started = started
		} else {
			job.started = time.Now()
		}
		// Its end may have been journaled since the list was read (this process's own command ending just now): nothing to adopt.
		if ended, err := c.Journal.HasCommandExit(ctx, item.JobID); err != nil || ended {
			continue
		}
		if !c.runner().putNew(item.JobID, job) {
			continue
		}
		timeout := time.Duration(item.TimeoutSeconds) * time.Second
		if item.TimeoutSeconds <= 0 {
			timeout = 6 * time.Hour // a record from before timeouts were journaled: the longest any job may ask for
		}
		jobID := item.JobID
		job.mu.Lock()
		job.timer = time.AfterFunc(max(time.Until(job.started.Add(timeout)), 0), func() { c.commandKill(jobID, true) })
		job.mu.Unlock()
		go func() {
			defer telemetry.Recover(ctx)
			c.watchAdoptedCommand(ctx, job)
		}()
	}
}

// sameCommandProcess says whether an adopted job's pid is still its own process: alive, and started when the journal says it was. A
// record without a start time (from a supervisor that did not keep one) is taken at its pid.
func (c *Client) sameCommandProcess(job *commandJob) bool {
	if !commandProcessAlive(job.pid) {
		return false
	}
	return job.pidStart == "" || c.identityOf(job.pid) == job.pidStart
}

func (c *Client) watchAdoptedCommand(ctx context.Context, job *commandJob) {
	// The pid is checked against its start time once, here: after that, the process is polled for being alive, and its exit marker read.
	alive := c.sameCommandProcess(job)
	for {
		if code, found := readExitMarker(job.marker); found {
			exit := protocol.CommandExit{JobID: job.spec.JobID, AgentID: job.spec.AgentID, Generation: job.spec.Generation}
			if code >= 0 {
				value := code
				exit.ExitCode = &value
			} else {
				exit.Lost = true
			}
			c.finishAdoptedCommand(ctx, job, exit)
			return
		}
		if job.isDetached() {
			return // this supervisor exits: its replacement watches the command
		}
		if !alive || job.isNotOurs() || !commandProcessAlive(job.pid) {
			c.finishAdoptedCommand(ctx, job, protocol.CommandExit{JobID: job.spec.JobID, AgentID: job.spec.AgentID,
				Generation: job.spec.Generation, Lost: true})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(commandWaitPoll):
		}
	}
}

// finishAdoptedCommand ends a job this supervisor found running after a restart. Its log is the file the previous one wrote; its tail is
// read from there, and the end is reported like any other. A job this supervisor killed (a cancel, a stop, its timeout) says so, as the
// marker of a killed wrapper is never written. Only the holder of the job reports it, once.
func (c *Client) finishAdoptedCommand(ctx context.Context, job *commandJob, exit protocol.CommandExit) {
	job.mu.Lock()
	if job.timer != nil {
		job.timer.Stop()
	}
	job.mu.Unlock()
	if killed, timedOut := job.flags(); killed {
		exit.Killed, exit.TimedOut = true, timedOut
		if exit.ExitCode == nil {
			exit.Lost = false
			// A launcher this process did not start leaves no exit status to read: without the command's marker, nothing confirms
			// that the command in the container is gone.
			exit.Unconfirmed = job.container
		}
	}
	if exit.Lost && exit.ExitCode == nil {
		// Its process went without leaving an exit code: it died with a supervisor that was replaced (or crashed).
		code := -1
		exit.ExitCode, exit.Reason = &code, protocol.CommandEndLostInSwap
	}
	job.settleReplaced(&exit)
	if tail, written, truncated, err := tailFile(job.logPath, commandTailBytes); err == nil {
		exit.LogBytes, exit.TailTruncated, exit.Tail = written, truncated, adapter.RedactSecrets(tail)
	}
	exit.StartedAt = job.started.UTC().Format(commandTimeLayout)
	if !c.runner().take(exit.JobID, job) {
		return
	}
	if ended, err := c.Journal.HasCommandExit(ctx, exit.JobID); err == nil && ended {
		return // already ended and journaled by the process that ran it
	}
	c.recordCommandEnd(context.WithoutCancel(ctx), exit)
}

// killAgentCommands ends the commands one agent has running: a stopped agent has nothing of its own left on the machine. Each kill waits up
// to its grace for the process group to go, so it runs aside from the caller (the frame loop).
func (c *Client) killAgentCommands(agentID string) {
	for _, jobID := range c.runner().forAgent(agentID) {
		go func() {
			defer telemetry.Recover(context.Background())
			c.commandKill(jobID, false)
		}()
	}
}

// commandExitMarker is the file a command writes its exit code to before it exits: the proof of an end for a supervisor that cannot wait
// for the process.
func commandExitMarker(logPath string) string { return logPath + ".exit" }

func readExitMarker(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1, true
	}
	return code, true
}

func commandProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// commandDocker is the Docker launcher a command runs through: the agent's own, as its turns run it.
type commandDocker struct {
	command string
	args    []string
	refs    map[string]string
	// features is what the launcher said when its row was made: the fallback when the probe cannot be asked now.
	features []string
	// visible is the checkout as the container sees it.
	visible string
}

// commandLauncher says how one agent's command runs: nil for this machine (a native launcher, or an agent with none), the Docker launcher
// otherwise. An agent whose launcher is not here, or whose Docker harness is unavailable, is refused: it never falls back to the host.
func (c *Client) commandLauncher(spec protocol.CommandSpec) (*commandDocker, error) {
	if spec.LauncherID != "" {
		row := c.launcherRow(spec.LauncherID)
		if row.ID == "" {
			return nil, errors.New("the agent's launcher is not configured on this machine")
		}
		if !row.Containerized {
			return nil, nil
		}
		if row.ResolvedCommand == "" {
			reason := row.UnavailableReason
			if reason == "" {
				reason = "its command was not found"
			}
			return nil, fmt.Errorf("the agent's Docker launcher is unavailable on this machine: %s", reason)
		}
		visible, err := launcherCheckout(row.LauncherDefinition, spec.CheckoutPath)
		if err != nil {
			return nil, err
		}
		return &commandDocker{command: row.ResolvedCommand, args: append([]string(nil), row.Args...), refs: row.EnvRefs,
			features: row.Features, visible: visible}, nil
	}
	if spec.Variant != "docker" {
		return nil, nil
	}
	c.mu.Lock()
	row := c.harnessRow(harnessKey(spec.Tool, "docker"))
	c.mu.Unlock()
	if !row.Available {
		reason := row.UnavailableReason
		if reason == "" {
			reason = "this machine does not list it"
		}
		return nil, fmt.Errorf("the agent's Docker harness is unavailable on this machine: %s", reason)
	}
	command := row.LauncherPath
	if command == "" {
		command = spec.Tool + "-docker"
	}
	return &commandDocker{command: command, visible: spec.CheckoutPath}, nil
}

// commandInContainer says whether a command would run through a Docker launcher, whose feature probe may take a while: its start is then
// answered aside from the frame loop.
func (c *Client) commandInContainer(spec protocol.CommandSpec) bool {
	if spec.LauncherID != "" {
		return c.launcherRow(spec.LauncherID).Containerized
	}
	return spec.Variant == "docker"
}

// dockerCommand is the launcher in command mode (docs/design/harness-docker-run-command.md): the command line goes in
// HARNESS_DOCKER_RUN_COMMAND, by name only, and like a native one it writes its exit code to the marker before it exits. The working
// directory is relative to the checkout, from which the launcher is run. Nothing is forwarded into the container: no agent key, no
// toolbox.
func dockerCommand(docker *commandDocker, spec protocol.CommandSpec, marker string) *exec.Cmd {
	line := "aimebu_exit_marker=" + shellQuote(marker) + "; ( eval " + shellQuote(spec.Cmd) + ` ); rc=$?; printf %s "$rc" > "$aimebu_exit_marker"; exit $rc`
	cwd := spec.Cwd
	if cwd == "." {
		cwd = ""
	}
	cmd := exec.Command(docker.command, docker.args...)
	cmd.Dir = spec.CheckoutPath
	cmd.Env = append(adapter.DockerControlEnv(docker.refs), "HARNESS_DOCKER_RUN_COMMAND="+line, "HARNESS_DOCKER_CWD="+cwd,
		"HARNESS_DOCKER_EXIT_MARKER="+marker, "HARNESS_DOCKER_TIMEOUT_SECONDS="+strconv.Itoa(spec.TimeoutSeconds))
	return cmd
}

// shellQuote makes one word of a value for sh: single quotes, with each quote in it closed, escaped and reopened.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
