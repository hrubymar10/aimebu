package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
