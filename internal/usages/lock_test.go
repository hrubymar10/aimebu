package usages

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockSerializesConcurrentRefreshAcrossManagers(t *testing.T) {
	root := t.TempDir()
	store := NewStoreAt(root)
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	fp := &fakeProvider{key: ProviderCodex}
	clock := &fakeClock{now: time.Unix(1000, 0)}

	const workers = 24
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager := NewManager(NewStoreAt(root), NewRegistry(fp))
			manager.SetClock(clock)
			_, err := manager.Snapshot(context.Background(), ProviderCodex)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
	}
	if got := atomic.LoadInt32(&fp.calls); got != 1 {
		t.Fatalf("fetch calls = %d, want 1", got)
	}
}
