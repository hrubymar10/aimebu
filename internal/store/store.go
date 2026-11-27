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
	"sync/atomic"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	sqlite "modernc.org/sqlite"
)

const (
	defaultMaxDepth = 3
	hardMaxDepth    = 5
)

// SQLDriverName is the database/sql driver the store opens its databases with: the plain SQLite driver, or a traced copy of it that main
// registers when telemetry is on (internal/telemetry.SQLDriver). Set it before Open.
var SQLDriverName = "sqlite"

var ErrDepthExceeded = errors.New("depth_exceeded")
var ErrTxReentry = errors.New("store transaction re-entry")

type txContextKey struct{}

// DispatchPassContextKey marks the context a dispatcher pass runs under. A
// change the pass itself commits must not wake the pass again: the pass sees
// its own writes, and waking on them would loop passes back to back forever.
type DispatchPassContextKey struct{}

type Options struct {
	MaxRoomDepth int
	// DriverName is the database/sql driver to open the databases with; empty is SQLDriverName. A test passes a counting copy of the driver.
	DriverName string
}

type Store struct {
	db            *sql.DB
	readDB        *sql.DB
	maxDepth      int
	attachmentDir string
	dbPath        string
	// txBarrier is read-held by every transaction. HoldDispatch write-locks and releases it at once, which waits for the
	// transactions that are open and makes the next ones see the hold; nothing holds it for longer.
	txBarrier sync.RWMutex
	// holds is the limit guard's per-tool hold on new turns (see tool_hold.go).
	holdMu sync.RWMutex
	holds  map[string]string
	// switchHolds counts the account switches in progress per tool: while one runs no new turn of the tool is dispatched on the host (see HoldDispatch).
	switchHolds map[string]int
	// hostLogin names the login of a tool live on the host machine now (see SetHostLogin); nil or "" is unknown.
	hostLogin func(tool string) string
	// drift caches how far a room worktree is behind its base ref (see base_drift.go).
	drift baseDriftCache
	// wakeMu guards wakeFn, the dispatcher wake installed once at server start.
	wakeMu sync.Mutex
	wakeFn func()
	// flowMigrationBusy caches whether a team-flow change or its interrupted
	// agents still need dispatcher passes. It is set when a change starts, is
	// cancelled, or fails, and cleared by AdvanceTeamFlowMigrations once
	// nothing is left, so an idle server skips the step entirely.
	flowMigrationBusy atomic.Bool
	// flowMigrationAdvances counts AdvanceTeamFlowMigrations runs, for tests
	// that show an idle server leaves the step alone.
	flowMigrationAdvances atomic.Int64
	// writeCommits counts committed write transactions, for tests that show an
	// idle server commits none. A transaction counts when it changed rows (see
	// Tx.wrote); a read-only transaction does not.
	writeCommits atomic.Int64
	// snapshotBeforeCopy is a test seam after the snapshot connection is reserved.
	snapshotBeforeCopy func(*sql.DB)
}

func dataRootForDB(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "server" {
		return filepath.Dir(dir)
	}
	return dir
}

// DoctorReadDB is the store-owned read pool used by the authenticated doctor
// route. Callers must not close it.
func (s *Store) DoctorReadDB() *sql.DB { return s.readDB }

// OpenReadOnly opens an existing store file without migrations. Callers must
// first prove the server writer lock is free when they run in another process.
// With the lock free and no write-ahead log left behind by a crash, nothing can
// change the file under the reader, so it opens immutable: SQLite then creates
// no -shm or -wal file next to a database that a clean shutdown left without
// them. A leftover log is read through a plain read-only connection instead, so
// its pages are not ignored.
func OpenReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open(SQLDriverName, sqliteutil.ImmutableWhenNoWAL(readOnlyDSN(path), path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// WriteCommits says how many write transactions this store has committed since
// it opened. Tests snapshot it around an idle stretch and expect no movement.
func (s *Store) WriteCommits() int64 {
	return s.writeCommits.Load()
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
	driverName := opts.DriverName
	if driverName == "" {
		driverName = SQLDriverName
	}
	db, err := sql.Open(driverName, sqliteDSN(path))
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
	readDB, err := sql.Open(driverName, readOnlyDSN(path))
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
	s := &Store{db: db, readDB: readDB, maxDepth: depth, attachmentDir: filepath.Join(filepath.Dir(path), "attachments"), dbPath: path}
	// A crash can leave a team-flow change (or an agent it interrupted) waiting
	// for a pass; one read here seeds the cache so the first passes do the
	// step, and an idle server skips it once nothing is left.
	var busy bool
	if err := readDB.QueryRowContext(ctx, flowMigrationBusySQL).Scan(&busy); err != nil {
		_ = readDB.Close()
		_ = db.Close()
		return nil, err
	}
	s.flowMigrationBusy.Store(busy)
	return s, nil
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

// Snapshot writes a transactionally consistent copy through a dedicated
// connection, leaving the single writer connection free. SQLite refuses
// VACUUM INTO on the query-only read pool even though it does not modify the
// source. Callers must supply a new path outside the live database.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	copyDB, err := sql.Open(SQLDriverName, sqliteDSN(s.dbPath))
	if err != nil {
		return err
	}
	defer copyDB.Close()
	copyDB.SetMaxOpenConns(1)
	conn, err := copyDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if s.snapshotBeforeCopy != nil {
		s.snapshotBeforeCopy(copyDB)
	}
	_, err = conn.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(path, `'`, `''`)+`'`)
	return err
}

func (s *Store) DatabasePath() string { return s.dbPath }

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
	s.txBarrier.RLock()
	defer s.txBarrier.RUnlock()
	markedContext := context.WithValue(ctx, txContextKey{}, s)
	inner, err := s.db.BeginTx(markedContext, nil)
	if err != nil {
		return err
	}
	defer inner.Rollback()
	tx := &Tx{store: s, tx: inner, ctx: markedContext, maxDepth: s.maxDepth}
	if err := fn(tx); err != nil {
		return err
	}
	if err := inner.Commit(); err != nil {
		return err
	}
	// A committed change that can create dispatch work wakes the dispatcher
	// pass at once, so delivery does not wait for the fallback tick. The pass
	// itself is not woken by its own writes (DispatchPassContextKey).
	if tx.wrote {
		s.writeCommits.Add(1)
		s.wakeDispatch(markedContext)
	}
	return nil
}

