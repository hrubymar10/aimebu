package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// Scratch measurement probe for A11-1 point 6: counts committed write
// transactions at the driver level while a server idles with one connected
// machine at the production ping cadence. Never committed; never pointed at a
// real database.
var (
	probeSeq        atomic.Int64
	probeCommits    atomic.Int64
	probeStatements atomic.Pointer[sync.Map]
)

type probeDriver struct{ inner driver.Driver }

func (d probeDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &probeConn{Conn: conn}, nil
}

func probeWrite(query string) bool {
	trimmed := strings.TrimLeft(query, " \t\r\n;")
	for _, prefix := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE", "WITH"} {
		if len(trimmed) >= len(prefix) && strings.EqualFold(trimmed[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func probeNote(query string, c *probeConn) {
	if !probeWrite(query) {
		return
	}
	if statements := probeStatements.Load(); statements != nil {
		statements.Store(statementHead(query), true)
	}
	if c.inTx.Load() {
		c.txWrote.Store(true)
		return
	}
	probeCommits.Add(1)
}

func statementHead(query string) string {
	trimmed := strings.TrimLeft(query, " \t\r\n;")
	if len(trimmed) > 60 {
		trimmed = trimmed[:60]
	}
	return strings.ReplaceAll(trimmed, "\n", " ")
}

type probeConn struct {
	driver.Conn
	inTx    atomic.Bool
	txWrote atomic.Bool
}

func (c *probeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	probeNote(query, c)
	return rows, err
}

func (c *probeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	probeNote(query, c)
	return result, err
}

func (c *probeConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *probeConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.inTx.Store(true)
	c.txWrote.Store(false)
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		inner, err := beginner.BeginTx(ctx, options)
		return &probeTx{conn: c, inner: inner}, err
	}
	inner, err := c.Conn.Begin() //nolint:staticcheck
	return &probeTx{conn: c, inner: inner}, err
}

type probeTx struct {
	conn  *probeConn
	inner driver.Tx
}

func (t *probeTx) Commit() error {
	err := t.inner.Commit()
	if err == nil && t.conn.txWrote.Load() {
		probeCommits.Add(1)
	}
	t.conn.inTx.Store(false)
	t.conn.txWrote.Store(false)
	return err
}

func (t *probeTx) Rollback() error {
	err := t.inner.Rollback()
	t.conn.inTx.Store(false)
	t.conn.txWrote.Store(false)
	return err
}

func registerProbeDriver() string {
	name := fmt.Sprintf("aimebu-probe-%d", probeSeq.Add(1))
	sql.Register(name, probeDriver{inner: &sqlite.Driver{}})
	return name
}

func TestProbeIdleWriteCommitsPerMinute(t *testing.T) {
	f := newFixtureWith(t, store.Options{DriverName: registerProbeDriver()}, nil)
	f.seed()
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_probe','worker','Worker','worker','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_probe',?,?,?)`, f.project.ID, f.machineID, "/tmp/probe")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: "probe", ProjectID: f.project.ID, RoleID: "role_probe", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_probe", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_probe',?,0,'fresh_with_history','2026-11-24T00:00:00.000000000Z')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	client.PingInterval = 15 * time.Second
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(runCtx) }()
	select {
	case <-ready:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	// Warmup: startup sweeps, hello, the first round of pings.
	time.Sleep(20 * time.Second)
	probeCommits.Store(0)
	probeStatements.Store(&sync.Map{})
	walBefore := probeWalBytes(t, f.db.DBPath())
	time.Sleep(time.Minute)
	commits := probeCommits.Load()
	walAfter := probeWalBytes(t, f.db.DBPath())
	heads := []string{}
	probeStatements.Load().Range(func(key, _ any) bool {
		heads = append(heads, key.(string))
		return true
	})
	sort.Strings(heads)
	t.Logf("PROBE idle commits in 60s: %d; WAL grew %d bytes; write statements seen: %v", commits, walAfter-walBefore, heads)
	cancel()
	<-done
}

func probeWalBytes(t *testing.T, dbPath string) int64 {
	t.Helper()
	info, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}
