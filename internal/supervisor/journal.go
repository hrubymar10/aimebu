package supervisor

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	_ "modernc.org/sqlite"
)

var ErrJournalConflict = errors.New("journal conflict")
var ErrJournalInUse = errors.New("supervisor journal already in use")

// JournalOwner probes the journal's process lock without opening the database.
// A free lock means no supervisor owns it; pid is advisory while it is held.
func JournalOwner(path string) (held bool, pid string, err error) {
	held, pid, _, err = JournalLockInfo(path)
	return held, pid, err
}

// JournalLockInfo is JournalOwner plus the journal ID the holder wrote after its pid, so a manager can tell which instance a process is without
// opening the database: "" while the holder has not written it yet, and for a holder from before instances had one.
func JournalLockInfo(path string) (held bool, pid, journalID string, err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, "", "", err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, "", "", err
	}
	defer file.Close()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			data, readErr := os.ReadFile(path + ".lock")
			if readErr != nil {
				return true, "", "", readErr
			}
			lines := strings.Fields(string(data))
			if len(lines) > 0 {
				pid = lines[0]
			}
			if len(lines) > 1 {
				journalID = lines[1]
			}
			return true, pid, journalID, nil
		}
		return false, "", "", err
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false, "", "", nil
}

type Journal struct {
	db       *sql.DB
	lockFile *os.File
	ID       string
	path     string
	closed   atomic.Bool
}

