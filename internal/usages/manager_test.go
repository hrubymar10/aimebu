package usages

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	return Snapshot{Status: StatusOK, Plan: "test"}, nil
}

func TestManagerEmptyRegistry(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, p := range resp.Providers {
		if len(p.Profiles) != 1 || p.Profiles[0].ProfileName != implicitProfileName || !p.Profiles[0].Active {
			t.Fatalf("expected one active local profile for %s: %+v", p.ProviderName, p.Profiles)
		}
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
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := atomic.LoadInt32(&fp.calls); got != 1 {
		t.Fatalf("fetch calls = %d, want 1", got)
	}
}

func TestManagerCredentialChangeBypassesFreshCache(t *testing.T) {
	for _, tc := range []struct {
		provider string
		tool     string
		file     string
	}{
		{provider: ProviderClaudeCode, tool: "claude", file: ".credentials.json"},
		{provider: ProviderCodex, tool: "codex", file: "auth.json"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			root := t.TempDir()
			credPath := filepath.Join(root, tc.file)
			if err := os.WriteFile(credPath, []byte("first"), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewStoreAt(root)
			cfg := DefaultConfig()
			cfg.Providers[tc.provider] = ProviderConfig{Enabled: true}
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			provider := &fakeProvider{key: tc.provider}
			m := NewManager(store, NewRegistry(provider))
			m.SetProfileLister(func(tool string) []ProfileInfo {
				if tool != tc.tool {
					return nil
				}
				return []ProfileInfo{{Tool: tc.tool, Name: "main", Active: true, CredPath: credPath, HasCredentials: true}}
			})

			if _, _, err := m.refresh(context.Background(), tc.provider, false); err != nil {
				t.Fatalf("initial refresh: %v", err)
			}
			if err := os.WriteFile(credPath, []byte("second"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.refresh(context.Background(), tc.provider, false); err != nil {
				t.Fatalf("credential refresh: %v", err)
			}
			if got := atomic.LoadInt32(&provider.calls); got != 2 {
				t.Fatalf("fetch calls = %d, want 2 after credentials changed", got)
			}
		})
	}
}

func TestManagerDiscardsFetchWhenActiveProfileChanges(t *testing.T) {
	for _, tc := range []struct {
		provider string
		tool     string
	}{
		{provider: ProviderClaudeCode, tool: "claude"},
		{provider: ProviderCodex, tool: "codex"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			root := t.TempDir()
			mainPath := filepath.Join(root, "main-creds")
			backupPath := filepath.Join(root, "backup-creds")
			if err := os.WriteFile(mainPath, []byte("main"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backupPath, []byte("backup"), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewStoreAt(root)
			cfg := DefaultConfig()
			cfg.Providers[tc.provider] = ProviderConfig{Enabled: true}
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			provider := &fakeProvider{key: tc.provider, entered: entered, block: release}
			m := NewManager(store, NewRegistry(provider))
			var backupActive atomic.Bool
			m.SetProfileLister(func(tool string) []ProfileInfo {
				if tool != tc.tool {
					return nil
				}
				return []ProfileInfo{
					{Tool: tc.tool, Name: "main", Active: !backupActive.Load(), CredPath: mainPath, HasCredentials: true},
					{Tool: tc.tool, Name: "backup", Active: backupActive.Load(), CredPath: backupPath, HasCredentials: true},
				}
			})

			done := make(chan error, 1)
			go func() {
				_, _, err := m.refresh(context.Background(), tc.provider, true)
				done <- err
			}()
			<-entered
			backupActive.Store(true)
			m.InvalidateProfileSnapshot(tc.tool, "main")
			m.InvalidateProfileSnapshot(tc.tool, "backup")
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("refresh: %v", err)
			}

			cache, err := store.LoadCache()
			if err != nil {
				t.Fatal(err)
			}
			entries := cache.Snapshots[tc.provider]
			if len(entries) != 2 {
				t.Fatalf("profile shape after switch = %+v", entries)
			}
			for _, entry := range entries {
				if entry.Profile.Plan == "test" {
					t.Fatalf("late active fetch was attributed after profile switch: %+v", entries)
				}
				if entry.Profile.ProfileName == "backup" && !entry.Profile.Active {
					t.Fatalf("new active profile was not published: %+v", entries)
				}
			}
		})
	}
}

func TestManagerDiscardsFetchWhenCredentialChangesMidRequest(t *testing.T) {
	root := t.TempDir()
	credPath := filepath.Join(root, "auth.json")
	if err := os.WriteFile(credPath, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStoreAt(root)
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{key: ProviderCodex}
	m := NewManager(store, NewRegistry(provider))
	m.SetProfileLister(func(tool string) []ProfileInfo {
		return []ProfileInfo{{Tool: "codex", Name: "main", Active: true, CredPath: credPath, HasCredentials: true}}
	})
	if _, _, err := m.refresh(context.Background(), ProviderCodex, true); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	provider.entered = entered
	provider.block = release
	done := make(chan error, 1)
	go func() {
		_, _, err := m.refresh(context.Background(), ProviderCodex, true)
		done <- err
	}()
	<-entered
	if err := os.WriteFile(credPath, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("refresh: %v", err)
	}

	cache, err := store.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	entries := cache.Snapshots[ProviderCodex]
	if len(entries) != 1 || entries[0].Profile.Plan != "test" {
		t.Fatalf("previous usage was not preserved after discarded refresh: %+v", entries)
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
			_, _, err := m.refresh(context.Background(), "", false)
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
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "old", LastRefreshAt: &old}},
	})

	if entry.Profile.Status != StatusStaleCache || !entry.Profile.Stale || entry.Profile.Plan != "old" {
		t.Fatalf("profile = %+v", entry.Profile)
	}
	if entry.Profile.LastRefreshAt == nil || !entry.Profile.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.Profile.LastRefreshAt, old)
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
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "old", LastRefreshAt: &old}},
	})

	if entry.Profile.Status != StatusStaleCache || !entry.Profile.Stale || entry.Profile.Plan != "old" {
		t.Fatalf("profile = %+v", entry.Profile)
	}
	if entry.Profile.LastRefreshAt == nil || !entry.Profile.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.Profile.LastRefreshAt, old)
	}
}

