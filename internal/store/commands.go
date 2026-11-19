package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

var ErrCommandInvalid = errors.New("invalid command")
var ErrCommandLimit = errors.New("command limit")

const (
	// CommandMaxPerAgent is how many commands one agent may have queued or running at once. A long command runs outside its turn, so more of
	// them at once is just contention on one checkout.
	CommandMaxPerAgent = 2
	// CommandTimeoutDefault is the timeout a command gets when its caller names none, and CommandTimeoutMax the longest one that may ask for.
	CommandTimeoutDefault = 3600
	CommandTimeoutMax     = 6 * 3600
	// CommandMaxCmdBytes bounds one command line; CommandMaxTailBytes bounds the tail a wake note carries (the whole output stays on disk).
	CommandMaxCmdBytes  = 8 << 10
	CommandMaxTailBytes = 8 << 10
)

// CommandJob is one long command an agent started outside its turn. It runs under the supervisor in the agent's own checkout, and its
// completion wakes the agent with a note.
type CommandJob struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	Generation     int    `json:"generation"`
	MachineID      string `json:"machine_id"`
	ProjectID      string `json:"project_id"`
	RoomID         string `json:"room_id"`
	Label          string `json:"label,omitempty"`
	Cmd            string `json:"cmd"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	State          string `json:"state"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	LogPath        string `json:"log_path"`
	VisibleLogPath string `json:"visible_log_path,omitempty"`
	VisibleCwd     string `json:"visible_cwd,omitempty"`
	LogBytes       int64  `json:"log_bytes,omitempty"`
	TailTruncated  bool   `json:"tail_truncated,omitempty"`
	Error          string `json:"error,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	EndedAt        string `json:"ended_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	// CheckoutPath is where the agent works; the job's log is written beneath it, in the git-excluded runtime state the agent can open.
	CheckoutPath string `json:"-"`
}

const commandColumns = `id,agent_id,generation,machine_id,project_id,room_id,label,cmd,cwd,timeout_seconds,
	state,exit_code,log_path,visible_log_path,visible_cwd,log_bytes,tail_truncated,COALESCE(error,''),COALESCE(started_at,''),COALESCE(ended_at,''),created_at`

func scanCommand(row interface{ Scan(...any) error }) (CommandJob, error) {
	var job CommandJob
	var exitCode sql.NullInt64
	err := row.Scan(&job.ID, &job.AgentID, &job.Generation, &job.MachineID, &job.ProjectID, &job.RoomID,
		&job.Label, &job.Cmd, &job.Cwd, &job.TimeoutSeconds, &job.State, &exitCode, &job.LogPath,
		&job.VisibleLogPath, &job.VisibleCwd, &job.LogBytes, &job.TailTruncated, &job.Error, &job.StartedAt, &job.EndedAt, &job.CreatedAt)
	if err != nil {
		return CommandJob{}, err
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		job.ExitCode = &code
	}
	return job, nil
}

// validateCommand normalizes and bounds one command request.
func validateCommand(job CommandJob) (CommandJob, error) {
	job.Label = strings.TrimSpace(job.Label)
	job.Cmd = strings.TrimSpace(job.Cmd)
	job.Cwd = strings.TrimSpace(job.Cwd)
	if job.Cmd == "" || len(job.Cmd) > CommandMaxCmdBytes {
		return CommandJob{}, fmt.Errorf("%w: cmd is required and at most %d bytes", ErrCommandInvalid, CommandMaxCmdBytes)
	}
	if utf8.RuneCountInString(job.Label) > 80 {
		return CommandJob{}, fmt.Errorf("%w: label is at most 80 characters", ErrCommandInvalid)
	}
	if job.Cwd == "" {
		job.Cwd = "."
	}
	if !CommandCwdInside(job.Cwd) {
		return CommandJob{}, fmt.Errorf("%w: cwd must stay inside the agent's checkout", ErrCommandInvalid)
	}
	job.Cwd = path.Clean(job.Cwd)
	if job.TimeoutSeconds == 0 {
		job.TimeoutSeconds = CommandTimeoutDefault
	}
	if job.TimeoutSeconds < 1 || job.TimeoutSeconds > CommandTimeoutMax {
		return CommandJob{}, fmt.Errorf("%w: timeout_seconds is between 1 and %d", ErrCommandInvalid, CommandTimeoutMax)
	}
	return job, nil
}

