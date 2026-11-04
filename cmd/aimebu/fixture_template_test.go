package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hrubymar10/aimebu/internal/store"
)

// Migrating a fresh database takes about three seconds under the race detector on a two CPU runner, and nearly every test of this package that
// needs a database used to do it for itself: doctor_db_test.go alone opened nineteen. The migrated, empty database is the same every time, so
// it is made once per test binary, under the package's isolated root (which is removed with it), and each test gets a copy of that file.
var fixtureTemplate struct {
	once sync.Once
	path string
	err  error
}

func fixtureTemplatePath() (string, error) {
	fixtureTemplate.once.Do(func() {
		dir, err := os.MkdirTemp(packageTestRoot, "fixture-db-")
		if err != nil {
			fixtureTemplate.err = err
			return
		}
		path := filepath.Join(dir, "aimebu.sqlite")
		db, err := store.Open(context.Background(), path, store.Options{})
		if err != nil {
			fixtureTemplate.err = err
			return
		}
		if err := db.Close(); err != nil {
			fixtureTemplate.err = err
			return
		}
		// A copy of the main file alone must be the whole database: nothing may be left in a write-ahead log.
		for _, suffix := range []string{"-wal", "-shm"} {
			if info, err := os.Stat(path + suffix); err == nil && info.Size() > 0 {
				fixtureTemplate.err = fmt.Errorf("write-ahead data left in %s (%d bytes): the template is not the whole database", path+suffix, info.Size())
				return
			}
		}
		fixtureTemplate.path = path
	})
	return fixtureTemplate.path, fixtureTemplate.err
}

// copyFixtureDatabase puts a migrated, empty database at path (its folder is made if needed).
func copyFixtureDatabase(t *testing.T, path string) {
	t.Helper()
	template, err := fixtureTemplatePath()
	if err != nil {
		t.Fatalf("make the template database: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(template)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// openFixtureStore opens a migrated, empty store at path, as store.Open would on a new file, without migrating it again.
func openFixtureStore(t *testing.T, path string) *store.Store {
	t.Helper()
	copyFixtureDatabase(t, path)
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("open fixture database %s: %v", path, err)
	}
	return db
}

// useMigratedConfigDir points the config directory at dir and puts a migrated, empty database in its server folder, which is what a first
// `server` start finds after it has migrated: the start then opens it instead of spending the three seconds on the migrations again. A test
// that is about the first creation of the database (or about its absence) sets the variable itself.
func useMigratedConfigDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("AIMEBU2_CONFIG_DIR", dir)
	path := filepath.Join(dir, "server", "aimebu.sqlite")
	if _, err := os.Stat(path); err == nil {
		return
	}
	copyFixtureDatabase(t, path)
}
