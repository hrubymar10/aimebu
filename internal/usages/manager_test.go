package usages

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

type fakeProvider struct {
	key     string
	calls   int32
	err     error
	entered chan<- struct{}
	block   <-chan struct{}
}

func (f *fakeProvider) Key() string { return f.key }

func (f *fakeProvider) Fetch(ctx context.Context, store *Store) (Snapshot, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	}
	if f.err != nil {
		return Snapshot{}, f.err
	}
	return Snapshot{Provider: f.key, Status: StatusOK, Plan: "test"}, nil
}

func TestManagerEmptyRegistry(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(resp.Snapshots) != 0 {
		t.Fatalf("snapshots = %+v", resp.Snapshots)
	}
}

func TestManagerIntervalGatesFetch(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{key: ProviderCodex}
	clock := &fakeClock{now: time.Unix(1000, 0)}
	m := NewManager(store, NewRegistry(fp))
	m.SetClock(clock)
	if _, err := m.Snapshot(context.Background(), ""); err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	if _, err := m.Snapshot(context.Background(), ""); err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	if got := atomic.LoadInt32(&fp.calls); got != 1 {
		t.Fatalf("fetch calls = %d, want 1", got)
	}
}

func TestManagerConcurrentSnapshotsShareLockAndInterval(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{key: ProviderCodex}
	clock := &fakeClock{now: time.Unix(1000, 0)}
	m := NewManager(store, NewRegistry(fp))
	m.SetClock(clock)

	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Snapshot(context.Background(), "")
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

func TestManagerForceCooldown(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{key: ProviderCodex}
	clock := &fakeClock{now: time.Unix(1000, 0)}
	m := NewManager(store, NewRegistry(fp))
	m.SetClock(clock)
	if _, _, err := m.ForceRefresh(context.Background(), ProviderCodex); err != nil {
		t.Fatalf("first ForceRefresh: %v", err)
	}
	_, retry, err := m.ForceRefresh(context.Background(), ProviderCodex)
	if !errors.Is(err, ErrForceCooldown) || retry <= 0 {
		t.Fatalf("second ForceRefresh err=%v retry=%d", err, retry)
	}
}

func TestManagerDNSFailureKeepsPreviousSnapshotStale(t *testing.T) {
	old := time.Unix(100, 0)
	clock := &fakeClock{now: old.Add(time.Hour)}
	m := NewManager(NewStoreAt(t.TempDir()), NewRegistry(&fakeProvider{
		key: ProviderCodex,
		err: &net.DNSError{Err: "no such host", Name: "usage.example"},
	}))
	m.SetClock(clock)

	entry := m.fetchOne(context.Background(), ProviderCodex, CacheEntry{
		Snapshot:      Snapshot{Provider: ProviderCodex, Status: StatusOK, Plan: "old"},
		LastRefreshAt: &old,
	})

	if entry.Snapshot.Status != StatusStaleCache || !entry.Snapshot.Stale || entry.Snapshot.Plan != "old" {
		t.Fatalf("snapshot = %+v", entry.Snapshot)
	}
	if entry.LastRefreshAt == nil || !entry.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.LastRefreshAt, old)
	}
}

func TestManagerConnectionResetKeepsPreviousSnapshotStale(t *testing.T) {
	old := time.Unix(100, 0)
	clock := &fakeClock{now: old.Add(time.Hour)}
	m := NewManager(NewStoreAt(t.TempDir()), NewRegistry(&fakeProvider{
		key: ProviderCodex,
		err: fmt.Errorf("read tcp: %w", syscall.ECONNRESET),
	}))
	m.SetClock(clock)

	entry := m.fetchOne(context.Background(), ProviderCodex, CacheEntry{
		Snapshot:      Snapshot{Provider: ProviderCodex, Status: StatusOK, Plan: "old"},
		LastRefreshAt: &old,
	})

	if entry.Snapshot.Status != StatusStaleCache || !entry.Snapshot.Stale || entry.Snapshot.Plan != "old" {
		t.Fatalf("snapshot = %+v", entry.Snapshot)
	}
	if entry.LastRefreshAt == nil || !entry.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.LastRefreshAt, old)
	}
}

func TestManagerTransientServerErrorKeepsPreviousSnapshotStale(t *testing.T) {
	old := time.Unix(100, 0)
	clock := &fakeClock{now: old.Add(time.Hour)}
	m := NewManager(NewStoreAt(t.TempDir()), NewRegistry(&fakeProvider{
		key: ProviderCodex,
		err: &SnapshotError{
			Snapshot: Snapshot{
				Provider:    ProviderCodex,
				Status:      StatusFetchError,
				Error:       "Codex usage endpoint returned HTTP 503.",
				ErrorDetail: fieldDetail("usage", "http_503"),
			},
			Err: errors.New("Codex usage endpoint returned HTTP 503."),
		},
	}))
	m.SetClock(clock)

	entry := m.fetchOne(context.Background(), ProviderCodex, CacheEntry{
		Snapshot:      Snapshot{Provider: ProviderCodex, Status: StatusOK, Plan: "old"},
		LastRefreshAt: &old,
	})

	if entry.Snapshot.Status != StatusStaleCache || !entry.Snapshot.Stale || entry.Snapshot.Plan != "old" {
		t.Fatalf("snapshot = %+v", entry.Snapshot)
	}
	if entry.LastRefreshAt == nil || !entry.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.LastRefreshAt, old)
	}
}