// CommandCwdInside says whether a command's cwd, relative to the agent's checkout, stays inside it: not absolute, and not "..", "./.." or
// "a/../.." once cleaned.
func CommandCwdInside(cwd string) bool {
	if cwd == "" {
		return true
	}
	if filepathIsAbsolute(cwd) || strings.Contains(cwd, "\x00") {
		return false
	}
	clean := path.Clean(cwd)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

// filepathIsAbsolute is filepath.IsAbsolute without importing the package here: a Windows drive path is absolute too.
func filepathIsAbsolute(path string) bool {
	return strings.HasPrefix(path, "/") || (len(path) > 1 && path[1] == ':')
}

// commandLogPath is where one job's log lives: beneath the agent's checkout, in the runtime state no commit carries.
func commandLogPath(checkout, jobID string) string {
	return strings.TrimRight(checkout, "/") + "/.aimebu/commands/" + jobID + ".log"
}

// CreateCommandJob stores a new job for the caller. The request key makes a retried tool call return the job it already made.
func (s *Store) CreateCommandJob(ctx context.Context, job CommandJob, request RequestKey) (CommandJob, bool, error) {
	var created CommandJob
	reused := false
	result, wasReused, err := s.WithRequestKey(ctx, request, func(tx *Tx) (string, error) {
		item, err := tx.CreateCommandJob(job)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(item)
		if err != nil {
			return "", err
		}
		created = item
		return string(data), nil
	})
	if err != nil {
		return CommandJob{}, false, err
	}
	reused = wasReused
	if reused {
		if err := json.Unmarshal([]byte(result), &created); err != nil {
			return CommandJob{}, true, err
		}
	}
	return created, reused, nil
}

// CreateCommandJob inserts the job row after checking the per-agent cap. The log path is the caller's (the supervisor writes there).
func (tx *Tx) CreateCommandJob(job CommandJob) (CommandJob, error) {
	job, err := validateCommand(job)
	if err != nil {
		return CommandJob{}, err
	}
	var running int
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT COUNT(*) FROM command_job WHERE agent_id=? AND state IN ('queued','running')`, job.AgentID).Scan(&running); err != nil {
		return CommandJob{}, err
	}
	if running >= CommandMaxPerAgent {
		return CommandJob{}, fmt.Errorf("%w: %d commands already queued or running", ErrCommandLimit, running)
	}
	var desired, roomLifecycle, roomProject string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.desired,r.lifecycle,r.project_id FROM agent a JOIN room r ON r.id=?
		WHERE a.id=?`, job.RoomID, job.AgentID).Scan(&desired, &roomLifecycle, &roomProject); err != nil {
		return CommandJob{}, err
	}
	if roomProject != job.ProjectID {
		return CommandJob{}, fmt.Errorf("%w: room differs from the agent's project", ErrScopeMismatch)
	}
	if desired == "stopped" {
		return CommandJob{}, fmt.Errorf("%w: stopped agent cannot start commands", ErrCommandInvalid)
	}
	var member int
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`,
		job.RoomID, job.AgentID).Scan(&member); err != nil {
		return CommandJob{}, err
	}
	if member == 0 {
		return CommandJob{}, fmt.Errorf("%w: the agent is not a member of that room", ErrScopeMismatch)
	}
	if roomLifecycle != "open" {
		return CommandJob{}, fmt.Errorf("%w: room is closed", ErrCommandInvalid)
	}
	job.ID, err = newID("cmd")
	if err != nil {
		return CommandJob{}, err
	}
	if job.LogPath == "" {
		if job.CheckoutPath == "" {
			return CommandJob{}, fmt.Errorf("%w: the agent's checkout is required", ErrCommandInvalid)
		}
		job.LogPath = commandLogPath(job.CheckoutPath, job.ID)
	}
	job.State = "queued"
	job.CreatedAt = timestamp()
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO command_job
		(id,agent_id,generation,machine_id,project_id,room_id,label,cmd,cwd,timeout_seconds,state,
		 log_path,visible_log_path,log_bytes,tail_truncated,error,started_at,ended_at,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, job.ID, job.AgentID, job.Generation, job.MachineID,
		job.ProjectID, job.RoomID, job.Label, job.Cmd, job.Cwd, job.TimeoutSeconds, job.State,
		job.LogPath, job.VisibleLogPath, job.LogBytes, job.TailTruncated, nullableString(job.Error),
		nullableString(job.StartedAt), nullableString(job.EndedAt), job.CreatedAt)
	if err != nil {
		return CommandJob{}, err
	}
	if _, err := tx.AppendEvent(Event{Kind: "command_started", ProjectID: job.ProjectID, RoomID: job.RoomID,
		AgentID: job.AgentID, PayloadJSON: payloadJSON(map[string]string{"job_id": job.ID, "label": job.Label})}); err != nil {
		return CommandJob{}, err
	}
	return job, nil
}

// RunningTurnRoom is the room the agent's current turn runs in: where a command it starts is reported when it ends. A tool call comes from
// a turn, and one agent has at most one.
func (s *Store) RunningTurnRoom(ctx context.Context, agentID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var roomID string
	err := s.db.QueryRowContext(ctx, `SELECT room_id FROM turn WHERE agent_id=? AND exec IN ('pending','running') ORDER BY id DESC LIMIT 1`,
		agentID).Scan(&roomID)
	return roomID, err
}

// RecordCommandStarted marks a job as running on its machine, with the spellings of its log and working directory the agent can open. A
// job that already ended (a fast command's end can arrive before its start is recorded) keeps its end and only gains those spellings: the
// current row is returned either way, and sql.ErrNoRows means there is no such job.
func (s *Store) RecordCommandStarted(ctx context.Context, id, startedAt, visibleLogPath, visibleCwd string) (CommandJob, error) {
	var job CommandJob
	err := s.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE command_job SET state='running',started_at=COALESCE(started_at,?),visible_log_path=?,
			visible_cwd=? WHERE id=? AND state='queued'`, startedAt, visibleLogPath, visibleCwd, id); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE command_job SET started_at=COALESCE(started_at,?),
			visible_log_path=CASE WHEN visible_log_path='' THEN ? ELSE visible_log_path END,
			visible_cwd=CASE WHEN visible_cwd='' THEN ? ELSE visible_cwd END WHERE id=?`, startedAt, visibleLogPath, visibleCwd, id); err != nil {
			return err
		}
		var err error
		job, err = scanCommand(tx.tx.QueryRowContext(tx.ctx, `SELECT `+commandColumns+` FROM command_job WHERE id=?`, id))
		return err
	})
	return job, err
}

// FailCommandStart records that a job never ran: the machine refused it, or the request never reached it. The tool call itself says why,
// so nothing wakes. Only a job whose start is unconfirmed (queued) can fail; sql.ErrNoRows means it is past that.
func (s *Store) FailCommandStart(ctx context.Context, id, message string) (CommandJob, error) {
	var job CommandJob
	err := s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.tx.ExecContext(tx.ctx, `UPDATE command_job SET state='failed',error=?,ended_at=? WHERE id=? AND state='queued'`,
			message, timestamp(), id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		job, err = scanCommand(tx.tx.QueryRowContext(tx.ctx, `SELECT `+commandColumns+` FROM command_job WHERE id=?`, id))
		if err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "command_finished", ProjectID: job.ProjectID, RoomID: job.RoomID,
			AgentID: job.AgentID, PayloadJSON: payloadJSON(map[string]string{"job_id": job.ID, "label": job.Label,
				"state": "failed", "error": message, "not_woken": "start_failed"})})
		return err
	})
	return job, err
}

// CancelCommand records the end of a job cancelled before it ever ran on a machine: its caller already sees the state in the tool result,
// so nothing wakes.
func (s *Store) CancelCommand(ctx context.Context, id string) (CommandJob, error) {
	var job CommandJob
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		job, err = scanCommand(tx.tx.QueryRowContext(tx.ctx, `SELECT `+commandColumns+` FROM command_job WHERE id=?`, id))
		if err != nil {
			return err
		}
		if job.State != "queued" {
			return nil // only a job that never started is cancelled here; a running one ends on its machine's report
		}
		if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE command_job SET state='cancelled',ended_at=? WHERE id=?`, timestamp(), id); err != nil {
			return err
		}
		job.State, job.EndedAt = "cancelled", timestamp()
		_, err = tx.AppendEvent(Event{Kind: "command_finished", ProjectID: job.ProjectID, RoomID: job.RoomID,
			AgentID: job.AgentID, PayloadJSON: payloadJSON(map[string]string{"job_id": job.ID, "label": job.Label,
				"state": "cancelled", "not_woken": "cancelled_before_start"})})
		return err
	})
	return job, err
}

