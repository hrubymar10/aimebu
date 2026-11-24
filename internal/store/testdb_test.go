package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// blankTestDB is migrated once, then copied for isolated test stores. Opening
// a copy still applies the normal connection PRAGMAs and migration check.
var blankTestDB []byte

func TestMain(m *testing.M) {
	_, cleanupHome := testenv.Isolate()
	root, err := os.MkdirTemp("", "aimebu-store-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path := filepath.Join(root, "template.sqlite")
	db, err := Open(context.Background(), path, Options{})
	if err == nil {
		err = db.Close()
	}
	if err == nil {
		blankTestDB, err = os.ReadFile(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		cleanupHome()
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	cleanupHome()
	os.Exit(code)
}

// databasesAtVersion holds, per target version, the bytes of a database migrated to that version. Migrating from nothing to a version is
// the slow part of a migration test, so it is done once per package and version and each test starts from a copy.
var databasesAtVersion struct {
	sync.Mutex
	once  map[int]*sync.Once
	bytes map[int][]byte
	err   map[int]error
}

// databaseAtVersion returns a new database file at path, migrated up to and including version (0 means all), opened without any further
// migration: the same thing as sql.Open on a new file followed by migrateUpTo(ctx, db, version), minus the repeated work.
func databaseAtVersion(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	state := &databasesAtVersion
	state.Lock()
	if state.once == nil {
		state.once, state.bytes, state.err = map[int]*sync.Once{}, map[int][]byte{}, map[int]error{}
	}
	once := state.once[version]
	if once == nil {
		once = &sync.Once{}
		state.once[version] = once
	}
	state.Unlock()
	once.Do(func() {
		build := func() ([]byte, error) {
			dir, err := os.MkdirTemp("", "aimebu-store-version-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(dir)
			template := filepath.Join(dir, "template.sqlite")
			db, err := sql.Open("sqlite", sqliteDSN(template))
			if err != nil {
				return nil, err
			}
			if err := migrateUpTo(context.Background(), db, version); err != nil {
				db.Close()
				return nil, err
			}
			if err := db.Close(); err != nil { // closing the last connection checkpoints the WAL into the file
				return nil, err
			}
			return os.ReadFile(template)
		}
		data, err := build()
		state.Lock()
		state.bytes[version], state.err[version] = data, err
		state.Unlock()
	})
	state.Lock()
	data, err := state.bytes[version], state.err[version]
	state.Unlock()
	if err != nil {
		t.Fatalf("build the database at version %d: %v", version, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// fixtureDSN opens a database a test builds by hand (an old store, a v1 server's file): the same file format as sqliteutil.DSN makes, but
// with no fsync after each statement, which on a busy disk costs about half a second per migration and nothing in what the tests check.
func fixtureDSN(path string) string {
	return sqliteutil.DSN(path, false) + "&_pragma=synchronous(OFF)"
}