// OpenJournal uses SQLite's FULL synchronous WAL. A committed turn_start is
// fsynced before Start may launch the tool or send the accepted ack. The
// exclusive flock on the stable sibling .lock file prevents two supervisor
// processes from sharing one journal while reporting incompatible in-memory
// process-liveness maps to the server.
func OpenJournal(ctx context.Context, path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			_ = lockFile.Close()
		}
	}()
	if err := lockFile.Chmod(0600); err != nil {
		return nil, err
	}
	// A status probe briefly owns this lock too. Wait a bounded moment so a
	// supervisor racing that probe does not mistake it for a second owner.
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		if time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		pidBytes, _ := os.ReadFile(path + ".lock")
		pid := strings.TrimSpace(string(pidBytes))
		if pid == "" {
			pid = "unknown"
		}
		return nil, fmt.Errorf("%w by pid %s", ErrJournalInUse, pid)
	}
	if err := lockFile.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := lockFile.Seek(0, 0); err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintln(lockFile, os.Getpid()); err != nil {
		return nil, err
	}
	if err := lockFile.Sync(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	db, err := sql.Open("sqlite", path+"?"+query.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS turn_start(
		turn_id TEXT PRIMARY KEY,agent_id TEXT NOT NULL,generation INTEGER NOT NULL,
		desired_rev INTEGER NOT NULL,note_ids_json TEXT NOT NULL,mode TEXT NOT NULL,
		spec_json BLOB NOT NULL,started_at TEXT NOT NULL,exec TEXT NOT NULL DEFAULT 'running',
		exit_code INTEGER,terminal_event INTEGER,cancel_intent INTEGER,
		exit_acked INTEGER NOT NULL DEFAULT 0,
		pid INTEGER,pgid INTEGER,pid_start TEXT);
	CREATE TABLE IF NOT EXISTS journal_event(
		source_event_id TEXT PRIMARY KEY,turn_id TEXT NOT NULL REFERENCES turn_start(turn_id),
		kind TEXT NOT NULL,summary TEXT NOT NULL,payload_json BLOB,created_at TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS answer(
		turn_id TEXT NOT NULL REFERENCES turn_start(turn_id),answer_rev INTEGER NOT NULL,
		digest TEXT NOT NULL,candidate BLOB,created_at TEXT NOT NULL,
		PRIMARY KEY(turn_id,answer_rev));
	CREATE TABLE IF NOT EXISTS receipt(
		turn_id TEXT NOT NULL REFERENCES turn_start(turn_id),answer_rev INTEGER NOT NULL,
		digest TEXT NOT NULL,result TEXT NOT NULL,details BLOB,terminal INTEGER NOT NULL,
		created_at TEXT NOT NULL,PRIMARY KEY(turn_id,answer_rev));
	CREATE TABLE IF NOT EXISTS journal_meta(
		id INTEGER PRIMARY KEY CHECK(id=1),journal_id TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS warm_process(
		pid INTEGER PRIMARY KEY,agent_id TEXT NOT NULL,started TEXT NOT NULL,recorded_at TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS command_start(
		job_id TEXT PRIMARY KEY,agent_id TEXT NOT NULL,generation INTEGER NOT NULL,pid INTEGER NOT NULL,
		pid_start TEXT NOT NULL,cmd TEXT NOT NULL,log_path TEXT NOT NULL,started_at TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS command_exit(
		job_id TEXT PRIMARY KEY REFERENCES command_start(job_id),exit_code INTEGER,timed_out INTEGER NOT NULL DEFAULT 0,
		killed INTEGER NOT NULL DEFAULT 0,unconfirmed INTEGER NOT NULL DEFAULT 0,lost INTEGER NOT NULL DEFAULT 0,
		started_at TEXT NOT NULL,ended_at TEXT NOT NULL,log_bytes INTEGER NOT NULL DEFAULT 0,
		tail_truncated INTEGER NOT NULL DEFAULT 0,tail TEXT NOT NULL DEFAULT '',exit_acked INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureExitAckColumn(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureProcessColumns(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(ctx, db, "command_start", "timeout_seconds", `ALTER TABLE command_start ADD COLUMN timeout_seconds INTEGER NOT NULL DEFAULT 0`); err != nil {
		db.Close()
		return nil, err
	}
	// A command run through a Docker launcher: a restarted supervisor that kills it cannot read the launcher's exit status, so it must
	// not take that kill as confirmed.
	if err := ensureColumn(ctx, db, "command_start", "container", `ALTER TABLE command_start ADD COLUMN container INTEGER NOT NULL DEFAULT 0`); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(ctx, db, "command_exit", "refused", `ALTER TABLE command_exit ADD COLUMN refused TEXT NOT NULL DEFAULT ''`); err != nil {
		db.Close()
		return nil, err
	}
	// When the server acknowledged a turn's exit, so pruning keeps a settled turn for its grace period (empty on a row acknowledged before it was kept).
	if err := ensureColumn(ctx, db, "turn_start", "settled_at", `ALTER TABLE turn_start ADD COLUMN settled_at TEXT NOT NULL DEFAULT ''`); err != nil {
		db.Close()
		return nil, err
	}
	// Pruning and the exit-diagnostic reads look a turn's events up by turn.
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS journal_event_turn ON journal_event(turn_id)`); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(ctx, db, "command_exit", "reason", `ALTER TABLE command_exit ADD COLUMN reason TEXT NOT NULL DEFAULT ''`); err != nil {
		db.Close()
		return nil, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO journal_meta(id,journal_id) VALUES (1,?)`, hex.EncodeToString(random)); err != nil {
		db.Close()
		return nil, err
	}
	j := &Journal{db: db, lockFile: lockFile, path: path}
	if err := db.QueryRowContext(ctx, `SELECT journal_id FROM journal_meta WHERE id=1`).Scan(&j.ID); err != nil {
		db.Close()
		return nil, err
	}
	// The holder's journal ID goes after its pid, for the manager that tells instances apart by it. A failure only costs that.
	if _, err := lockFile.Seek(0, 0); err == nil && lockFile.Truncate(0) == nil {
		_, _ = fmt.Fprintf(lockFile, "%d\n%s\n", os.Getpid(), j.ID)
		_ = lockFile.Sync()
	}
	owned = true
	return j, nil
}

func ensureExitAckColumn(ctx context.Context, db *sql.DB) error {
	return ensureColumn(ctx, db, "turn_start", "exit_acked", `ALTER TABLE turn_start ADD COLUMN exit_acked INTEGER NOT NULL DEFAULT 0`)
}

// ensureProcessColumns adds the harness process identity columns a journal written by an older supervisor lacks. They stay NULL on rows
// recorded before the identity existed: a NULL means the turn's process is unknown and is never signalled.
func ensureProcessColumns(ctx context.Context, db *sql.DB) error {
	if err := ensureColumn(ctx, db, "turn_start", "pid", `ALTER TABLE turn_start ADD COLUMN pid INTEGER`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "turn_start", "pgid", `ALTER TABLE turn_start ADD COLUMN pgid INTEGER`); err != nil {
		return err
	}
	return ensureColumn(ctx, db, "turn_start", "pid_start", `ALTER TABLE turn_start ADD COLUMN pid_start TEXT`)
}

// ensureColumn adds a column a journal written by an older supervisor lacks.
func ensureColumn(ctx context.Context, db *sql.DB, table, column, alter string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var index int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&index, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.ExecContext(ctx, alter)
	return err
}

func (j *Journal) Close() error {
	if !j.closed.CompareAndSwap(false, true) {
		return nil
	}
	dbErr := j.db.Close()
	unlockErr := syscall.Flock(int(j.lockFile.Fd()), syscall.LOCK_UN)
	return errors.Join(dbErr, unlockErr, j.lockFile.Close())
}

func (j *Journal) HasStart(ctx context.Context, turnID string) (bool, error) {
	var found int
	err := j.db.QueryRowContext(ctx, `SELECT 1 FROM turn_start WHERE turn_id=?`, turnID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type StartRecord struct {
	TurnID     string
	AgentID    string
	Generation int
	DesiredRev int64
	NoteIDs    []string
	Mode       string
	SpecJSON   json.RawMessage
}

func (j *Journal) TurnSpecJSON(ctx context.Context, turnID string) (json.RawMessage, error) {
	var raw []byte
	err := j.db.QueryRowContext(ctx, `SELECT spec_json FROM turn_start WHERE turn_id=?`, turnID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (j *Journal) RecordStart(ctx context.Context, record StartRecord) (already bool, err error) {
	notes, err := json.Marshal(record.NoteIDs)
	if err != nil {
		return false, err
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var generation int
	var desiredRev int64
	var agentID string
	var spec []byte
	err = tx.QueryRowContext(ctx, `SELECT agent_id,generation,desired_rev,spec_json FROM turn_start WHERE turn_id=?`, record.TurnID).Scan(&agentID, &generation, &desiredRev, &spec)
	if err == nil {
		// A settled turn whose spec was pruned (see PruneSettled) still dedupes a replayed start: the fields that stay must match.
		if generation != record.Generation || desiredRev != record.DesiredRev || agentID != record.AgentID || (len(spec) > 0 && !bytes.Equal(spec, record.SpecJSON)) {
			return false, ErrJournalConflict
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO turn_start(turn_id,agent_id,generation,desired_rev,note_ids_json,mode,spec_json,started_at) VALUES (?,?,?,?,?,?,?,?)`, record.TurnID, record.AgentID, record.Generation, record.DesiredRev, string(notes), record.Mode, []byte(record.SpecJSON), timestamp())
	if err != nil {
		return false, err
	}
	return false, tx.Commit()
}

func (j *Journal) RecordEvent(ctx context.Context, turnID, sourceID, kind, summary string, payload json.RawMessage) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldTurn, oldKind, oldSummary string
	var oldPayload []byte
	err = tx.QueryRowContext(ctx, `SELECT turn_id,kind,summary,payload_json FROM journal_event WHERE source_event_id=?`, sourceID).Scan(&oldTurn, &oldKind, &oldSummary, &oldPayload)
	if err == nil {
		if oldTurn != turnID || oldKind != kind || oldSummary != summary || !bytes.Equal(oldPayload, payload) {
			return ErrJournalConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_event(source_event_id,turn_id,kind,summary,payload_json,created_at) VALUES (?,?,?,?,?,?)`, sourceID, turnID, kind, summary, []byte(payload), timestamp())
	if err != nil {
		return err
	}
	return tx.Commit()
}

type NativeSessionEvent struct {
	TurnID, AgentID, SourceEventID, Handle string
	Generation                             int
}

// NativeSessionEvents replays durable thread handles for turns still in the
// hello inventory after a socket reconnect. The server accepts identical
// repeats, so no separate acknowledgement is needed.
func (j *Journal) NativeSessionEvents(ctx context.Context) ([]NativeSessionEvent, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT e.turn_id,t.agent_id,t.generation,e.source_event_id,e.payload_json
		FROM journal_event e JOIN turn_start t ON t.turn_id=e.turn_id
		WHERE e.kind=? AND (t.exec='running' OR (t.exec='exited' AND t.exit_acked=0)
			OR (EXISTS(SELECT 1 FROM answer a WHERE a.turn_id=t.turn_id)
				AND NOT EXISTS(SELECT 1 FROM receipt r WHERE r.turn_id=e.turn_id AND r.terminal=1)))
		ORDER BY e.created_at,e.source_event_id`, string(adapter.EventNativeSessionStarted))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []NativeSessionEvent{}
	for rows.Next() {
		var event NativeSessionEvent
		var payload []byte
		if err := rows.Scan(&event.TurnID, &event.AgentID, &event.Generation, &event.SourceEventID, &payload); err != nil {
			return nil, err
		}
		var body adapter.Event
		if err := json.Unmarshal(payload, &body); err != nil || body.NativeSessionHandle == "" {
			return nil, ErrJournalConflict
		}
		event.Handle = body.NativeSessionHandle
		events = append(events, event)
	}
	return events, rows.Err()
}

type PendingEvent struct {
	TurnID, AgentID string
	Generation      int
	Event           adapter.Event
}

// PendingUsageEvents lists journaled usage and compaction events whose turn
// the server has not yet acknowledged as exited. A live send can be lost with
// the socket; the server dedupes by source_event_id, and it processes a
// socket's frames in order, so its process acknowledgement proves every
// earlier event of that turn arrived.
func (j *Journal) PendingUsageEvents(ctx context.Context) ([]PendingEvent, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT e.turn_id,t.agent_id,t.generation,e.payload_json
		FROM journal_event e JOIN turn_start t ON t.turn_id=e.turn_id
		WHERE e.kind IN (?,?) AND (t.exec='running' OR t.exit_acked=0)
		ORDER BY e.created_at,e.source_event_id`, string(adapter.EventUsage), string(adapter.EventCompaction))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []PendingEvent{}
	for rows.Next() {
		var event PendingEvent
		var payload []byte
		if err := rows.Scan(&event.TurnID, &event.AgentID, &event.Generation, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &event.Event); err != nil {
			return nil, ErrJournalConflict
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (j *Journal) RecordExit(ctx context.Context, turnID string, exitCode int, terminalEvent, cancelIntent bool) error {
	return j.RecordExitInfo(ctx, turnID, adapter.ExitInfo{ExitCode: exitCode, TerminalEvent: terminalEvent, CancelIntent: cancelIntent})
}

func (j *Journal) RecordExitInfo(ctx context.Context, turnID string, exit adapter.ExitInfo) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE turn_start SET exec='exited',exit_code=?,terminal_event=?,cancel_intent=?,exit_acked=0 WHERE turn_id=? AND exec='running'`, exit.ExitCode, exit.TerminalEvent, exit.CancelIntent, turnID)
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
	if exit.SessionRecreated || !exit.TerminalEvent && (exit.StderrTail != "" || exit.ErrorEvent != "") {
		diagnostic := map[string]any{"stderr_tail": adapter.SanitizeStderr(exit.StderrTail), "error_event": adapter.SanitizeStderr(exit.ErrorEvent)}
		if exit.ErrorSummary != "" {
			diagnostic["error_summary"] = adapter.SanitizeStderr(exit.ErrorSummary)
		}
		if exit.DefiniteFailure {
			diagnostic["definite_failure"] = true
		}
		if exit.LimitStall {
			diagnostic["limit_stall"] = true
		}
		if exit.WorkTree != "" {
			diagnostic["work_tree"] = adapter.SanitizeStderr(exit.WorkTree)
		}
		if exit.Interrupted {
			diagnostic["interrupted"] = true
		}
		if exit.SessionRecreated {
			diagnostic["session_recreated"] = true
		}
		data, err := json.Marshal(diagnostic)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO journal_event(source_event_id,turn_id,kind,summary,payload_json,created_at) VALUES (?,?,?,?,?,?)`, turnID+":exit-diagnostic", turnID, "exit_diagnostic", "", data, timestamp()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TurnProcess is the harness process identity a turn's start recorded: the pid, the process group it leads, and the system's record of
// when the process started (so a later process that reused the pid is not mistaken for it). Pid 0 means the row carries no identity: a
// turn recorded by a supervisor that did not keep one, and its process is never signalled.
type TurnProcess struct {
	TurnID    string
	Pid, Pgid int
	Start     string
}

// RecordTurnProcess journals the harness process identity of a turn, right after its process started. A turn whose row is already gone
// (it ended while the identity was being read) records nothing and is not an error.
func (j *Journal) RecordTurnProcess(ctx context.Context, turnID string, pid, pgid int, start string) error {
	result, err := j.db.ExecContext(ctx, `UPDATE turn_start SET pid=?,pgid=?,pid_start=? WHERE turn_id=? AND exec='running'`, pid, pgid, start, turnID)
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
	return nil
}

// RunningTurnProcesses lists the journal's running turns with the process identity their start recorded, oldest first.
func (j *Journal) RunningTurnProcesses(ctx context.Context) ([]TurnProcess, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT turn_id,COALESCE(pid,0),COALESCE(pgid,0),COALESCE(pid_start,'') FROM turn_start WHERE exec='running' ORDER BY started_at,turn_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []TurnProcess{}
	for rows.Next() {
		var turn TurnProcess
		if err := rows.Scan(&turn.TurnID, &turn.Pid, &turn.Pgid, &turn.Start); err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

func (j *Journal) AcknowledgeExit(ctx context.Context, turnID string) error {
	result, err := j.db.ExecContext(ctx, `UPDATE turn_start SET exit_acked=1,settled_at=? WHERE turn_id=? AND exec='exited'`, timestamp(), turnID)
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
	return nil
}

// SettleRefusedExit marks the unacknowledged exit of one turn as settled when the server refuses that turn's frames with a code that
// says the turn is over for good. Two codes do: stale_generation (the server answers every frame of a superseded generation that way —
// the exit report, the turn's activity frames and its acks alike, and the Error does not say which frame it answers) and
// already_terminal (the server's turn is already terminal in the other state: the journal says exited where the server holds killed, or
// the reverse). Either way the exit report has nowhere to go and resending it would keep the journal open forever (the endless drain
// loop of A13-13), so the settle applies only to a row that is exited and not yet acknowledged, of that agent and turn. A turn that is
// still running keeps its normal course, and a turn with no such row settles nothing: the caller is told so with a nil error, never with
// sql.ErrNoRows, because a refused activity frame of a running turn is the protocol working and must not fail the read loop. The
// returned generation is the settled row's, for the one log line.
func (j *Journal) SettleRefusedExit(ctx context.Context, agentID, turnID string) (generation int, settled bool, err error) {
	err = j.db.QueryRowContext(ctx, `UPDATE turn_start SET exit_acked=1,settled_at=? WHERE turn_id=? AND agent_id=? AND exec='exited' AND exit_acked=0 RETURNING generation`, timestamp(), turnID, agentID).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return generation, true, nil
}

func (j *Journal) RecordAnswer(ctx context.Context, turnID string, answerRev int, digest string, candidate json.RawMessage) (already bool, err error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var oldDigest string
	err = tx.QueryRowContext(ctx, `SELECT digest FROM answer WHERE turn_id=? AND answer_rev=?`, turnID, answerRev).Scan(&oldDigest)
	if err == nil {
		if oldDigest != digest {
			return false, ErrJournalConflict
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO answer(turn_id,answer_rev,digest,candidate,created_at) VALUES (?,?,?,?,?)`, turnID, answerRev, digest, []byte(candidate), timestamp())
	if err != nil {
		return false, err
	}
	return false, tx.Commit()
}

func (j *Journal) RecordReceipt(ctx context.Context, receipt protocol.ReceiptBody) error {
	terminal := receipt.Result == "committed" || receipt.Result == "held" || receipt.Result == "superseded"
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var answerDigest string
	if err := tx.QueryRowContext(ctx, `SELECT digest FROM answer WHERE turn_id=? AND answer_rev=?`, receipt.TurnID, receipt.AnswerRev).Scan(&answerDigest); err != nil {
		return err
	}
	if answerDigest != receipt.Digest {
		return ErrJournalConflict
	}
	var oldDigest, oldResult string
	err = tx.QueryRowContext(ctx, `SELECT digest,result FROM receipt WHERE turn_id=? AND answer_rev=?`, receipt.TurnID, receipt.AnswerRev).Scan(&oldDigest, &oldResult)
	if err == nil {
		if oldDigest != receipt.Digest {
			return ErrJournalConflict
		}
		if oldResult == "correction_requested" && receipt.Result == "superseded" {
			if _, err := tx.ExecContext(ctx, `UPDATE receipt SET result='superseded',details=?,terminal=1 WHERE turn_id=? AND answer_rev=?`, []byte(receipt.Details), receipt.TurnID, receipt.AnswerRev); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE answer SET candidate=NULL WHERE turn_id=? AND answer_rev=?`, receipt.TurnID, receipt.AnswerRev); err != nil {
				return err
			}
			return tx.Commit()
		}
		if oldResult != receipt.Result {
			return ErrJournalConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO receipt(turn_id,answer_rev,digest,result,details,terminal,created_at) VALUES (?,?,?,?,?,?,?)`, receipt.TurnID, receipt.AnswerRev, receipt.Digest, receipt.Result, []byte(receipt.Details), terminal, timestamp())
	if err != nil {
		return err
	}
	if terminal {
		if _, err := tx.ExecContext(ctx, `UPDATE answer SET candidate=NULL WHERE turn_id=? AND answer_rev=?`, receipt.TurnID, receipt.AnswerRev); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OpenTurns lists the hello inventory: running turns, exits the server has
// not acknowledged, and answered turns still awaiting a terminal receipt. An
// acknowledged exit without an answer is settled: the server only resends
// receipts for answered turns and RecordReceipt needs an answer row, so such a
// turn could never leave the inventory. Keeping it would grow every hello and,
// once the agent moved to another machine, make this machine's hello fail its
// agent binding check forever.
func (j *Journal) OpenTurns(ctx context.Context) ([]protocol.HelloAgent, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT t.agent_id,t.generation,t.turn_id,t.exec,
		COALESCE(a.answer_rev,0),COALESCE(a.digest,''),a.turn_id IS NOT NULL
		FROM turn_start t LEFT JOIN answer a ON a.turn_id=t.turn_id
		WHERE t.exec='running' OR (t.exec='exited' AND t.exit_acked=0)
		   OR (a.turn_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM receipt r WHERE r.turn_id=t.turn_id AND r.terminal=1))
		ORDER BY t.agent_id,t.generation,t.started_at,t.turn_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []protocol.HelloAgent{}
	indices := map[string]int{}
	for rows.Next() {
		var agentID, turnID, exec, digest string
		var generation, answerRev int
		var hasAnswer bool
		if err := rows.Scan(&agentID, &generation, &turnID, &exec, &answerRev, &digest, &hasAnswer); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s/%d", agentID, generation)
		index, ok := indices[key]
		if !ok {
			index = len(agents)
			indices[key] = index
			agents = append(agents, protocol.HelloAgent{AgentID: agentID, Generation: generation, OpenTurns: []protocol.OpenTurn{}})
		}
		agents[index].OpenTurns = append(agents[index].OpenTurns, protocol.OpenTurn{TurnID: turnID, Exec: exec, HasAnswer: hasAnswer, AnswerRev: answerRev, Digest: digest})
	}
	return agents, rows.Err()
}

type PendingExit struct {
	TurnID, AgentID string
	Generation      int
	Exit            adapter.ExitInfo
}

func (j *Journal) PendingExits(ctx context.Context) ([]PendingExit, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT t.turn_id,t.agent_id,t.generation,t.exit_code,t.terminal_event,t.cancel_intent,
		COALESCE((SELECT json_extract(e.payload_json,'$.stderr_tail') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),''),
		COALESCE((SELECT json_extract(e.payload_json,'$.error_event') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),''),
		COALESCE((SELECT json_extract(e.payload_json,'$.error_summary') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),''),
		COALESCE((SELECT json_extract(e.payload_json,'$.definite_failure') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),0),
		COALESCE((SELECT json_extract(e.payload_json,'$.limit_stall') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),0),
		COALESCE((SELECT json_extract(e.payload_json,'$.interrupted') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),0),
		COALESCE((SELECT json_extract(e.payload_json,'$.session_recreated') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),0),
		COALESCE((SELECT json_extract(e.payload_json,'$.work_tree') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),'')
		FROM turn_start t WHERE t.exec='exited' AND t.exit_acked=0 ORDER BY t.started_at,t.turn_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exits := []PendingExit{}
	for rows.Next() {
		var exit PendingExit
		if err := rows.Scan(&exit.TurnID, &exit.AgentID, &exit.Generation, &exit.Exit.ExitCode, &exit.Exit.TerminalEvent, &exit.Exit.CancelIntent, &exit.Exit.StderrTail, &exit.Exit.ErrorEvent, &exit.Exit.ErrorSummary, &exit.Exit.DefiniteFailure, &exit.Exit.LimitStall, &exit.Exit.Interrupted, &exit.Exit.SessionRecreated, &exit.Exit.WorkTree); err != nil {
			return nil, err
		}
		exits = append(exits, exit)
	}
	return exits, rows.Err()
}

type PendingAnswer struct {
	TurnID, AgentID, Digest string
	Generation, AnswerRev   int
	Candidate               json.RawMessage
}

func (j *Journal) PendingAnswers(ctx context.Context) ([]PendingAnswer, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT a.turn_id,t.agent_id,t.generation,a.answer_rev,a.digest,a.candidate FROM answer a JOIN turn_start t ON t.turn_id=a.turn_id WHERE a.candidate IS NOT NULL ORDER BY a.created_at,a.turn_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	answers := []PendingAnswer{}
	for rows.Next() {
		var answer PendingAnswer
		if err := rows.Scan(&answer.TurnID, &answer.AgentID, &answer.Generation, &answer.AnswerRev, &answer.Digest, &answer.Candidate); err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	return answers, rows.Err()
}

func timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }

// CommandStartRecord is one long command this supervisor started for an agent, written and fsynced before its process exists.
type CommandStartRecord struct {
	JobID      string
	AgentID    string
	Generation int
	Pid        int
	PidStart   string
	Cmd        string
	LogPath    string
	StartedAt  string
	// TimeoutSeconds is the job's own timeout, so a supervisor that adopts it after a restart still enforces it (0 in a record written
	// before it was kept).
	TimeoutSeconds int
	// Container says the process is a Docker launcher running the command in the agent's container.
	Container bool
}

func (j *Journal) RecordCommandStart(ctx context.Context, record CommandStartRecord) (already bool, err error) {
	if record.StartedAt == "" {
		record.StartedAt = timestamp()
	}
	result, err := j.db.ExecContext(ctx, `INSERT OR IGNORE INTO command_start
		(job_id,agent_id,generation,pid,pid_start,cmd,log_path,started_at,timeout_seconds,container) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		record.JobID, record.AgentID, record.Generation, record.Pid, record.PidStart, record.Cmd, record.LogPath, record.StartedAt,
		record.TimeoutSeconds, record.Container)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// CommandStart reads the start record of one job (for a restart that must decide what became of it).
func (j *Journal) CommandStart(ctx context.Context, jobID string) (CommandStartRecord, error) {
	var record CommandStartRecord
	err := j.db.QueryRowContext(ctx, `SELECT job_id,agent_id,generation,pid,pid_start,cmd,log_path,started_at,timeout_seconds,container
		FROM command_start WHERE job_id=?`, jobID).Scan(&record.JobID, &record.AgentID, &record.Generation,
		&record.Pid, &record.PidStart, &record.Cmd, &record.LogPath, &record.StartedAt, &record.TimeoutSeconds, &record.Container)
	return record, err
}

// OpenCommandStarts are the start records of the jobs this journal has not seen end: what a restarted supervisor adopts.
func (j *Journal) OpenCommandStarts(ctx context.Context) ([]CommandStartRecord, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT s.job_id,s.agent_id,s.generation,s.pid,s.pid_start,s.cmd,s.log_path,s.started_at,s.timeout_seconds,
		s.container FROM command_start s LEFT JOIN command_exit e ON e.job_id=s.job_id WHERE e.job_id IS NULL ORDER BY s.started_at,s.job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []CommandStartRecord{}
	for rows.Next() {
		var record CommandStartRecord
		if err := rows.Scan(&record.JobID, &record.AgentID, &record.Generation, &record.Pid, &record.PidStart, &record.Cmd,
			&record.LogPath, &record.StartedAt, &record.TimeoutSeconds, &record.Container); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// HasCommandExit says whether a job's end is already journaled: nothing reports it a second time.
func (j *Journal) HasCommandExit(ctx context.Context, jobID string) (bool, error) {
	var found int
	err := j.db.QueryRowContext(ctx, `SELECT 1 FROM command_exit WHERE job_id=?`, jobID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RecordCommandExit writes how one command ended. It is replayed until the server acknowledges it.
func (j *Journal) RecordCommandExit(ctx context.Context, exit protocol.CommandExit) error {
	if exit.EndedAt == "" {
		exit.EndedAt = timestamp()
	}
	result, err := j.db.ExecContext(ctx, `INSERT OR REPLACE INTO command_exit
		(job_id,exit_code,timed_out,killed,unconfirmed,lost,started_at,ended_at,log_bytes,tail_truncated,tail,refused,reason,exit_acked)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0)`, exit.JobID, nullableInt(exit.ExitCode), exit.TimedOut, exit.Killed,
		exit.Unconfirmed, exit.Lost, exit.StartedAt, exit.EndedAt, exit.LogBytes, exit.TailTruncated, exit.Tail, exit.Refused, exit.Reason)
	if err != nil {
		return err
	}
	_, err = result.RowsAffected()
	return err
}

func (j *Journal) AcknowledgeCommandExit(ctx context.Context, jobID string) error {
	result, err := j.db.ExecContext(ctx, `UPDATE command_exit SET exit_acked=1 WHERE job_id=?`, jobID)
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
	return nil
}

// OpenCommands are the jobs whose process this journal started and has not seen end: what hello reports after a restart.
func (j *Journal) OpenCommands(ctx context.Context) ([]protocol.CommandOpen, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT s.job_id,s.agent_id,s.generation,s.pid,s.pid_start,s.log_path,s.started_at
		FROM command_start s LEFT JOIN command_exit e ON e.job_id=s.job_id WHERE e.job_id IS NULL ORDER BY s.started_at,s.job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	open := []protocol.CommandOpen{}
	for rows.Next() {
		var item protocol.CommandOpen
		if err := rows.Scan(&item.JobID, &item.AgentID, &item.Generation, &item.Pid, &item.PidStart, &item.LogPath, &item.StartedAt); err != nil {
			return nil, err
		}
		open = append(open, item)
	}
	return open, rows.Err()
}

// PendingCommandExits are the ends the server has not acknowledged yet; they are replayed with every connection.
func (j *Journal) PendingCommandExits(ctx context.Context) ([]protocol.CommandExit, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT e.job_id,s.agent_id,s.generation,e.exit_code,e.timed_out,e.killed,e.unconfirmed,e.lost,
		e.started_at,e.ended_at,e.log_bytes,e.tail_truncated,e.tail,e.refused,e.reason FROM command_exit e JOIN command_start s ON s.job_id=e.job_id
		WHERE e.exit_acked=0 ORDER BY e.ended_at,e.job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exits := []protocol.CommandExit{}
	for rows.Next() {
		var exit protocol.CommandExit
		var exitCode sql.NullInt64
		if err := rows.Scan(&exit.JobID, &exit.AgentID, &exit.Generation, &exitCode, &exit.TimedOut, &exit.Killed,
			&exit.Unconfirmed, &exit.Lost, &exit.StartedAt, &exit.EndedAt, &exit.LogBytes, &exit.TailTruncated, &exit.Tail, &exit.Refused,
			&exit.Reason); err != nil {
			return nil, err
		}
		if exitCode.Valid {
			code := int(exitCode.Int64)
			exit.ExitCode = &code
		}
		exits = append(exits, exit)
	}
	return exits, rows.Err()
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// WarmProcess is a warm harness process this supervisor started and has not yet seen end: Started is the process's own start time as the
// system reports it, so a pid that has since been reused is not mistaken for it.
type WarmProcess struct {
	PID     int
	AgentID string
	Started string
}

// RecordWarmProcess notes a warm process so that a supervisor that dies without ending it can find and end it at its next start.
func (j *Journal) RecordWarmProcess(ctx context.Context, process WarmProcess) error {
	_, err := j.db.ExecContext(ctx, `INSERT OR REPLACE INTO warm_process(pid,agent_id,started,recorded_at) VALUES (?,?,?,?)`, process.PID, process.AgentID, process.Started, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ForgetWarmProcess drops a warm process that has ended.
func (j *Journal) ForgetWarmProcess(ctx context.Context, pid int) error {
	_, err := j.db.ExecContext(ctx, `DELETE FROM warm_process WHERE pid=?`, pid)
	return err
}

// WarmProcesses lists the warm processes noted and not yet forgotten.
func (j *Journal) WarmProcesses(ctx context.Context) ([]WarmProcess, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT pid,agent_id,started FROM warm_process ORDER BY pid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WarmProcess
	for rows.Next() {
		var process WarmProcess
		if err := rows.Scan(&process.PID, &process.AgentID, &process.Started); err != nil {
			return nil, err
		}
		out = append(out, process)
	}
	return out, rows.Err()
}

// Remove closes the journal and deletes its files (the database, its write-ahead log and shared memory, and the lock file): what an instance that
// retired with nothing owed leaves behind is nothing. The lock goes last, so no other process can open the path while its files are being removed.
func (j *Journal) Remove() error {
	closeErr := j.Close()
	var errs []error
	for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
		if err := os.Remove(j.path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(append(errs, closeErr)...)
}