// CommandJobs lists one agent's own jobs, newest first.
func (s *Store) CommandJobs(ctx context.Context, agentID string) ([]CommandJob, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+commandColumns+` FROM command_job WHERE agent_id=? ORDER BY created_at DESC,id DESC LIMIT 100`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []CommandJob{}
	for rows.Next() {
		job, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// CommandJobByID reads one job. sql.ErrNoRows means there is none (or it is another agent's).
func (s *Store) CommandJobByID(ctx context.Context, agentID, id string) (CommandJob, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return CommandJob{}, err
	}
	return scanCommand(s.db.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_job WHERE id=? AND agent_id=?`, id, agentID))
}

// CommandJobByAnyAgent reads one job whatever the caller, for the machine's reports.
func (s *Store) CommandJobByAnyAgent(ctx context.Context, id string) (CommandJob, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return CommandJob{}, err
	}
	return scanCommand(s.db.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_job WHERE id=?`, id))
}

// CommandResult is what the supervisor reports when a job ends.
type CommandResult struct {
	State         string // done | cancelled | cancelled_unconfirmed | timed_out | lost | failed
	ExitCode      *int
	StartedAt     string // when the machine started it, for a job whose start was never recorded
	EndedAt       string
	LogBytes      int64
	TailTruncated bool
	Tail          string // already redacted by the supervisor
	Error         string
}

