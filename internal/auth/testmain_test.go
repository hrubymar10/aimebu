package auth

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// Every test of this package runs with HOME and the data directories in a temporary root, never the real machine's.
func TestMain(m *testing.M) { testenv.Main(m) }

var (
	migratedOnce  sync.Once
	migratedBytes []byte
	migratedErr   error
)

// openTestStore opens a new store at path. Migrating a database from nothing takes seconds under the race detector and every test here
// wants a store, so the migrated file is built once per package and each test starts from a copy; Open still checks that the copy needs
// no migration.
func openTestStore(t *testing.T, path string) *store.Store {
	t.Helper()
	migratedOnce.Do(func() {
		dir, err := os.MkdirTemp("", "aimebu-auth-template-")
		if err != nil {
			migratedErr = err
			return
		}
		defer os.RemoveAll(dir)
		template := filepath.Join(dir, "template.sqlite")
		db, err := store.Open(context.Background(), template, store.Options{})
		if err != nil {
			migratedErr = err
			return
		}
		if migratedErr = db.Close(); migratedErr == nil {
			migratedBytes, migratedErr = os.ReadFile(template)
		}
	})
	if migratedErr != nil {
		t.Fatalf("build the migrated template: %v", migratedErr)
	}
	if err := os.WriteFile(path, migratedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
