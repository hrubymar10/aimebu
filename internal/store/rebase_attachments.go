package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

// RebaseSnapshotAttachments changes only attachment file paths in a verified
// extracted database. It refuses any path outside the manifest's source root.
func RebaseSnapshotAttachments(ctx context.Context, snapshot, sourceRoot, targetRoot string) error {
	if !filepath.IsAbs(sourceRoot) || !filepath.IsAbs(targetRoot) || filepath.Clean(sourceRoot) != sourceRoot {
		return fmt.Errorf("invalid backup source or target root")
	}
	db, err := sql.Open(SQLDriverName, sqliteutil.DSN(snapshot, false))
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,path FROM attachment`)
	if err != nil {
		return err
	}
	type change struct{ id, path string }
	var changes []change
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return err
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil || !filepath.IsAbs(path) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			rows.Close()
			return fmt.Errorf("attachment %s has path outside backup source_root: %s", id, path)
		}
		changes = append(changes, change{id, filepath.Join(targetRoot, rel)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range changes {
		if _, err := tx.ExecContext(ctx, `UPDATE attachment SET path=? WHERE id=?`, item.path, item.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