func TestManagerTransientServerErrorKeepsPreviousSnapshotStale(t *testing.T) {
	old := time.Unix(100, 0)
	clock := &fakeClock{now: old.Add(time.Hour)}
	m := NewManager(NewStoreAt(t.TempDir()), NewRegistry(&fakeProvider{
		key: ProviderCodex,
		err: &SnapshotError{
			Snapshot: Snapshot{
				Status:      StatusFetchError,
				Error:       "Codex usage endpoint returned HTTP 503.",
				ErrorDetail: fieldDetail("usage", "http_503"),
			},
			Err: errors.New("Codex usage endpoint returned HTTP 503."),
		},
	}))
	m.SetClock(clock)

	entry := m.fetchOne(context.Background(), ProviderCodex, CacheEntry{
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "old", LastRefreshAt: &old}},
	})

	if entry.Profile.Status != StatusStaleCache || !entry.Profile.Stale || entry.Profile.Plan != "old" {
		t.Fatalf("snapshot = %+v", entry.Profile)
	}
	if entry.Profile.LastRefreshAt == nil || !entry.Profile.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.Profile.LastRefreshAt, old)
	}
}

func TestManagerRateLimitKeepsPreviousSnapshotStale(t *testing.T) {
	old := time.Unix(100, 0)
	clock := &fakeClock{now: old.Add(time.Hour)}
	m := NewManager(NewStoreAt(t.TempDir()), NewRegistry(&fakeProvider{
		key: ProviderCodex,
		err: &SnapshotError{
			Snapshot: Snapshot{
				Status:      StatusFetchError,
				Error:       "Codex usage endpoint returned HTTP 429.",
				ErrorDetail: httpStatusDetail("usage", []byte(`{"error":"rate limited"}`), 429),
			},
			Err: errors.New("Codex usage endpoint returned HTTP 429."),
		},
	}))
	m.SetClock(clock)

	entry := m.fetchOne(context.Background(), ProviderCodex, CacheEntry{
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "old", LastRefreshAt: &old}},
	})

	if entry.Profile.Status != StatusStaleCache || !entry.Profile.Stale || entry.Profile.Plan != "old" {
		t.Fatalf("a 429 blanked the previous snapshot instead of preserving it stale: %+v", entry.Profile)
	}
	if entry.Profile.LastRefreshAt == nil || !entry.Profile.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.Profile.LastRefreshAt, old)
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
	cache.Snapshots[ProviderCodex] = []CacheEntry{{
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "old", LastRefreshAt: &old}},
	}}
	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: old.Add(time.Hour)}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderCodex, err: &net.DNSError{Err: "no such host", Name: "usage.example"}}))
	m.SetClock(clock)
	// Snapshot is a pure cache read now — it never fetches. ForceRefresh
	// triggers the fetch, which fails (DNS error) and sets the stale-cache
	// status on the cached entry. Snapshot then reads it back.
	if _, _, err := m.ForceRefresh(context.Background(), ProviderCodex); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	resp, err := m.Snapshot(context.Background(), ProviderCodex)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	snap := resp.Providers[0].Profiles[0]
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
			Snapshot: Snapshot{Status: StatusAuthMissing, Error: "OAuth token was rejected."},
			Err:      errors.New("OAuth token was rejected."),
		},
	}))
	m.SetClock(clock)

	entry := m.fetchOne(context.Background(), ProviderCodex, CacheEntry{
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "old", LastRefreshAt: &old}},
	})

	if entry.Profile.Status != StatusAuthMissing || entry.Profile.Stale || entry.Profile.Plan != "" {
		t.Fatalf("snapshot = %+v", entry.Profile)
	}
	if entry.Profile.LastRefreshAt == nil || !entry.Profile.LastRefreshAt.Equal(now) {
		t.Fatalf("last refresh = %+v, want %v", entry.Profile.LastRefreshAt, now)
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
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "old", LastRefreshAt: &old}},
	})

	if entry.Profile.Status != StatusStaleCache || !entry.Profile.Stale || entry.Profile.Plan != "old" {
		t.Fatalf("snapshot = %+v", entry.Profile)
	}
	if entry.Profile.LastRefreshAt == nil || !entry.Profile.LastRefreshAt.Equal(old) {
		t.Fatalf("last refresh = %+v, want %v", entry.Profile.LastRefreshAt, old)
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
	resp, _, err := m.refresh(context.Background(), ProviderCodex, false)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := resp.Providers[0].Profiles[0].Error; got != "failed with "+redacted {
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
	updates := make(chan UsagesResponse, 1)
	m.SetUpdateHook(func(resp UsagesResponse) {
		updates <- resp
	})
	if _, _, err := m.ForceRefresh(context.Background(), ProviderCodex); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	select {
	case resp := <-updates:
		if resp.Providers[0].Profiles[0].Status != StatusOK {
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
		_, _, err := m.refresh(context.Background(), ProviderCodex, false)
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

// TestInvariantProfileNameNeverEmpty verifies that every profile entry in the
// response has a non-empty ProfileName. ProfileName is the join key — an empty
// value silently breaks the multi-profile card. This catches the class of bug
// where ProfileName is set on the cache entry but not on the response Profile.
func TestInvariantProfileNameNeverEmpty(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderCodex}))
	resp, _, err := m.refresh(context.Background(), "", false)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, prov := range resp.Providers {
		for i, p := range prov.Profiles {
			if p.ProfileName == "" {
				t.Fatalf("provider %s profile %d has empty ProfileName", prov.ProviderName, i)
			}
		}
	}
}

// TestInvariantNamedMigrationDropsUnownedDefault verifies that legacy
// default history is never attributed to a named switcher account whose
// ownership cannot be proven.
func TestInvariantNamedMigrationDropsUnownedDefault(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cache := EmptyCache()
	cache.Snapshots[ProviderClaudeCode] = []CacheEntry{{
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "test", LastRefreshAt: &now}},
	}}
	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{{Tool: "claude", Name: "main", Active: true, Email: "main@test", HasCredentials: true}}
	})
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var entries []Profile
	for _, prov := range resp.Providers {
		if prov.ProviderName == ProviderClaudeCode {
			entries = prov.Profiles
			break
		}
	}
	if len(entries) != 1 {
		t.Fatalf("expected one named placeholder, got %d", len(entries))
	}
	found := entries[0]
	if found.ProfileName != "main" {
		t.Fatalf("expected ProfileName=main, got %q", found.ProfileName)
	}
	if found.Plan != "" || found.LastRefreshAt != nil {
		t.Fatalf("legacy default history was attributed to main: %+v", found)
	}
	if !found.Active || !found.HasCredentials || found.Email != "main@test" {
		t.Fatalf("named placeholder lost profile facts: %+v", found)
	}
}

