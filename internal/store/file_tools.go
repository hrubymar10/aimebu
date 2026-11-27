package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

// The two helpers below open a SQLite file by path for the backup and restore commands. They live here because the store package owns the
// opening of aimebu databases (internal/testenv scans every other package for a second process opening one).

// VacuumFileInto writes a standalone snapshot of the SQLite file source to dest. source may be the live server database only while the
// caller holds the server's startup writer lock, which is what keeps a running server from having a second connection open on it: the
// backup command takes that lock first (cmd/aimebu/backup_restore.go) and asks a running server for its own snapshot instead.
func VacuumFileInto(ctx context.Context, source, dest string) error {
	db, err := sql.Open(SQLDriverName, sqliteutil.DSN(source, false))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(dest, `'`, `''`)+`'`)
	return err
}

// FileSchemaVersion reads the highest applied schema version of a SQLite file, read-only. It is for a private copy (a snapshot taken for an
// archive, or a database extracted from one), never for the live server database.
func FileSchemaVersion(ctx context.Context, path string) (int, error) {
	db, err := sql.Open(SQLDriverName, sqliteutil.DSN(path, true))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version int
	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version)
	return version, err
}