// FinishCommand records the end of a job and wakes its agent with a note in the room the command was started from. A job whose agent is
// stopped or gone, or whose room is closed or left, gets no note: its result stays in list_commands and the event says why nothing woke.
func (s *Store) FinishCommand(ctx context.Context, id string, result CommandResult) (CommandJob, error) {
	var job CommandJob
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		job, err = finishCommand(tx, id, result)
		return err
	})
	return job, err
}

func finishCommand(tx *Tx, id string, result CommandResult) (CommandJob, error) {
	job, err := scanCommand(tx.tx.QueryRowContext(tx.ctx, `SELECT `+commandColumns+` FROM command_job WHERE id=?`, id))
	if err != nil {
		return CommandJob{}, err
	}
	if job.State == "done" || job.State == "cancelled" || job.State == "cancelled_unconfirmed" || job.State == "timed_out" || job.State == "lost" || job.State == "failed" {
		return job, nil // a replayed report of an end already recorded changes nothing
	}
	if result.State == "" {
		result.State = "done"
	}
	if result.EndedAt == "" {
		result.EndedAt = timestamp()
	}
	if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE command_job SET state=?,exit_code=?,ended_at=?,log_bytes=?,tail_truncated=?,error=?,
		started_at=COALESCE(started_at,?) WHERE id=?`, result.State, nullableExitCode(result.ExitCode), result.EndedAt, result.LogBytes,
		result.TailTruncated, nullableString(result.Error), nullableString(result.StartedAt), id); err != nil {
		return CommandJob{}, err
	}
	if job.StartedAt == "" {
		job.StartedAt = result.StartedAt
	}
	job.State, job.EndedAt, job.LogBytes = result.State, result.EndedAt, result.LogBytes
	job.ExitCode, job.TailTruncated, job.Error = result.ExitCode, result.TailTruncated, result.Error

	notWoken := ""
	var roomLifecycle, desired string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.lifecycle,a.desired FROM room r JOIN agent a ON a.id=?
		WHERE r.id=?`, job.AgentID, job.RoomID).Scan(&roomLifecycle, &desired); err != nil {
		return CommandJob{}, err
	}
	switch {
	case desired == "stopped":
		notWoken = "agent_stopped"
	case roomLifecycle != "open":
		notWoken = "room_closed"
	default:
		var member int
		if err := tx.tx.QueryRowContext(tx.ctx,
			`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`,
			job.RoomID, job.AgentID).Scan(&member); err != nil {
			return CommandJob{}, err
		}
		if member == 0 {
			notWoken = "not_member"
		}
	}
	payload := map[string]string{"job_id": job.ID, "label": job.Label, "state": job.State}
	if job.ExitCode != nil {
		payload["exit_code"] = fmt.Sprint(*job.ExitCode)
	}
	if notWoken != "" {
		payload["not_woken"] = notWoken
		if _, err := tx.AppendEvent(Event{Kind: "command_finished", ProjectID: job.ProjectID, RoomID: job.RoomID,
			AgentID: job.AgentID, PayloadJSON: payloadJSON(payload)}); err != nil {
			return CommandJob{}, err
		}
		return job, nil
	}
	message, err := tx.InsertMessage(Message{RoomID: job.RoomID, AuthorKind: "system", AuthorID: "system",
		Body: commandNoteBody(job, result.Tail)})
	if err != nil {
		return CommandJob{}, err
	}
	if _, err := tx.InsertNote(Note{AgentID: job.AgentID, MessageID: message.ID, RoomID: job.RoomID,
		WakeReason: "command_finished"}); err != nil {
		return CommandJob{}, err
	}
	payload["message_no"] = fmt.Sprint(message.No)
	if _, err := tx.AppendEvent(Event{Kind: "command_finished", ProjectID: job.ProjectID, RoomID: job.RoomID,
		AgentID: job.AgentID, PayloadJSON: payloadJSON(payload)}); err != nil {
		return CommandJob{}, err
	}
	return job, nil
}