func TestCacheMigrationPreservesOwnedHistory(t *testing.T) {
	t.Run("implicit", func(t *testing.T) {
		store := NewStoreAt(t.TempDir())
		old := time.Unix(100, 0)
		newer := old.Add(time.Minute)
		cache := EmptyCache()
		cache.Snapshots[ProviderCodex] = []CacheEntry{
			{Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "legacy", LastRefreshAt: &old}}},
			{Profile: Profile{ProfileName: implicitProfileName, Snapshot: Snapshot{Status: StatusOK, Plan: "newer", Windows: []Window{{Key: "weekly", PercentUsed: 42}}, LastRefreshAt: &newer}}},
		}
		if err := store.SaveCache(cache); err != nil {
			t.Fatal(err)
		}

		m := NewManager(store, EmptyRegistry())
		resp, err := m.Snapshot(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		profiles := providerProfiles(resp, ProviderCodex)
		if len(profiles) != 1 || profiles[0].ProfileName != implicitProfileName || profiles[0].Plan != "newer" {
			t.Fatalf("implicit migration = %+v", profiles)
		}
		if len(profiles[0].Windows) != 1 || profiles[0].Windows[0].PercentUsed != 42 {
			t.Fatalf("implicit history lost windows: %+v", profiles[0])
		}
	})

	t.Run("named", func(t *testing.T) {
		store := NewStoreAt(t.TempDir())
		old := time.Unix(100, 0)
		newer := old.Add(time.Minute)
		cache := EmptyCache()
		cache.Snapshots[ProviderClaudeCode] = []CacheEntry{
			{Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, Plan: "unowned", LastRefreshAt: &newer}}},
			{Profile: Profile{ProfileName: "ghost", Snapshot: Snapshot{Status: StatusOK, Plan: "orphan", LastRefreshAt: &newer}}},
			{Profile: Profile{ProfileName: "main", Snapshot: Snapshot{Status: StatusStaleCache, Plan: "old-main", LastRefreshAt: &old}}},
			{Profile: Profile{ProfileName: "main", Snapshot: Snapshot{Status: StatusOK, Plan: "new-main", Windows: []Window{{Key: "session", PercentUsed: 17}}, LastRefreshAt: &newer}}},
			{Profile: Profile{ProfileName: "backup", Snapshot: Snapshot{Status: StatusOK, Plan: "backup-history", LastRefreshAt: &old}}},
		}
		if err := store.SaveCache(cache); err != nil {
			t.Fatal(err)
		}
		m := NewManager(store, EmptyRegistry())
		m.SetProfileLister(func(tool string) []ProfileInfo {
			if tool != "claude" {
				return []ProfileInfo{}
			}
			return []ProfileInfo{
				{Tool: "claude", Name: "main", Active: true, HasCredentials: true, Email: "main@test"},
				{Tool: "claude", Name: "backup", HasCredentials: true},
			}
		})
		resp, err := m.Snapshot(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		profiles := providerProfiles(resp, ProviderClaudeCode)
		if len(profiles) != 2 || profiles[0].ProfileName != "main" || profiles[1].ProfileName != "backup" {
			t.Fatalf("named migration identities = %+v", profiles)
		}
		if profiles[0].Plan != "new-main" || len(profiles[0].Windows) != 1 || profiles[0].Windows[0].PercentUsed != 17 {
			t.Fatalf("newest main history not preserved: %+v", profiles[0])
		}
		if profiles[1].Plan != "backup-history" {
			t.Fatalf("backup history not preserved: %+v", profiles[1])
		}
		persisted, err := store.LoadCache()
		if err != nil {
			t.Fatal(err)
		}
		if len(persisted.Snapshots[ProviderClaudeCode]) != 2 {
			t.Fatalf("migration did not purge legacy/orphan/duplicate entries: %+v", persisted.Snapshots[ProviderClaudeCode])
		}
	})
}

