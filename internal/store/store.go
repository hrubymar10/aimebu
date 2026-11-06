package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
)

const (
	defaultMaxDepth = 3
	hardMaxDepth    = 5
)

var ErrDepthExceeded = errors.New("depth_exceeded")
var ErrTxReentry = errors.New("store transaction re-entry")

type txContextKey struct{}

type Options struct {
	MaxRoomDepth int
}

type Store struct {
	db            *sql.DB
	readDB        *sql.DB
	maxDepth      int
	attachmentDir string
	dbPath        string
	// turnGate is read-held by every transaction and write-held by HoldTurns, so no turn can become pending or
	// running between the switcher's quiescence check and the credential swap it protects.
	turnGate sync.RWMutex
	// turnGateWait bounds how long a transaction waits for the gate; zero means defaultTurnGateWait.
	turnGateWait time.Duration
	// holds is the limit guard's per-tool hold on new turns (see tool_hold.go).
	holdMu sync.RWMutex
	holds  map[string]string
}

// defaultTurnGateWait is far longer than the file swap HoldTurns exists for (a few seconds at worst, a busy Claude
// storage lock included), so reaching it means the holder broke its rule and is waiting on the store itself.
const defaultTurnGateWait = 30 * time.Second

// ErrTurnGateHeld: a transaction waited out turnGateWait for HoldTurns' holder. It is returned instead of hanging the store
// forever when the holder opens a transaction itself.
var ErrTurnGateHeld = errors.New("store: transactions are held off by a credential swap that did not finish")

// enterTurnGate read-holds the gate for one transaction, giving up with ErrTurnGateHeld (or the context's error) rather
// than waiting for ever. The caller releases it with turnGate.RUnlock.
func (s *Store) enterTurnGate(ctx context.Context) error {
	if s.turnGate.TryRLock() {
		return nil
	}
	wait := s.turnGateWait
	if wait <= 0 {
		wait = defaultTurnGateWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(2 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrTurnGateHeld
		case <-poll.C:
			if s.turnGate.TryRLock() {
				return nil
			}
		}
	}
}

// HoldTurns blocks every new transaction (so no turn can be recorded or start running) until the returned release is
// called. Transactions already open finish first. The holder must not open a transaction itself, and should keep the hold
// to file I/O: the whole store waits behind it. A holder that does open one is not left hanging: every transaction waits
// at most turnGateWait for the gate and then fails with ErrTurnGateHeld, which also frees the holder's own.
func (s *Store) HoldTurns() (release func()) {
	s.turnGate.Lock()
	return s.turnGate.Unlock
}

// Open creates or opens a store, applies forward-only migrations, and enables
// SQLite's write-ahead log, foreign keys, and a five-second busy timeout.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	depth := opts.MaxRoomDepth
	if depth == 0 {
		depth = defaultMaxDepth
	}
	if depth < 1 || depth > hardMaxDepth {
		return nil, fmt.Errorf("max room depth must be between 1 and %d", hardMaxDepth)
	}
	// The writer and the read-only pool must open the same file: an in-memory database is private to each connection, and a file: URI
	// would be wrapped a second time by the read-only DSN. Only a plain file path is accepted.
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("store path %q must be a file path", path)
	}
	// The read-only DSN is a file: URI, which reads the first segment of a relative path as its authority, so the path is made absolute once, here.
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = abs
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	// One connection serializes write transactions. Independent read-only
	// connections keep long reads from blocking a write's connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := openStoreWithBusyRetry(ctx, db, path); err != nil {
		_ = db.Close()
		return nil, err
	}
	readDB, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4)
	if err := readDB.PingContext(ctx); err != nil {
		_ = readDB.Close()
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, readDB: readDB, maxDepth: depth, attachmentDir: filepath.Join(filepath.Dir(path), "attachments"), dbPath: path}, nil
}

