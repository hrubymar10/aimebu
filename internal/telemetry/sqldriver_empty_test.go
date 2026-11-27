package telemetry

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"

	"modernc.org/sqlite"
)

func TestTracedDriverExecWithoutResult(t *testing.T) {
	for _, query := range []string{"-- migration implemented in Go", ""} {
		t.Run(query, func(t *testing.T) {
			db, err := sql.Open(RegisterSQLDriver(&sqlite.Driver{}), ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.ExecContext(context.Background(), query); err != nil {
				t.Fatalf("exec %q: %v", query, err)
			}
		})
	}
}

type nilResultStmt struct{}

func (nilResultStmt) Close() error                               { return nil }
func (nilResultStmt) NumInput() int                              { return 0 }
func (nilResultStmt) Exec([]driver.Value) (driver.Result, error) { return nil, nil }
func (nilResultStmt) Query([]driver.Value) (driver.Rows, error)  { return nil, nil }

func TestTracedStatementExecWithoutResult(t *testing.T) {
	stmt := tracedStmt{inner: nilResultStmt{}, query: "-- no SQL"}
	if _, err := stmt.ExecContext(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}