func providerProfiles(resp UsagesResponse, key string) []Profile {
	for _, provider := range resp.Providers {
		if provider.ProviderName == key {
			return provider.Profiles
		}
	}
	return nil
}

// TestInvariantProfilesNeverNil verifies that every provider in the response
// has a non-nil Profiles slice (always at least []). A nil slice would cause
// frontend iteration issues and is indistinguishable from "not initialized".
func TestInvariantProfilesNeverNil(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, DefaultRegistry())
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, prov := range resp.Providers {
		if prov.Profiles == nil {
			t.Fatalf("provider %s has nil Profiles — must be initialized to []", prov.ProviderName)
		}
	}
}

// TestInvariantNoDefaultOnSwitcherTool verifies that after a full refresh cycle
// (provider fetch + profile derivation), no profile on a switcher tool is named
// "default". Fetches write directly under their resolved profile identity.
func TestInvariantNoDefaultOnSwitcherTool(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{
			{Tool: "claude", Name: "main", Active: true, HasCredentials: true},
			{Tool: "claude", Name: "bkp", Active: false, HasCredentials: true},
		}
	})
	// Full refresh cycle: provider fetch + profile derivation
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, prov := range resp.Providers {
		if !prov.SwitchEligible {
			continue
		}
		for _, p := range prov.Profiles {
			if p.ProfileName == "default" {
				t.Fatalf("switcher provider %s has a profile named \"default\" — should be renamed to the active profile's name", prov.ProviderName)
			}
		}
	}
}

