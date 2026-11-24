package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func databaseBeforeLatestMigration(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := databaseAtVersion(t, path, LatestSchemaVersion()-1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE backup_probe (value TEXT); INSERT INTO backup_probe(value) VALUES ('committed WAL data')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func migrationBackupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if migrationBackupName.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestOpenBacksUpPendingMigrationAndIncludesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server", "aimebu.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	old := databaseBeforeLatestMigration(t, path)
	opened, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	_ = old
	dir := filepath.Join(filepath.Dir(path), "backups")
	names := migrationBackupFiles(t, dir)
	if len(names) != 1 || !strings.Contains(names[0], fmt.Sprintf("-%d-to-%d-", LatestSchemaVersion()-1, LatestSchemaVersion())) {
		t.Fatalf("backups = %v", names)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != names[0] {
		t.Fatalf("backup folder contains sidecars or incomplete files: %v, %v", entries, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("backup directory mode = %v, %v", info, err)
	}
	backupPath := filepath.Join(dir, names[0])
	if info, err := os.Stat(backupPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup file mode = %v, %v", info, err)
	}
	if err := verifyMigrationBackup(context.Background(), backupPath); err != nil {
		t.Fatal(err)
	}
	copy, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var version int
	var value string
	if err := copy.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != LatestSchemaVersion()-1 {
		t.Fatalf("backup version = %d, %v", version, err)
	}
	if err := copy.QueryRow(`SELECT value FROM backup_probe`).Scan(&value); err != nil || value != "committed WAL data" {
		t.Fatalf("backup probe = %q, %v", value, err)
	}
	if err := opened.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != LatestSchemaVersion() {
		t.Fatalf("upgraded version = %d, %v", version, err)
	}
}

func TestOpenSkipsBackupForFreshAndCurrentDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.sqlite")
	first, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), "backups")); !os.IsNotExist(err) {
		t.Fatalf("unexpected backup directory: %v", err)
	}
}

func TestOpenAbortsMigrationWhenBackupCannotBeCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.sqlite")
	db := databaseBeforeLatestMigration(t, path)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "backups"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), path, Options{}); err == nil || !strings.Contains(err.Error(), "pre-migration backup") {
		t.Fatalf("Open error = %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != LatestSchemaVersion()-1 {
		t.Fatalf("migration ran after backup failure: version = %d, %v", version, err)
	}
}

func TestMigrationBackupIntegrityRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.sqlite")
	if err := os.WriteFile(path, []byte("not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyMigrationBackup(context.Background(), path); err == nil {
		t.Fatal("corrupt backup passed integrity check")
	}
}

func TestMigrationBackupRotationFailureDoesNotBlockUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.sqlite")
	db := databaseBeforeLatestMigration(t, path)
	if err := backupBeforeMigration(context.Background(), db, path, LatestSchemaVersion()-1, LatestSchemaVersion(), func(string, string) error {
		return errors.New("old backup could not be removed")
	}); err != nil {
		t.Fatalf("verified backup rejected because rotation failed: %v", err)
	}
	dir := filepath.Join(filepath.Dir(path), "backups")
	names := migrationBackupFiles(t, dir)
	if len(names) != 1 {
		t.Fatalf("verified backup missing after rotation warning: %v", names)
	}
	if err := verifyMigrationBackup(context.Background(), filepath.Join(dir, names[0])); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migration after rotation warning: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != LatestSchemaVersion() {
		t.Fatalf("version = %d, %v", version, err)
	}
}

func TestOpenKeepsFreshBackupWhenOlderNamesHaveFutureTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.sqlite")
	databaseBeforeLatestMigration(t, path)
	dir := filepath.Join(filepath.Dir(path), "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 5; day++ {
		name := fmt.Sprintf("aimebu-pre-migrate-1-to-2-209901%02dT000000.000000000Z.sqlite", day)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("older copy"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	orphan := filepath.Join(dir, "aimebu-pre-migrate-1-to-2-20990201T000000.000000000Z.sqlite-wal")
	if err := os.WriteFile(orphan, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan sidecar retained: %v", err)
	}
	names := migrationBackupFiles(t, dir)
	if len(names) != migrationBackupKeep {
		t.Fatalf("kept backups = %v", names)
	}
	var fresh string
	for _, name := range names {
		if !strings.Contains(name, "-2099") {
			fresh = name
		}
	}
	if fresh == "" {
		t.Fatalf("newly verified backup was rotated away: %v", names)
	}
	if err := verifyMigrationBackup(context.Background(), filepath.Join(dir, fresh)); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationBackupRotationContinuesAfterRemoveFailures(t *testing.T) {
	dir := t.TempDir()
	fresh := "aimebu-pre-migrate-1-to-2-20261020T000000.000000000Z.sqlite"
	if err := os.WriteFile(filepath.Join(dir, fresh), []byte("fresh"), 0600); err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 7; day++ {
		name := fmt.Sprintf("aimebu-pre-migrate-1-to-2-202610%02dT000000.000000000Z.sqlite", day)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	failed := filepath.Join(dir, "aimebu-pre-migrate-1-to-2-20261003T000000.000000000Z.sqlite")
	orphan := filepath.Join(dir, "aimebu-pre-migrate-1-to-2-20260901T000000.000000000Z.sqlite-wal")
	if err := os.WriteFile(orphan, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	var attempted []string
	err := rotateMigrationBackupsWithRemove(dir, fresh, func(path string) error {
		attempted = append(attempted, path)
		if path == failed || path == orphan {
			return errors.New("injected deletion failure")
		}
		return os.Remove(path)
	})
	if err == nil || !strings.Contains(err.Error(), failed) || !strings.Contains(err.Error(), orphan) {
		t.Fatalf("all removal failures should be reported: %v", err)
	}
	for _, day := range []int{1, 2} {
		name := fmt.Sprintf("aimebu-pre-migrate-1-to-2-202610%02dT000000.000000000Z.sqlite", day)
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("rotation stopped before removing %s: %v (attempted %v)", name, err, attempted)
		}
	}
	for _, path := range []string{filepath.Join(dir, fresh), failed, orphan} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed or protected backup should remain at %s: %v", path, err)
		}
	}
}

func TestOpenRotatesOnlyMigrationBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.sqlite")
	databaseBeforeLatestMigration(t, path)
	dir := filepath.Join(filepath.Dir(path), "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 6; day++ {
		name := fmt.Sprintf("aimebu-pre-migrate-1-to-2-202610%02dT000000.000000000Z.sqlite", day)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old backup"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "operator-snapshot.sqlite")
	if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if names := migrationBackupFiles(t, dir); len(names) != migrationBackupKeep {
		t.Fatalf("kept backups = %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "aimebu-pre-migrate-1-to-2-20261001T000000.000000000Z.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("old backup retained: %v", err)
	}
	if data, err := os.ReadFile(unrelated); err != nil || string(data) != "keep" {
		t.Fatalf("unrelated file = %q, %v", data, err)
	}
}