// Another process may be opening or checkpointing the same WAL database while
// this server starts. Retry only SQLite lock contention; a genuine migration
// error must still fail immediately.
func openStoreWithBusyRetry(ctx context.Context, db *sql.DB, path string) error {
	deadline := time.Now().Add(15 * time.Second)
	backedUp := false
	for {
		err := db.PingContext(ctx)
		if err == nil {
			var current int
			var existing bool
			current, existing, err = pendingMigrationState(ctx, db)
			if err == nil && existing && current < LatestSchemaVersion() && !backedUp {
				if err = backupBeforeMigration(ctx, db, path, current, LatestSchemaVersion(), rotateMigrationBackups); err != nil {
					err = fmt.Errorf("pre-migration backup: %w", err)
				} else {
					backedUp = true
				}
			}
		}
		if err == nil {
			err = migrate(ctx, db)
		}
		if err == nil {
			return nil
		}
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != 5 && sqliteErr.Code()&0xff != 6 || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Store) Close() error {
	readErr := s.readDB.Close()
	writeErr := s.db.Close()
	if readErr != nil {
		return readErr
	}
	return writeErr
}

func readOnlyDSN(path string) string {
	query := url.Values{}
	query.Add("mode", "ro")
	query.Add("_pragma", "query_only(ON)")
	query.Add("_pragma", "busy_timeout(5000)")
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

func sqliteDSN(path string) string {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "journal_mode(WAL)")
	// With WAL, NORMAL still cannot corrupt the database; it syncs the log at checkpoints instead of on every commit, so a power loss or
	// operating-system crash (not an aimebu crash) can lose the last few commits. That is the trade for much cheaper writes.
	query.Add("_pragma", "synchronous(NORMAL)")
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + query.Encode()
}

func (s *Store) checkTxReentry(ctx context.Context) error {
	if ctx.Value(txContextKey{}) == s {
		return ErrTxReentry
	}
	return nil
}

// Tx runs a change and its event writes in one database transaction. Inside
// fn, use Tx methods; if a Store method is needed, pass tx.Context() so it
// fails fast with ErrTxReentry instead of waiting on this connection.
func (s *Store) Tx(ctx context.Context, fn func(*Tx) error) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	if err := s.enterTurnGate(ctx); err != nil {
		return err
	}
	defer s.turnGate.RUnlock()
	markedContext := context.WithValue(ctx, txContextKey{}, s)
	inner, err := s.db.BeginTx(markedContext, nil)
	if err != nil {
		return err
	}
	defer inner.Rollback()
	tx := &Tx{tx: inner, ctx: markedContext, maxDepth: s.maxDepth}
	if err := fn(tx); err != nil {
		return err
	}
	return inner.Commit()
}

type Tx struct {
	tx       *sql.Tx
	ctx      context.Context
	maxDepth int
	// scopeWorktrees are the worktrees made ahead of a bulk resume for agents that would find their folder's writer slot taken (see
	// ScopeResumeWriters); the resume moves such an agent to its worktree instead of failing.
	scopeWorktrees map[string]AgentWorktree
}

// UseScopeWorktrees hands a bulk control the worktrees of the agents it may find blocked.
func (tx *Tx) UseScopeWorktrees(worktrees map[string]AgentWorktree) { tx.scopeWorktrees = worktrees }

// Context carries the active transaction marker for fail-fast Store calls.
func (tx *Tx) Context() context.Context { return tx.ctx }

// Exec exposes SQL inside the managed transaction for later store accessors.
func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(tx.ctx, query, args...)
}

func (tx *Tx) nextSeq() (int64, error) {
	var seq int64
	err := tx.tx.QueryRowContext(tx.ctx,
		`UPDATE seq_counter SET value = value + 1
		 WHERE id = 1 RETURNING value`).Scan(&seq)
	return seq, err
}

type Event struct {
	ID          string `json:"id"`
	Seq         int64  `json:"seq"`
	Kind        string `json:"kind"`
	ProjectID   string `json:"project_id,omitempty"`
	RoomID      string `json:"room_id,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	TurnID      string `json:"turn_id,omitempty"`
	PayloadJSON string `json:"payload_json"`
	CreatedAt   string `json:"created_at"`
}

func (tx *Tx) AppendEvent(event Event) (Event, error) {
	var err error
	event.ID, err = newID("evt")
	if err != nil {
		return Event{}, err
	}
	event.Seq, err = tx.nextSeq()
	if err != nil {
		return Event{}, err
	}
	if event.PayloadJSON == "" {
		event.PayloadJSON = "{}"
	}
	event.CreatedAt = timestamp()
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO event
		(id, seq, kind, project_id, room_id, agent_id, turn_id, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.Seq, event.Kind, nullable(event.ProjectID), nullable(event.RoomID),
		nullable(event.AgentID), nullable(event.TurnID), event.PayloadJSON, event.CreatedAt)
	if err != nil {
		return Event{}, err
	}
	if event.TurnID != "" {
		switch event.Kind {
		case "turn_process_exit", "turn_lost", "turn_ack_accepted", "answer_committed", "answer_held":
			if err := tx.refreshRecoveryItemsAfterEvidence(event); err != nil {
				return Event{}, err
			}
		}
	}
	return event, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