func TestManagerStaleCacheSurvivesRepeatedFailures(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	cfg.RefreshIntervalSec = MinRefreshSec
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(100, 0)
	cache := EmptyCache()
	cache.Snapshots[ProviderCodex] = CacheEntry{
		Snapshot:      Snapshot{Provider: ProviderCodex, Status: StatusOK, Plan: "old"},
		LastRefreshAt: &old,
	}
	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: old.Add(time.Hour)}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderCodex, err: &net.DNSError{Err: "no such host", Name: "usage.example"}}))
	m.SetClock(clock)
	if _, err := m.Snapshot(context.Background(), ProviderCodex); err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	resp, err := m.Snapshot(context.Background(), ProviderCodex)
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	snap := resp.Snapshots[ProviderCodex]
	if snap.Status != StatusStaleCache || !snap.Stale || snap.Plan != "old" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestManagerAuthFailureDropsPreviousSnapshot(t *testing.T) {
	old := time.Unix(100, 0)
	now := old.Add(time.Hour)
	clock := &fakeClock{now: now}
	m := NewManager(NewStoreAt(t.TempDir()), NewRegistry(&fakeProvider{
		key: ProviderCodex,
		err: &SnapshotError{
			Snapshot: Snapshot{Provider: ProviderCodex, Status: StatusAuthMissing, Error: "OAuth token was rejected."},
			Err:      errors.New("OAuth token was rejected."),
		},
	}))
	m.SetClock(clock)

	entry := m.fetchOne(context.Background(), ProviderCodex, CacheEntry{
		Snapshot:      Snapshot{Provider: ProviderCodex, Status: StatusOK, Plan: "old"},
		LastRefreshAt: &old,
	})

	if entry.Snapshot.Status != StatusAuthMissing || entry.Snapshot.Stale || entry.Snapshot.Plan != "" {
		t.Fatalf("snapshot = %+v", entry.Snapshot)
	}
	if entry.LastRefreshAt == nil || !entry.LastRefreshAt.Equal(now) {
		t.Fatalf("last refresh = %+v, want %v", entry.LastRefreshAt, now)
	}
}

func TestManagerTimeoutKeepsPreviousSnapshotStale(t *testing.T) {
	old := time.Unix(100, 0)
	clock := &fakeClock{now: old.Add(time.Hour)}
	m := NewManager(NewStoreAt(t.TempDir()), NewRegistry(&fakeProvider{key: ProviderCodex, block: make(chan struct{})}))
	m.SetClock(clock)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	entry := m.fetchOne(ctx, ProviderCodex, CacheEntry{
		Snapshot:      Snapshot{Provider: ProviderCodex, Status: StatusOK, Plan: "old"},
		LastRefreshAt: &old,
	})

	if entry.Snapshot.Status != StatusStaleCache || !entry.Snapshot.Stale || entry.Snapshot.Plan != "old" {
		t.Fatalf("snapshot = %+v", entry.Snapshot)
	}
	if entry.LastRefreshAt == nil || !entry.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.LastRefreshAt, old)
	}
}

func TestManagerRedactsConfiguredSecretsFromFreshErrors(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true, Token: "token-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderCodex, err: errors.New("failed with token-secret")}))
	resp, err := m.Snapshot(context.Background(), ProviderCodex)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := resp.Snapshots[ProviderCodex].Error; got != "failed with "+redacted {
		t.Fatalf("error = %q", got)
	}
}

func TestManagerForceRefreshEmitsUpdate(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderCodex}))
	updates := make(chan Response, 1)
	m.SetUpdateHook(func(resp Response) {
		updates <- resp
	})
	if _, _, err := m.ForceRefresh(context.Background(), ProviderCodex); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	select {
	case resp := <-updates:
		if resp.Snapshots[ProviderCodex].Status != StatusOK {
			t.Fatalf("update response = %+v", resp)
		}
	default:
		t.Fatal("force refresh did not emit update")
	}
}

func TestManagerDoesNotHoldStoreLockDuringFetch(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderCodex, entered: entered, block: release}))

	done := make(chan error, 1)
	go func() {
		_, err := m.Snapshot(context.Background(), ProviderCodex)
		done <- err
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start")
	}

	settingsDone := make(chan error, 1)
	go func() {
		_, err := m.Settings(context.Background())
		settingsDone <- err
	}()
	select {
	case err := <-settingsDone:
		if err != nil {
			t.Fatalf("Settings: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("store lock was held while provider fetch was blocked")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
}