type Tx struct {
	store    *Store
	tx       *sql.Tx
	ctx      context.Context
	maxDepth int
	wrote    bool
	// switchTrusted skips the owner/operator check of a harness switch: the server begins it on the strength of a decision already authorised.
	switchTrusted bool
}

// Context carries the active transaction marker for fail-fast Store calls.
func (tx *Tx) Context() context.Context { return tx.ctx }

// Exec exposes SQL inside the managed transaction for later store accessors.
func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	result, err := tx.tx.ExecContext(tx.ctx, query, args...)
	if err == nil && writesData(query) {
		tx.wrote = true
	}
	return result, err
}

// writesData reports whether a statement changes rows (not a read, a
// savepoint, or a pragma): a transaction holding one can create dispatch work
// when it commits. A WITH header may open a data-changing statement.
func writesData(query string) bool {
	trimmed := strings.TrimLeft(query, " \t\r\n;")
	for _, prefix := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE", "WITH"} {
		if len(trimmed) >= len(prefix) && strings.EqualFold(trimmed[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// SetDispatchWake installs the callback the store calls once per committed
// change that can create dispatch work, so the dispatcher pass runs without
// waiting for its fallback tick. The callback must never block; the store
// calls it at most one per commit and drops extra wakes (the dispatcher
// coalesces them itself). One dispatcher per store: a later install replaces
// an earlier one.
func (s *Store) SetDispatchWake(wake func()) {
	s.wakeMu.Lock()
	s.wakeFn = wake
	s.wakeMu.Unlock()
}

// wakeDispatch calls the installed wake for a change committed outside a
// dispatcher pass. Passes carry DispatchPassContextKey and are not woken by
// their own writes.
func (s *Store) wakeDispatch(ctx context.Context) {
	if ctx.Value(DispatchPassContextKey{}) != nil {
		return
	}
	s.wakeMu.Lock()
	wake := s.wakeFn
	s.wakeMu.Unlock()
	if wake != nil {
		wake()
	}
}

func (tx *Tx) nextSeq() (int64, error) {
	var seq int64
	err := tx.tx.QueryRowContext(tx.ctx,
		`UPDATE seq_counter SET value = value + 1
		 WHERE id = 1 RETURNING value`).Scan(&seq)
	if err == nil {
		tx.wrote = true
	}
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
	// Names says who the ids of the event are (its agent, and the users its payload names: user_id, by), for the feed that shows the event to a person;
	// only NameEvents fills it, and an event that is stored or compared has none. Agents are slug@project (a removed agent keeps the name it had), users their login.
	Names map[string]string `json:"names,omitempty"`
	// Notable is set on the events of the system-events feed that the bell shows (IsNotableSystemEvent); the web shows exactly those and keeps no list of its own.
	Notable bool `json:"notable,omitempty"`
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
	if _, err = tx.Exec(`INSERT INTO event
		(id, seq, kind, project_id, room_id, agent_id, turn_id, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.Seq, event.Kind, nullable(event.ProjectID), nullable(event.RoomID),
		nullable(event.AgentID), nullable(event.TurnID), event.PayloadJSON, event.CreatedAt); err != nil {
		return Event{}, err
	}
	if event.TurnID != "" {
		switch event.Kind {
		case "turn_process_exit", "turn_finished", "turn_lost", "turn_ack_accepted", "answer_committed", "answer_held":
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