// TestInvariantExactlyOneActivePerSwitcherTool verifies that after a full
// refresh cycle, exactly one profile per switcher tool has Active: true.
// Zero means the active flag is never transferred; two means a stale entry
// wasn't cleaned up. Both are the class of bug that passes the compiler and
// the gate but fails in production.
func TestInvariantExactlyOneActivePerSwitcherTool(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{
			{Tool: "claude", Name: "main", Active: true, HasCredentials: true},
			{Tool: "claude", Name: "bkp", Active: false, HasCredentials: true},
		}
	})
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, prov := range resp.Providers {
		if !prov.SwitchEligible {
			continue
		}
		activeCount := 0
		for _, p := range prov.Profiles {
			if p.Active {
				activeCount++
			}
		}
		if activeCount != 1 {
			t.Fatalf("switcher provider %s has %d active profiles, want exactly 1", prov.ProviderName, activeCount)
		}
	}
}

// TestInvariantNoDuplicatesAcrossCycles verifies that refreshing twice
// (simulating two poller cycles) does not produce duplicate profile entries.
// The first cycle creates entries; the second must replace, not append.
// This catches writes that accidentally append rather than replace one target.
func TestInvariantNoDuplicatesAcrossCycles(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{
			{Tool: "claude", Name: "main", Active: true, HasCredentials: true},
			{Tool: "claude", Name: "bkp", Active: false, HasCredentials: true},
		}
	})
	// Two full refresh cycles
	for cycle := 0; cycle < 2; cycle++ {
		if _, _, err := m.refresh(context.Background(), "", false); err != nil {
			t.Fatalf("cycle %d refresh: %v", cycle, err)
		}
		if _, _, err := m.refresh(context.Background(), "", false); err != nil {
			t.Fatalf("cycle %d refresh: %v", cycle, err)
		}
	}
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, prov := range resp.Providers {
		if prov.ProviderName != ProviderClaudeCode {
			continue
		}
		seen := map[string]bool{}
		for _, p := range prov.Profiles {
			if seen[p.ProfileName] {
				t.Fatalf("duplicate profile %q after 2 cycles — entries must be replaced, not appended", p.ProfileName)
			}
			seen[p.ProfileName] = true
		}
		if len(prov.Profiles) != 2 {
			t.Fatalf("expected 2 profiles after 2 cycles, got %d: %+v", len(prov.Profiles), prov.Profiles)
		}
	}
}

