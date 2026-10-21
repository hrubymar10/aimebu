package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/sqliteutil"
	sqlite "modernc.org/sqlite"
)

const migrationBackupKeep = 5

var migrationBackupName = regexp.MustCompile(`^aimebu-pre-migrate-[0-9]+-to-[0-9]+-([0-9]{8}T[0-9]{6}\.[0-9]{9}Z)\.sqlite$`)

// pendingMigrationState inspects the database before migrate creates its first
// table. A truly empty database needs no backup; a populated database without
// schema_version is still worth preserving before migration attempts to run.
func pendingMigrationState(ctx context.Context, db *sql.DB) (current int, existing bool, err error) {
	var tables int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil || tables == 0 {
		return 0, false, err
	}
	var hasVersion int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&hasVersion); err != nil {
		return 0, true, err
	}
	if hasVersion == 0 {
		return 0, true, nil
	}
	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current)
	return current, true, err
}

// backupBeforeMigration uses SQLite's online backup API, which includes
// committed WAL pages. It finishes and verifies the copy before migrate may
// write schema_version, and removes an incomplete copy on any failure.
func backupBeforeMigration(ctx context.Context, db *sql.DB, path string, from, to int, rotate func(string, string) error) (err error) {
	dir := filepath.Join(filepath.Dir(path), "backups")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe backup directory %s", dir)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	dest := filepath.Join(dir, fmt.Sprintf("aimebu-pre-migrate-%d-to-%d-%s.sqlite", from, to, stamp))
	partial := dest + ".partial"
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(partial)
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(partial)
			_ = os.Remove(partial + "-wal")
			_ = os.Remove(partial + "-shm")
			_ = os.Remove(dest)
			_ = os.Remove(dest + "-wal")
			_ = os.Remove(dest + "-shm")
		}
	}()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver has no online backup API")
		}
		backup, err := backuper.NewBackup(sqliteutil.DSN(partial, false))
		if err != nil {
			return err
		}
		for more := true; more; {
			if err = ctx.Err(); err != nil {
				break
			}
			more, err = backup.Step(256)
			if err != nil {
				break
			}
		}
		return errors.Join(err, backup.Finish())
	})
	if err != nil {
		return err
	}
	if err = os.Chmod(partial, 0600); err != nil {
		return err
	}
	if err = makeMigrationBackupStandalone(ctx, partial); err != nil {
		return err
	}
	if err = verifyMigrationBackup(ctx, partial); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, statErr := os.Lstat(partial + suffix); statErr == nil {
			return fmt.Errorf("backup still has a %s sidecar", suffix)
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	if err = syncMigrationBackup(partial, dir); err != nil {
		return err
	}
	if err = os.Link(partial, dest); err != nil {
		return err
	}
	if err = os.Remove(partial); err != nil {
		return err
	}
	if err = syncMigrationDirectory(dir); err != nil {
		return err
	}
	complete = true
	if err := rotate(dir, filepath.Base(dest)); err != nil {
		if multiple, ok := err.(interface{ Unwrap() []error }); ok {
			for _, failure := range multiple.Unwrap() {
				log.Printf("pre-migration backup rotation: %v; verified backup retained at %s", failure, dest)
			}
		} else {
			log.Printf("pre-migration backup rotation: %v; verified backup retained at %s", err, dest)
		}
	}
	return nil
}

// makeMigrationBackupStandalone checkpoints SQLite's copied WAL into the main
// file and closes the write connection before the file is verified or published.
func makeMigrationBackupStandalone(ctx context.Context, path string) error {
	copy, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		return err
	}
	var mode string
	err = copy.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode)
	err = errors.Join(err, copy.Close())
	if err != nil {
		return err
	}
	if mode != "delete" {
		return fmt.Errorf("backup journal mode remained %q", mode)
	}
	return nil
}

func verifyMigrationBackup(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, true))
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var result string
	if !rows.Next() {
		return errors.New("integrity_check returned no result")
	}
	if err := rows.Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check: %s", result)
	}
	if rows.Next() {
		return errors.New("integrity_check returned additional errors")
	}
	return rows.Err()
}

func syncMigrationBackup(path, dir string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	err = file.Sync()
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return syncMigrationDirectory(dir)
}

func syncMigrationDirectory(dir string) error {
	folder, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(folder.Sync(), folder.Close())
}

func rotateMigrationBackups(dir, fresh string) error {
	return rotateMigrationBackupsWithRemove(dir, fresh, os.Remove)
}

func rotateMigrationBackupsWithRemove(dir, fresh string, remove func(string) error) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var failures []error
	removeFile := func(path string, optional bool) bool {
		if err := remove(path); err != nil {
			if optional && os.IsNotExist(err) {
				return true
			}
			failures = append(failures, fmt.Errorf("remove %s: %w", path, err))
			return false
		}
		return true
	}
	type backupFile struct{ name, stamp string }
	var files []backupFile
	for _, entry := range entries {
		for _, suffix := range []string{"-wal", "-shm"} {
			if !strings.HasSuffix(entry.Name(), suffix) {
				continue
			}
			base := strings.TrimSuffix(entry.Name(), suffix)
			if !migrationBackupName.MatchString(base) && !migrationBackupName.MatchString(strings.TrimSuffix(base, ".partial")) {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, base)); os.IsNotExist(err) {
				removeFile(filepath.Join(dir, entry.Name()), true)
			} else if err != nil {
				return err
			}
		}
		match := migrationBackupName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if _, err := time.Parse("20060102T150405.000000000Z", match[1]); err != nil {
			continue
		}
		files = append(files, backupFile{entry.Name(), match[1]})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].stamp != files[j].stamp {
			return files[i].stamp > files[j].stamp
		}
		return strings.Compare(files[i].name, files[j].name) > 0
	})
	freshFound := false
	for _, file := range files {
		if file.name == fresh {
			freshFound = true
			break
		}
	}
	if !freshFound {
		return fmt.Errorf("fresh backup %s is missing during rotation", fresh)
	}
	kept := 0
	for _, file := range files {
		if file.name == fresh {
			continue
		}
		if kept < migrationBackupKeep-1 {
			kept++
			continue
		}
		if !removeFile(filepath.Join(dir, file.name), false) {
			continue
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			removeFile(filepath.Join(dir, file.name+suffix), true)
		}
	}
	return errors.Join(failures...)
}
