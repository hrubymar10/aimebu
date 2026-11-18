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
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	_ "modernc.org/sqlite"
)

var ErrJournalConflict = errors.New("journal conflict")
var ErrJournalInUse = errors.New("supervisor journal already in use")

type Journal struct {
	db       *sql.DB
	lockFile *os.File
	ID       string
	path     string
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
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			pidBytes, _ := os.ReadFile(path + ".lock")
			pid := strings.TrimSpace(string(pidBytes))
			if pid == "" {
				pid = "unknown"
			}
			return nil, fmt.Errorf("%w by pid %s; is the server's embedded supervisor running? set AIMEBU_EMBEDDED_SUPERVISOR=0 on the server", ErrJournalInUse, pid)
		}
		return nil, err
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
		exit_acked INTEGER NOT NULL DEFAULT 0);
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
		pid INTEGER PRIMARY KEY,agent_id TEXT NOT NULL,started TEXT NOT NULL,recorded_at TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureExitAckColumn(ctx, db); err != nil {
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
	owned = true
	return j, nil
}

func ensureExitAckColumn(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(turn_start)`)
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
		if name == "exit_acked" {
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
	_, err = db.ExecContext(ctx, `ALTER TABLE turn_start ADD COLUMN exit_acked INTEGER NOT NULL DEFAULT 0`)
	return err
}

func (j *Journal) Close() error {
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
		if generation != record.Generation || desiredRev != record.DesiredRev || agentID != record.AgentID || !bytes.Equal(spec, record.SpecJSON) {
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
	if !exit.TerminalEvent && (exit.StderrTail != "" || exit.ErrorEvent != "") {
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

func (j *Journal) AcknowledgeExit(ctx context.Context, turnID string) error {
	result, err := j.db.ExecContext(ctx, `UPDATE turn_start SET exit_acked=1 WHERE turn_id=? AND exec='exited'`, turnID)
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
		COALESCE((SELECT json_extract(e.payload_json,'$.limit_stall') FROM journal_event e WHERE e.turn_id=t.turn_id AND e.kind='exit_diagnostic' LIMIT 1),0)
		FROM turn_start t WHERE t.exec='exited' AND t.exit_acked=0 ORDER BY t.started_at,t.turn_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exits := []PendingExit{}
	for rows.Next() {
		var exit PendingExit
		if err := rows.Scan(&exit.TurnID, &exit.AgentID, &exit.Generation, &exit.Exit.ExitCode, &exit.Exit.TerminalEvent, &exit.Exit.CancelIntent, &exit.Exit.StderrTail, &exit.Exit.ErrorEvent, &exit.Exit.ErrorSummary, &exit.Exit.DefiniteFailure, &exit.Exit.LimitStall); err != nil {
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