func nullableExitCode(code *int) any {
	if code == nil {
		return nil
	}
	return *code
}

// commandNoteBody is the wake note itself: what ran, how it ended, the last lines of its output and where the whole log is. The tail is
// already redacted and bounded; anything longer lives in the log.
func commandNoteBody(job CommandJob, tail string) string {
	ending := "finished"
	switch job.State {
	case "cancelled":
		ending = "was cancelled"
	case "cancelled_unconfirmed":
		ending = "was cancelled, but the end could not be confirmed (the process may still be running)"
	case "timed_out":
		ending = "timed out and was killed"
	case "lost":
		ending = "was lost when the supervisor restarted: its exit code is unknown"
	case "failed":
		ending = "was never started"
	}
	outcome := ""
	if job.ExitCode != nil {
		outcome = fmt.Sprintf(" (exit %d", *job.ExitCode)
	} else {
		outcome = " ("
	}
	if started, err := time.Parse(timestampLayout, job.StartedAt); err == nil {
		if ended, err := time.Parse(timestampLayout, job.EndedAt); err == nil && ended.After(started) {
			outcome += fmt.Sprintf(", %s", commandDuration(ended.Sub(started)))
		}
	}
	outcome += ")"
	var b strings.Builder
	label := job.Label
	if label == "" {
		label = job.Cmd
	}
	fmt.Fprintf(&b, "Command %s: %s%s\ncmd: %s\ncwd: %s\n", label, ending, outcome, job.Cmd, job.Cwd)
	if job.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", job.Error)
	}
	if tail != "" {
		b.WriteString("last lines of output:\n```text\n")
		b.WriteString(strings.TrimRight(tail, "\n"))
		b.WriteString("\n```\n")
	}
	fmt.Fprintf(&b, "full log: %s", job.LogPath)
	if job.VisibleLogPath != "" && job.VisibleLogPath != job.LogPath {
		fmt.Fprintf(&b, " (here: %s)", job.VisibleLogPath)
	}
	if job.TailTruncated {
		b.WriteString("\n(the output was too large: the log holds the truncated tail)")
	}
	return b.String()
}

