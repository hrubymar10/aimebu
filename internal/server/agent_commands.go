package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// errCommandUnsupported is a machine whose supervisor predates commands outside a turn.
var errCommandUnsupported = errors.New("commands outside a turn need an updated supervisor on that machine (it does not report supports_commands)")

// errContainerCommandUnsupported is a machine whose supervisor runs commands, but not in an agent's container: it would run a docker-variant
// agent's command on the host, so the command is refused instead.
var errContainerCommandUnsupported = errors.New("this agent runs in Docker, and the supervisor on its machine cannot run a command in its " +
	"container (it does not report supports_container_commands): update aimebu on that machine, then retry")

// commandTimeLayout matches the store's timestamps.
const commandTimeLayout = "2006-01-02T15:04:05.000000000Z"

type commandRunRequest struct {
	Cmd            string `json:"cmd"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Label          string `json:"label"`
	RoomID         string `json:"room_id"`
}

// agentToolCommandRun starts one long command outside the caller's turn. It runs under the supervisor in the agent's own checkout, and its
// end wakes the agent with a note in the room the call was made from.
func (s *Server) agentToolCommandRun(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required")
		return
	}
	var req commandRunRequest
	if !readJSON(w, r, &req) {
		return
	}
	agent, err := s.Store.AgentRuntimeByID(r.Context(), caller.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if agent.Desired != "active" {
		apiError(w, 403, "forbidden", "only an active agent may start commands")
		return
	}
	roomID := req.RoomID
	if roomID == "" {
		// A tool call comes from a turn, and one agent has at most one: its room is where the command was started from.
		roomID, err = s.Store.RunningTurnRoom(r.Context(), caller.ID)
		if err != nil {
			apiError(w, 400, "invalid_request", "no turn is running: name room_id")
			return
		}
	}
	joined, err := s.Store.AgentInRoom(r.Context(), caller.ID, roomID)
	if err != nil {
		fail(w, err)
		return
	}
	if !joined {
		apiError(w, 403, "forbidden", "you are not a member of that room: run commands from a room of your own project or one of your project's chats")
		return
	}
	if err := s.Store.CommandRoomScope(r.Context(), caller.ProjectID, roomID); err != nil {
		commandError(w, err)
		return
	}
	checkout, err := s.Store.AgentCheckoutPath(r.Context(), caller.ID)
	if err != nil {
		fail(w, err)
		return
	}
	conn, err := s.machineForDispatch(agent.MachineID)
	if err != nil {
		apiError(w, 503, "machine_unavailable", err.Error())
		return
	}
	job := store.CommandJob{AgentID: caller.ID, Generation: agent.Generation, MachineID: agent.MachineID, JournalID: conn.journalID,
		ProjectID: caller.ProjectID, RoomID: roomID, Label: req.Label, Cmd: req.Cmd, Cwd: req.Cwd,
		TimeoutSeconds: req.TimeoutSeconds, CheckoutPath: checkout}
	raw, _ := json.Marshal(job)
	digest := sha256.Sum256(raw)
	created, reused, err := s.Store.CreateCommandJob(r.Context(), job, store.RequestKey{ActorKind: "agent",
		ActorID: caller.ID, Kind: "command_run", Key: key, Digest: hex.EncodeToString(digest[:])})
	if err != nil {
		commandError(w, err)
		return
	}
	if reused {
		// A retry sees the job as it is now, not as it was when the key was first used.
		if current, err := s.Store.CommandJobByAnyAgent(r.Context(), created.ID); err == nil {
			created = current
		}
		writeJSON(w, 200, created)
		return
	}
	result, err := s.machineCommand(r.Context(), agent.MachineID, protocol.Frame{V: protocol.Version,
		Type: protocol.CommandRun, ReqID: created.ID, JobID: created.ID, AgentID: caller.ID, Generation: agent.Generation,
		Command: &protocol.CommandSpec{JobID: created.ID, AgentID: caller.ID, Generation: agent.Generation,
			LauncherID: agent.LauncherID, Tool: agent.Tool, Variant: agent.Variant, CheckoutPath: checkout, Cmd: created.Cmd, Cwd: created.Cwd,
			TimeoutSeconds: created.TimeoutSeconds, LogPath: created.LogPath}}, 30*time.Second)
	// The request was sent, but no answer came (a timeout, a disconnect, the caller gone): that does not prove the machine refused it, so the
	// job stays queued. Its late start, its end, or the machine's next hello settles it, and its end still wakes the agent.
	ctx := context.WithoutCancel(r.Context())
	var unanswered commandUnansweredError
	if errors.As(err, &unanswered) {
		current, err := s.Store.CommandJobByAnyAgent(ctx, created.ID)
		if err != nil {
			fail(w, err)
			return
		}
		if current.State != "queued" {
			writeJSON(w, 201, current)
			return
		}
		writeJSON(w, 202, commandUnconfirmed{CommandJob: current, StartUnconfirmed: true,
			Warning: "the machine has not confirmed the start (" + unanswered.Error() + "); it may still be running. Do not start it again: " +
				"its end wakes you as usual, and list_commands shows the job once the machine reports it"})
		return
	}
	if err != nil || result.Result != "ok" {
		message := "the machine did not start the command"
		if err != nil {
			message = err.Error()
		} else if result.Message != "" {
			message = result.Message
		}
		_, _ = s.Store.FailCommandStart(ctx, created.ID, message)
		apiError(w, 409, "command_refused", message)
		return
	}
	// The machine reader has already recorded the start (and a fast command's end may have come first): this only returns the row.
	started, err := s.Store.RecordCommandStarted(ctx, created.ID, created.JournalID, time.Now().UTC().Format(commandTimeLayout),
		result.VisibleLogPath, result.VisibleCwd)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, started)
}

// commandUnconfirmed is run_command's answer when the machine was asked but did not answer: the job, and that its start is unknown.
type commandUnconfirmed struct {
	store.CommandJob
	StartUnconfirmed bool   `json:"start_unconfirmed"`
	Warning          string `json:"warning"`
}

// commandUnansweredError is a machine request that was sent but got no answer: unlike a refusal, the machine may have acted on it.
type commandUnansweredError struct{ err error }

func (e commandUnansweredError) Error() string { return e.err.Error() }
func (e commandUnansweredError) Unwrap() error { return e.err }

// commandOwned says whether a job is this machine's and this agent's: a command frame is authorized by the job it reports on, not by the
// agent's current generation (a context clear or a harness switch moves that on while the command keeps running).
func (s *Server) commandOwned(ctx context.Context, conn *machineConn, jobID, agentID string) (store.CommandJob, error) {
	job, err := s.Store.CommandJobByAnyAgent(ctx, jobID)
	if err != nil {
		return store.CommandJob{}, err
	}
	if job.MachineID != conn.id || job.JournalID != "" && job.JournalID != conn.journalID || agentID == "" || job.AgentID != agentID {
		return store.CommandJob{}, store.ErrForbidden
	}
	return job, nil
}

// applyCommandStarted records a machine's answer to a CommandRun as it arrives, whether or not the call that asked still waits for it: a
// late start still makes the job running, and a refusal fails it. A job cancelled before its start was confirmed is killed now.
func (s *Server) applyCommandStarted(ctx context.Context, conn *machineConn, frame protocol.Frame) {
	s.maintenanceMu.RLock()
	defer s.maintenanceMu.RUnlock()
	if _, err := s.commandOwned(ctx, conn, frame.JobID, frame.AgentID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			telemetry.ReportWarning(ctx, "machine", fmt.Errorf("command start report refused: %w", err))
		}
		return
	}
	if frame.Result != "ok" {
		message := frame.Message
		if message == "" {
			message = "the machine did not start the command"
		}
		if _, err := s.Store.FailCommandStart(ctx, frame.JobID, message); err != nil && !errors.Is(err, sql.ErrNoRows) {
			telemetry.ReportError(ctx, "machine", fmt.Errorf("recording a refused command: %w", err))
		}
		return
	}
	job, err := s.Store.RecordCommandStarted(ctx, frame.JobID, conn.journalID, time.Now().UTC().Format(commandTimeLayout), frame.VisibleLogPath, frame.VisibleCwd)
	if err != nil {
		telemetry.ReportError(ctx, "machine", fmt.Errorf("recording a command's start: %w", err))
		return
	}
	if job.State == "cancelled" {
		_ = conn.send(protocol.Frame{V: protocol.Version, Type: protocol.CommandCancel, ReqID: newRequestNonce(), JobID: job.ID, AgentID: job.AgentID})
	}
}

// agentToolCommands lists the caller's own jobs.
func (s *Server) agentToolCommands(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	jobs, err := s.Store.CommandJobs(r.Context(), caller.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"commands": jobs})
}

// agentToolCommandLog returns a bounded tail of one job's log, read on the machine that ran it.
func (s *Server) agentToolCommandLog(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	job, ok := s.ownedCommand(w, r, caller)
	if !ok {
		return
	}
	maxBytes := 64 << 10
	if raw := r.URL.Query().Get("max_bytes"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1<<20 {
			apiError(w, 400, "invalid_request", "max_bytes must be between 1 and 1048576")
			return
		}
		maxBytes = value
	}
	result, err := s.machineCommand(r.Context(), job.MachineID, protocol.Frame{V: protocol.Version,
		Type: protocol.CommandLogRead, ReqID: newRequestNonce(), JobID: job.ID, LogPath: job.LogPath, LogMaxBytes: maxBytes}, 30*time.Second)
	if err != nil {
		apiError(w, 502, "command_log_unavailable", err.Error())
		return
	}
	if result.Result != "ok" {
		apiError(w, 502, "command_log_unavailable", result.Message)
		return
	}
	writeJSON(w, 200, map[string]any{"job_id": job.ID, "state": job.State, "log": result.Log,
		"truncated": result.LogTruncated, "log_bytes": job.LogBytes})
}

// agentToolCommandCancel ends one of the caller's jobs. Its end still arrives as a CommandEnded frame, which wakes the agent with a note.
func (s *Server) agentToolCommandCancel(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if key := r.Header.Get("Idempotency-Key"); key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required")
		return
	}
	job, ok := s.ownedCommand(w, r, caller)
	if !ok {
		return
	}
	if commandTerminal(job.State) {
		writeJSON(w, 200, job)
		return
	}
	if job.State == "queued" {
		cancelled, err := s.Store.CancelCommand(r.Context(), job.ID)
		if err != nil {
			fail(w, err)
			return
		}
		// Its start is unconfirmed, not refused: the machine may run it already, or be about to (a cancel sent now follows its run
		// request on the same connection). Best effort: a late start kills it too, and so does the machine's next hello.
		if conn, err := s.machineForOwner(job.MachineID, job.JournalID); err == nil && conn.supportsCommands {
			_ = conn.send(protocol.Frame{V: protocol.Version, Type: protocol.CommandCancel, ReqID: newRequestNonce(), JobID: job.ID, AgentID: job.AgentID})
		}
		writeJSON(w, 200, cancelled)
		return
	}
	result, err := s.machineCommand(r.Context(), job.MachineID, protocol.Frame{V: protocol.Version,
		Type: protocol.CommandCancel, ReqID: newRequestNonce(), JobID: job.ID, AgentID: job.AgentID}, 30*time.Second)
	if err != nil {
		apiError(w, 502, "command_cancel_failed", err.Error())
		return
	}
	if result.Result != "ok" {
		apiError(w, 409, "command_cancel_failed", result.Message)
		return
	}
	current, err := s.Store.CommandJobByAnyAgent(r.Context(), job.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, current)
}

func (s *Server) ownedCommand(w http.ResponseWriter, r *http.Request, caller store.AgentToolIdentity) (store.CommandJob, bool) {
	job, err := s.Store.CommandJobByID(r.Context(), caller.ID, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "no such command")
		return store.CommandJob{}, false
	}
	if err != nil {
		fail(w, err)
		return store.CommandJob{}, false
	}
	return job, true
}

// commandSupervisorReplaced is the error a command's end carries when its supervisor ended it on its way out, being replaced.
const commandSupervisorReplaced = protocol.CommandEndSupervisorReplaced + ": the supervisor was replaced while the command ran"

// commandLostInSwap is the error of a command whose process the supervisor that took over found gone without an exit code.
const commandLostInSwap = protocol.CommandEndLostInSwap + ": its process ended without an exit code while its supervisor was replaced; run it again if you still need it"

func commandTerminal(state string) bool {
	return state != "queued" && state != "running"
}

// handleCommandEnd applies one command's end from its machine: the job's state, and the note that wakes the agent. The frame is
// acknowledged whatever the job turns out to be, so a report of an end never repeats forever.
func (s *Server) handleCommandEnd(ctx context.Context, conn *machineConn, frame protocol.Frame) error {
	s.maintenanceMu.RLock()
	defer s.maintenanceMu.RUnlock()
	if frame.CommandExit == nil {
		return errors.New("missing command exit")
	}
	exit := *frame.CommandExit
	if frame.AgentID != "" && frame.AgentID != exit.AgentID {
		return store.ErrForbidden
	}
	if _, err := s.commandOwned(ctx, conn, exit.JobID, exit.AgentID); errors.Is(err, sql.ErrNoRows) {
		// No job left to record it on: acknowledged, so the machine stops replaying it.
		return conn.send(protocol.Frame{V: protocol.Version, Type: protocol.Ack, JobID: exit.JobID, Level: "command_exit"})
	} else if err != nil {
		return err
	}
	state, message := "done", ""
	switch {
	case exit.Refused != "":
		// The launcher refused the command before it ran (an image from before command runs): not an exit of the person's command.
		state, message = "failed", exit.Refused
		exit.ExitCode = nil
	case exit.Lost:
		state = "lost"
		if exit.Reason == protocol.CommandEndLostInSwap {
			message = commandLostInSwap
		}
	case exit.Unconfirmed:
		// A kill whose end in the container is not confirmed may have left the process running: that outranks why it was killed.
		state = "cancelled_unconfirmed"
		if exit.TimedOut {
			message = "it reached its timeout; the kill could not be confirmed"
		} else if exit.Reason == protocol.CommandEndSupervisorReplaced {
			message = commandSupervisorReplaced + "; the kill could not be confirmed"
		}
	case exit.TimedOut:
		state = "timed_out"
	case exit.Reason == protocol.CommandEndSupervisorReplaced:
		state, message = "cancelled", commandSupervisorReplaced+"; run it again if you still need it"
	case exit.Killed:
		state = "cancelled"
	}
	if _, err := s.Store.FinishCommand(ctx, exit.JobID, store.CommandResult{State: state, ExitCode: exit.ExitCode, Error: message,
		StartedAt: exit.StartedAt, EndedAt: exit.EndedAt, Reported: true, LogBytes: exit.LogBytes, TailTruncated: exit.TailTruncated, Tail: exit.Tail}); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return conn.send(protocol.Frame{V: protocol.Version, Type: protocol.Ack, JobID: exit.JobID, Level: "command_exit"})
}

// commandInContainer says whether a command belongs in the agent's container: a docker variant, or a launcher the machine reports as
// containerized or does not report at all.
func (c *machineConn) commandInContainer(spec protocol.CommandSpec) bool {
	if spec.Variant == "docker" {
		return true
	}
	if spec.LauncherID == "" {
		return false
	}
	c.harnessMu.Lock()
	defer c.harnessMu.Unlock()
	for _, row := range c.launchers {
		if row.ID == spec.LauncherID {
			return row.Containerized
		}
	}
	// A launcher the machine does not report: an older supervisor would run the command on the host, a current one refuses it.
	return true
}

// machineCommand asks the agent's machine one question and waits for its answer, like the worktree and agent-log requests do.
func (s *Server) machineCommand(ctx context.Context, machineID string, frame protocol.Frame, timeout time.Duration) (protocol.Frame, error) {
	var conn *machineConn
	var err error
	if frame.JobID != "" {
		// Every frame about a job, the run request included, goes to the instance the job names: run_command picked that instance when it
		// created the job, and a hello that moved the active instance on since must not send the command to a different one.
		job, lookupErr := s.Store.CommandJobByAnyAgent(ctx, frame.JobID)
		if lookupErr != nil {
			return protocol.Frame{}, lookupErr
		}
		conn, err = s.machineForOwner(machineID, job.JournalID)
	} else {
		conn, err = s.machineForDispatch(machineID)
	}
	if err != nil {
		return protocol.Frame{}, err
	}
	if !conn.supportsCommands {
		return protocol.Frame{}, errCommandUnsupported
	}
	if frame.Type == protocol.CommandRun && frame.Command != nil && !conn.supportsContainerCommands && conn.commandInContainer(*frame.Command) {
		return protocol.Frame{}, errContainerCommandUnsupported
	}
	if frame.ReqID == "" {
		frame.ReqID = newRequestNonce()
	}
	channel := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	if conn.pathPending == nil {
		conn.pathPending = make(map[string]chan protocol.Frame)
	}
	conn.pathPending[frame.ReqID] = channel
	conn.harnessMu.Unlock()
	defer func() {
		conn.harnessMu.Lock()
		delete(conn.pathPending, frame.ReqID)
		conn.harnessMu.Unlock()
	}()
	if err := conn.send(frame); err != nil {
		return protocol.Frame{}, err
	}
	// From here on the machine may have the request: an error says only that no answer came.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-channel:
		return result, nil
	case <-conn.done:
		return protocol.Frame{}, commandUnansweredError{fmt.Errorf("%w: supervisor disconnected during the command request", ErrMachineUnavailable)}
	case <-ctx.Done():
		return protocol.Frame{}, commandUnansweredError{ctx.Err()}
	case <-timer.C:
		return protocol.Frame{}, commandUnansweredError{fmt.Errorf("%w: supervisor did not answer the command request", ErrSlowMachine)}
	}
}

func newRequestNonce() string {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(nonce[:])
}

func commandError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrCommandInvalid):
		apiError(w, 400, "invalid_command", err.Error())
	case errors.Is(err, store.ErrCommandLimit):
		apiError(w, 409, "command_limit", err.Error())
	case errors.Is(err, store.ErrScopeMismatch):
		apiError(w, 403, "forbidden", err.Error())
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "conflict", "that request key was used for a different command")
	default:
		fail(w, err)
	}
}

// sweepUnconfirmedCommands ends the jobs whose start their machine never confirmed and never will: the run request went out on a connection
// that has since been replaced (the hello of the new one leaves a job that young alone, as its request may still be in flight), or the job
// has waited longer than any request could. Each is failed and its agent woken; a late start or end from the machine still corrects it.
func (s *Server) sweepUnconfirmedCommands(ctx context.Context) error {
	now := time.Now()
	jobs, err := s.Store.UnconfirmedCommands(ctx, now.Add(-store.CommandUnconfirmedWindow))
	if err != nil {
		return err
	}
	for _, job := range jobs {
		created, err := time.Parse(commandTimeLayout, job.CreatedAt)
		if err != nil {
			continue
		}
		replaced := false
		if conn, err := s.machineForOwner(job.MachineID, job.JournalID); err == nil {
			replaced = time.Unix(0, conn.connectedAt.Load()).After(created)
		}
		if !replaced && now.Sub(created) < store.CommandUnconfirmedMax {
			continue // still on the connection it was sent on (or the machine is offline): its answer or the next hello settles it
		}
		if _, err := s.Store.FailUnconfirmedCommand(ctx, job.ID); err != nil {
			return err
		}
	}
	return nil
}
