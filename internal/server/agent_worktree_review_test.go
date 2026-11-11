package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/supervisor"
)

// agentWorktreeFolders lists the folders under the project's worktrees directory (none when it does not exist yet); any other read error fails
// the test.
func agentWorktreeFolders(t *testing.T, project string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(project, ".aimebu", "worktrees"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (f *fixture) connectFakeSupervisor(t *testing.T) {
	t.Helper()
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not connect")
	}
}
