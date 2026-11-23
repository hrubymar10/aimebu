package server

import (
	"os"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// A switch refreshes the usage snapshot in the background, and that refresh writes the usage cache into the usages directory. Once
// WaitUsageRefreshes has returned nothing writes there any more: a test that removes the directory next (its cleanup) does not race the write.
func TestWaitUsageRefreshesJoinsTheRefreshASwitchStarted(t *testing.T) {
	f, _, _ := quotaEnv(t)
	dir := t.TempDir()
	reader, err := usages.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.useUsages(reader)
	for i := 0; i < 20; i++ {
		f.srv.refreshSwitchedUsage("claude")
	}
	f.srv.WaitUsageRefreshes()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refresh wrote into %s after WaitUsageRefreshes returned (stat: %v)", dir, err)
	}
}
