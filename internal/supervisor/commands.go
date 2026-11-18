package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/telemetry"
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
const commandRunFeature = "run_command"

// ErrCommandDockerUnsupported is what a supervisor says when the agent runs in Docker and its launcher cannot run a command in the
// container. It names the update, because that is what the person has to do.
var ErrCommandDockerUnsupported = errors.New("a command outside a turn for an agent in Docker needs a Docker launcher with the run_command " +
	"feature: run `HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version`, and it must print `run_command`; update harness-docker, then retry")

// ErrCommandContainerUnsupported is what a supervisor says when the launcher has the feature but this aimebu does not run container
// commands yet (the Docker side lands with the wrapper support).
var ErrCommandContainerUnsupported = errors.New("this aimebu build runs commands outside a turn for agents on a native launcher only: " +
	"the Docker side is not in this build")

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
}

func (j *commandJob) markKilled(timedOut bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.killed = true
	j.timedOut = j.timedOut || timedOut
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
	if spec.JobID == "" || !strings.HasPrefix(spec.JobID, "cmd_") || spec.AgentID == "" || spec.Cmd == "" {
		return fail(errors.New("job id, agent and cmd are required"))
	}
	if spec.CheckoutPath == "" || !filepath.IsAbs(spec.CheckoutPath) {
		return fail(errors.New("the agent's checkout is required"))
	}
	if strings.TrimSpace(spec.Cwd) == "" {
		spec.Cwd = "."
	}
	if filepath.IsAbs(spec.Cwd) || strings.Contains(spec.Cwd, "\x00") || strings.HasPrefix(spec.Cwd, "../") || strings.Contains(spec.Cwd, "/../") {
		return fail(errors.New("cwd must stay inside the agent's checkout"))
	}
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
	// container path is the wrapper's to provide, so a launcher that cannot run one is refused with what to update.
	visible := spec.CheckoutPath
	if spec.LauncherID != "" {
		row := c.launcherRow(spec.LauncherID)
		if row.Containerized {
			if !slices.Contains(row.Features, commandRunFeature) {
				return fail(ErrCommandDockerUnsupported)
			}
			return fail(ErrCommandContainerUnsupported)
		}
		if mapped, err := launcherCheckout(row.LauncherDefinition, spec.CheckoutPath); err == nil {
			visible = mapped
		}
	}
	if c.runner().has(spec.JobID) {
		return fail(errors.New("that command is already running here"))
	}
	log, err := openCommandLog(spec.LogPath)
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
	wrapped := `( eval "$2" ); rc=$?; printf %s "$rc" > "$1"; exit $rc`
	cmd := exec.Command("sh", "-c", wrapped, "sh", visibleMarker, spec.Cmd)
	cmd.Dir = filepath.Join(spec.CheckoutPath, spec.Cwd)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.CloseTail()
		return fail(fmt.Errorf("the command could not be started: %w", err))
	}
	job := &commandJob{spec: spec, pid: cmd.Process.Pid, started: time.Now(), logPath: spec.LogPath, marker: marker, log: log}
	if c.ProcessStart != nil {
		job.pidStart = c.ProcessStart(job.pid)
	}
	already, err := c.Journal.RecordCommandStart(ctx, CommandStartRecord{JobID: spec.JobID, AgentID: spec.AgentID,
		Generation: spec.Generation, Pid: job.pid, PidStart: job.pidStart, Cmd: spec.Cmd, LogPath: spec.LogPath,
		StartedAt: job.started.UTC().Format(commandTimeLayout)})
	if err != nil || already {
		// No command runs without its journal record: kill it now rather than leave one this machine cannot account for.
		_ = adapter.SignalGroup(job.pid, syscall.SIGKILL, func() bool { return false })
		_ = cmd.Wait()
		log.CloseTail()
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
	default:
		exit.Lost = true
	}
	c.finishCommand(ctx, job, exit)
}

// finishCommand writes the end of one command and reports it. A job adopted after a restart calls it with the exit code its marker holds.
func (c *Client) finishCommand(ctx context.Context, job *commandJob, exit protocol.CommandExit) {
	exit.EndedAt = time.Now().UTC().Format(commandTimeLayout)
	tail, written, truncated := job.log.CloseTail()
	exit.LogBytes, exit.TailTruncated, exit.Tail = written, truncated, adapter.RedactSecrets(tail)
	if job.timer != nil {
		job.timer.Stop()
	}
	c.runner().drop(exit.JobID)
	c.recordCommandEnd(ctx, exit)
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

// commandKill ends one running command: SIGTERM for its process group, SIGKILL after a grace. A timeout is a kill too, and says so.
func (c *Client) commandKill(jobID string, timedOut bool) {
	job := c.runner().get(jobID)
	if job == nil {
		return
	}
	job.markKilled(timedOut)
	notReaped := func() bool { return false }
	_ = adapter.SignalGroup(job.pid, syscall.SIGTERM, notReaped)
	deadline := time.Now().Add(commandKillGrace)
	for commandProcessAlive(job.pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if commandProcessAlive(job.pid) {
		_ = adapter.SignalGroup(job.pid, syscall.SIGKILL, notReaped)
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

// reconcileCommands is what a supervisor does with the commands of a dead predecessor at startup: one whose exit marker says how it ended
// is reported with that exit code, one whose process is gone and left no marker is reported lost, and one still running is watched until its
// marker appears (the process is not this supervisor's child, so it cannot be waited for).
func (c *Client) reconcileCommands(ctx context.Context) {
	open, err := c.Journal.OpenCommands(ctx)
	if err != nil {
		c.report("the journal's open commands could not be read", err)
		return
	}
	for _, item := range open {
		job := &commandJob{spec: protocol.CommandSpec{JobID: item.JobID, AgentID: item.AgentID, Generation: item.Generation},
			pid: item.Pid, pidStart: item.PidStart, marker: commandExitMarker(item.LogPath), logPath: item.LogPath}
		if started, err := time.Parse(commandTimeLayout, item.StartedAt); err == nil {
			job.started = started
		}
		go func() {
			defer telemetry.Recover(ctx)
			c.watchAdoptedCommand(ctx, job)
		}()
	}
}

func (c *Client) watchAdoptedCommand(ctx context.Context, job *commandJob) {
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
		if !commandProcessAlive(job.pid) {
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
// read from there, and the end is reported like any other.
func (c *Client) finishAdoptedCommand(ctx context.Context, job *commandJob, exit protocol.CommandExit) {
	if tail, written, truncated, err := tailFile(job.logPath, commandTailBytes); err == nil {
		exit.LogBytes, exit.TailTruncated, exit.Tail = written, truncated, adapter.RedactSecrets(tail)
	}
	c.recordCommandEnd(ctx, exit)
}

// killAgentCommands ends the commands one agent has running: a stopped agent has nothing of its own left on the machine.
func (c *Client) killAgentCommands(agentID string) {
	for _, jobID := range c.runner().forAgent(agentID) {
		c.commandKill(jobID, false)
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
