package telemetry

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"modernc.org/sqlite"
)

var driverSeq atomic.Int64

// RegisterSQLDriver registers a copy of inner whose queries are spans and returns the name to pass to sql.Open. It is registered whether or
// not reporting is on, because Settings can turn it on later: with it off a statement only costs one atomic load. A span is made only
// inside a request that is already being traced (a query outside one, such as startup or the sweeps, costs nothing), and it records the SQL
// text with its placeholders, the duration and the row count; never an argument value.
func RegisterSQLDriver(inner driver.Driver) string {
	name := fmt.Sprintf("aimebu-traced-%d", driverSeq.Add(1))
	sql.Register(name, &tracedDriver{inner: inner})
	return name
}

type tracedDriver struct{ inner driver.Driver }

func (d *tracedDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &tracedConn{inner: conn}, nil
}

// MaxDBSpans is how many statements of one request get a span of their own. A request that runs more (a list that asks per row) still
// reports them: the transaction carries the number of statements and how many were left out (db.calls, db.calls_dropped), so a hot loop shows up without
// every iteration being a span (the free Sentry plan counts spans).
const MaxDBSpans = 50

// spanBudget counts the statements of one request; the middleware puts it in the request's context.
type spanBudget struct{ calls, dropped atomic.Int64 }

type spanBudgetKey struct{}

// start begins a span for a statement when ctx is inside a recording span and the request still has span budget left.
func start(ctx context.Context, query string) (context.Context, trace.Span) {
	t := Current()
	if t == nil || !trace.SpanFromContext(ctx).IsRecording() {
		return ctx, nil
	}
	if budget, ok := ctx.Value(spanBudgetKey{}).(*spanBudget); ok && budget.calls.Add(1) > MaxDBSpans {
		budget.dropped.Add(1)
		return ctx, nil
	}
	// The span's name is what Sentry shows as its description: the statement itself, so a slow or repeated query can be told from the others.
	ctx, span := t.tracer.Start(ctx, NormalizeSQL(query), trace.WithAttributes(attribute.String(OpKey, "db.query"), attribute.String("db.system", "sqlite"), attribute.String("db.statement", query)))
	return ctx, span
}

// maxSQLDescription caps a span description; a longer statement is cut with an ellipsis.
const maxSQLDescription = 1000

var (
	sqlStringLiteral = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlNumberLiteral = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	sqlSpace         = regexp.MustCompile(`\s+`)
)