// TestInvariantOrphanEntriesRemoved verifies that after a refresh cycle, cache
// entries whose ProfileName isn't in the current profile list are removed.
// This catches the "default" orphan from older builds and profiles deleted in
// the switcher. The test seeds a stale "default" entry that no longer
// corresponds to any profile and asserts it's gone after refresh.
func TestInvariantOrphanEntriesRemoved(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// Seed a stale "default" entry (orphan from an older build)
	now := time.Now()
	cache := EmptyCache()
	cache.Snapshots[ProviderClaudeCode] = []CacheEntry{{
		Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, LastRefreshAt: &now}},
	}}
	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{
			{Tool: "claude", Name: "main", Active: true, HasCredentials: true},
		}
	})
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, prov := range resp.Providers {
		if prov.ProviderName != ProviderClaudeCode {
			continue
		}
		for _, p := range prov.Profiles {
			if p.ProfileName == "default" {
				t.Fatalf("orphan \"default\" entry survived reconciliation — should be removed when not in the current profile list")
			}
		}
	}
}

// TestEndStateAllPropertiesWithSeededHistory verifies the four end-state
// properties from wren's #13549 against a cache that has history: a stale
// "default" orphan, a deleted profile, and a profile whose fetch fails.
// Properties:
// 1. profiles array contains exactly the current profiles, no orphans
// 2. every profile carries account facts (profile_name, has_credentials, etc.)
// 3. exactly one active per switcher tool
// 4. usage data is independent per profile
func TestEndStateAllPropertiesWithSeededHistory(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// Seed history: a "default" orphan and a "deleted" profile that no longer
	// exists in the switcher.
	now := time.Now()
	cache := EmptyCache()
	cache.Snapshots[ProviderClaudeCode] = []CacheEntry{
		{Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusOK, LastRefreshAt: &now}}},
		{Profile: Profile{ProfileName: "ghost", Snapshot: Snapshot{Status: StatusOK, LastRefreshAt: &now}}},
	}
	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{
			{Tool: "claude", Name: "main", Active: true, HasCredentials: true, Email: "main@test"},
			{Tool: "claude", Name: "bkp", Active: false, HasCredentials: false, Email: ""},
		}
	})
	// Full refresh cycle
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Find the claude-code provider
	var prov *Provider
	for i := range resp.Providers {
		if resp.Providers[i].ProviderName == ProviderClaudeCode {
			prov = &resp.Providers[i]
			break
		}
	}
	if prov == nil {
		t.Fatal("claude-code provider not found in response")
	}
	// Property 1: exactly the current profiles, no orphans
	if len(prov.Profiles) != 2 {
		t.Fatalf("property 1: expected 2 profiles, got %d: %+v", len(prov.Profiles), prov.Profiles)
	}
	seen := map[string]bool{}
	for _, p := range prov.Profiles {
		if p.ProfileName == "default" || p.ProfileName == "ghost" {
			t.Fatalf("property 1: orphan %q survived reconciliation", p.ProfileName)
		}
		if seen[p.ProfileName] {
			t.Fatalf("property 1: duplicate profile %q", p.ProfileName)
		}
		seen[p.ProfileName] = true
	}
	if !seen["main"] || !seen["bkp"] {
		t.Fatalf("property 1: missing expected profiles, seen=%v", seen)
	}
	// Property 2: every profile carries account facts
	for _, p := range prov.Profiles {
		if p.ProfileName == "" {
			t.Fatal("property 2: empty profile_name")
		}
		if p.ProfileName == "main" && !p.HasCredentials {
			t.Fatal("property 2: main has HasCredentials=false, want true")
		}
		if p.ProfileName == "bkp" && p.HasCredentials {
			t.Fatal("property 2: bkp has HasCredentials=true, want false")
		}
	}
	// Property 3: exactly one active
	activeCount := 0
	for _, p := range prov.Profiles {
		if p.Active {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("property 3: %d active profiles, want exactly 1", activeCount)
	}
	// Property 5: every profile has a non-null last_refresh_at
	for _, p := range prov.Profiles {
		if p.LastRefreshAt == nil {
			t.Fatalf("property 5: profile %q has nil last_refresh_at — must be stamped on every write including failed fetches", p.ProfileName)
		}
	}
	// Property 6: provider has a non-empty label
	if prov.Label == "" {
		t.Fatalf("property 6: provider %s has empty label", prov.ProviderName)
	}
}