// commandDuration spells a duration the way the note reads it: "12m04s".
func commandDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", hours, minutes, seconds)
	}
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

// ReconcileMachineCommands settles one machine's unfinished jobs against what its supervisor said in hello: the commands it still runs
// (open) and the ends it has not been acknowledged for (exits, replayed right after). A job whose start was never confirmed but which the
// machine runs becomes running; a job the machine knows nothing of can no longer be reported, so it ends now (failed when it never started,
// lost when its end was never reported) and its agent is woken. It returns the jobs the machine runs that were cancelled before their start
// was confirmed: the caller kills them.
func (s *Store) ReconcileMachineCommands(ctx context.Context, machineID string, open []protocol.CommandOpen, exits []protocol.CommandExit) ([]CommandJob, error) {
	openByID := make(map[string]protocol.CommandOpen, len(open))
	for _, item := range open {
		openByID[item.JobID] = item
	}
	ended := make(map[string]bool, len(exits))
	for _, exit := range exits {
		ended[exit.JobID] = true
	}
	var kill []CommandJob
	err := s.Tx(ctx, func(tx *Tx) error {
		kill = nil
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT `+commandColumns+` FROM command_job WHERE machine_id=? AND state IN ('queued','running')`, machineID)
		if err != nil {
			return err
		}
		var unfinished []CommandJob
		for rows.Next() {
			job, err := scanCommand(rows)
			if err != nil {
				rows.Close()
				return err
			}
			unfinished = append(unfinished, job)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		known := map[string]bool{}
		for _, job := range unfinished {
			known[job.ID] = true
			if ended[job.ID] {
				continue // its end is replayed right after hello
			}
			if item, running := openByID[job.ID]; running {
				if job.State == "queued" {
					if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE command_job SET state='running',started_at=COALESCE(started_at,?) WHERE id=?`,
						nullableString(item.StartedAt), job.ID); err != nil {
						return err
					}
				}
				continue
			}
			result := CommandResult{State: "lost", Error: "the machine's supervisor no longer knows this command, so its end will never be reported"}
			if job.State == "queued" {
				result = CommandResult{State: "failed", Error: "the machine never started this command"}
			}
			if _, err := finishCommand(tx, job.ID, result); err != nil {
				return err
			}
		}
		for id := range openByID {
			if known[id] {
				continue
			}
			job, err := scanCommand(tx.tx.QueryRowContext(tx.ctx, `SELECT `+commandColumns+` FROM command_job WHERE id=? AND machine_id=?`, id, machineID))
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if job.State == "cancelled" {
				kill = append(kill, job)
			}
		}
		return nil
	})
	return kill, err
}