// NormalizeSQL is a statement as a span description: white space collapsed to single spaces, and any literal written into the text (a quoted
// string, a number) replaced by ? so a value is never part of it. Values bound as arguments are never in the text to begin with: it keeps its
// placeholders. A statement longer than maxSQLDescription is cut.
func NormalizeSQL(query string) string {
	text := sqlStringLiteral.ReplaceAllString(query, "?")
	text = sqlNumberLiteral.ReplaceAllString(text, "?")
	text = strings.TrimSpace(sqlSpace.ReplaceAllString(text, " "))
	if len(text) > maxSQLDescription {
		cut := maxSQLDescription
		for cut > 0 && text[cut]&0xC0 == 0x80 { // not in the middle of a character
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

func finish(span trace.Span, rows int64, err error) {
	if span == nil {
		return
	}
	if rows >= 0 {
		span.SetAttributes(attribute.Int64("db.rows", rows))
	}
	if err != nil && err != io.EOF {
		span.RecordError(err) // the span's status carries it; the scrubber drops the event detail of a span
	}
	span.End()
}

type tracedConn struct{ inner driver.Conn }

func (c *tracedConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &tracedStmt{inner: stmt, query: query}, nil
}

func (c *tracedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if preparer, ok := c.inner.(driver.ConnPrepareContext); ok {
		stmt, err := preparer.PrepareContext(ctx, query)
		if err != nil {
			return nil, err
		}
		return &tracedStmt{inner: stmt, query: query}, nil
	}
	return c.Prepare(query)
}

func (c *tracedConn) Close() error { return c.inner.Close() }

func (c *tracedConn) Begin() (driver.Tx, error) { return c.inner.Begin() } //nolint:staticcheck // part of driver.Conn

func (c *tracedConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.inner.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, options)
	}
	return c.inner.Begin() //nolint:staticcheck
}

func (c *tracedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	ctx, span := start(ctx, query)
	result, err := execer.ExecContext(ctx, query, args)
	if err == driver.ErrSkip {
		if span != nil {
			span.End() // database/sql retries through Prepare, which makes its own span
		}
		return nil, err
	}
	rows := int64(-1)
	if err == nil {
		if affected, rowsErr := result.RowsAffected(); rowsErr == nil {
			rows = affected
		}
	}
	finish(span, rows, err)
	return result, err
}

func (c *tracedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	ctx, span := start(ctx, query)
	rows, err := queryer.QueryContext(ctx, query, args)
	if err == driver.ErrSkip {
		if span != nil {
			span.End()
		}
		return nil, err
	}
	if err != nil {
		finish(span, -1, err)
		return nil, err
	}
	return wrapRows(rows, span), nil
}

func (c *tracedConn) Ping(ctx context.Context) error {
	if pinger, ok := c.inner.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *tracedConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.inner.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *tracedConn) IsValid() bool {
	if validator, ok := c.inner.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *tracedConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.inner.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

// NewBackup passes the SQLite online-backup call of the underlying connection through (the migration backup reaches it with conn.Raw).
func (c *tracedConn) NewBackup(dest string) (*sqlite.Backup, error) {
	backuper, ok := c.inner.(interface {
		NewBackup(string) (*sqlite.Backup, error)
	})
	if !ok {
		return nil, errors.New("SQLite driver has no online backup API")
	}
	return backuper.NewBackup(dest)
}

type tracedStmt struct {
	inner driver.Stmt
	query string
}

func (s *tracedStmt) Close() error  { return s.inner.Close() }
func (s *tracedStmt) NumInput() int { return s.inner.NumInput() }

func (s *tracedStmt) Exec(args []driver.Value) (driver.Result, error) { //nolint:staticcheck
	return s.inner.Exec(args) //nolint:staticcheck
}

func (s *tracedStmt) Query(args []driver.Value) (driver.Rows, error) { //nolint:staticcheck
	return s.inner.Query(args) //nolint:staticcheck
}

func plain(args []driver.NamedValue) []driver.Value {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	return values
}

func (s *tracedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	ctx, span := start(ctx, s.query)
	var result driver.Result
	var err error
	if execer, ok := s.inner.(driver.StmtExecContext); ok {
		result, err = execer.ExecContext(ctx, args)
	} else {
		result, err = s.inner.Exec(plain(args)) //nolint:staticcheck
	}
	rows := int64(-1)
	if err == nil {
		if affected, rowsErr := result.RowsAffected(); rowsErr == nil {
			rows = affected
		}
	}
	finish(span, rows, err)
	return result, err
}

func (s *tracedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	ctx, span := start(ctx, s.query)
	var rows driver.Rows
	var err error
	if queryer, ok := s.inner.(driver.StmtQueryContext); ok {
		rows, err = queryer.QueryContext(ctx, args)
	} else {
		rows, err = s.inner.Query(plain(args)) //nolint:staticcheck
	}
	if err != nil {
		finish(span, -1, err)
		return nil, err
	}
	return wrapRows(rows, span), nil
}

func (s *tracedStmt) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := s.inner.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

// tracedRows counts the rows read and ends the span when the rows are closed.
type tracedRows struct {
	driver.Rows
	span  trace.Span
	count int64
	done  bool
}

func wrapRows(rows driver.Rows, span trace.Span) driver.Rows {
	if span == nil {
		return rows
	}
	return &tracedRows{Rows: rows, span: span}
}

func (r *tracedRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.count++
	}
	return err
}

func (r *tracedRows) Close() error {
	err := r.Rows.Close()
	if !r.done {
		r.done = true
		finish(r.span, r.count, err)
	}
	return err
}