// TestInvariantAllProfileTimestampsAdvance verifies that after a refresh
// cycle past the configured interval, every profile's timestamp advances —
// not just the active one. With the one-cadence design (all profiles on the
// provider interval), this is the property that catches a staleness check
// reading the wrong entry's timestamp and blocking fetches indefinitely.
func TestInvariantAllProfileTimestampsAdvance(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	cfg.RefreshIntervalSec = MinRefreshSec
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Unix(1000, 0)}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetClock(clock)
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{
			{Tool: "claude", Name: "main", Active: true, HasCredentials: true},
			{Tool: "claude", Name: "bkp", Active: false, HasCredentials: true},
		}
	})
	// First refresh
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	resp1, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	ts1 := map[string]time.Time{}
	for _, prov := range resp1.Providers {
		if prov.ProviderName != ProviderClaudeCode {
			continue
		}
		for _, p := range prov.Profiles {
			if p.LastRefreshAt == nil {
				t.Fatalf("first snapshot: %s has nil LastRefreshAt", p.ProfileName)
			}
			ts1[p.ProfileName] = *p.LastRefreshAt
		}
	}
	if len(ts1) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(ts1))
	}
	// Advance clock past the interval and refresh again
	clock.now = ts1["main"].Add(time.Duration(cfg.RefreshIntervalSec) * time.Second)
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	resp2, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	for _, prov := range resp2.Providers {
		if prov.ProviderName != ProviderClaudeCode {
			continue
		}
		for _, p := range prov.Profiles {
			old, ok := ts1[p.ProfileName]
			if !ok {
				t.Fatalf("second snapshot: new profile %s appeared", p.ProfileName)
			}
			if p.LastRefreshAt == nil || !p.LastRefreshAt.After(old) {
				t.Fatalf("profile %s timestamp did not advance: was %v, now %v", p.ProfileName, old, p.LastRefreshAt)
			}
		}
	}
}

func TestRefreshPublishesNamedActiveEntryWhenProviderEntryMissing(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Unix(2000, 0)}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode, err: errors.New("temporary claude failure")}))
	m.SetClock(clock)
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{{
			Tool:           "claude",
			Name:           "main",
			Active:         true,
			CredPath:       "live-credentials.json",
			Email:          "main@test",
			HasCredentials: true,
		}}
	})
	if _, _, err := m.refresh(context.Background(), "", false); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	resp, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var profiles []Profile
	for _, prov := range resp.Providers {
		if prov.ProviderName == ProviderClaudeCode {
			profiles = prov.Profiles
			break
		}
	}
	if len(profiles) != 1 {
		t.Fatalf("profiles len = %d, want 1: %+v", len(profiles), profiles)
	}
	p := profiles[0]
	if p.ProfileName != "main" || !p.Active || !p.HasCredentials || p.Email != "main@test" {
		t.Fatalf("profile identity = %+v", p)
	}
	if p.Status != StatusFetchError || p.Error != "temporary claude failure" {
		t.Fatalf("profile status = %+v", p)
	}
	if p.LastRefreshAt == nil || !p.LastRefreshAt.Equal(clock.now) {
		t.Fatalf("LastRefreshAt = %v, want %v", p.LastRefreshAt, clock.now)
	}
}

func TestForceRefreshBroadcastsNamedProfileError(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, NewRegistry(
		&fakeProvider{key: ProviderCodex},
		&fakeProvider{key: ProviderClaudeCode, err: errors.New("temporary claude failure")},
	))
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{{
			Tool:           "claude",
			Name:           "main",
			Active:         true,
			HasCredentials: true,
		}}
	})
	updates := make(chan UsagesResponse, 1)
	m.SetUpdateHook(func(resp UsagesResponse) {
		updates <- resp
	})

	if _, _, err := m.ForceRefresh(context.Background(), ""); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	var update UsagesResponse
	select {
	case update = <-updates:
	default:
		t.Fatal("force refresh did not emit update")
	}
	var codex, claude *Provider
	for i := range update.Providers {
		switch update.Providers[i].ProviderName {
		case ProviderCodex:
			codex = &update.Providers[i]
		case ProviderClaudeCode:
			claude = &update.Providers[i]
		}
	}
	if codex == nil || len(codex.Profiles) != 1 || codex.Profiles[0].Status != StatusOK {
		t.Fatalf("codex update = %+v", codex)
	}
	if claude == nil || len(claude.Profiles) != 1 {
		t.Fatalf("claude update = %+v", claude)
	}
	if got := claude.Profiles[0]; got.ProfileName != "main" || !got.Active || got.Status != StatusFetchError {
		t.Fatalf("claude profile = %+v", got)
	}
}
