package usages

import (
	"context"
	"testing"
	"time"
)

func TestActiveCacheEntryPreventsStarvation(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1000, 0)
	clock := &fakeClock{now: now}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetClock(clock)

	// Seed history
	cache := EmptyCache()
	// Inactive profile - fresh
	cache.Snapshots[ProviderClaudeCode] = append(cache.Snapshots[ProviderClaudeCode], CacheEntry{
		Profile: Profile{
			ProfileName: "bkp",
			Active:      false,
			Snapshot:    Snapshot{Status: StatusOK, LastRefreshAt: &now},
		},
	})
	// Active profile - stale (older than 1 hour, assuming interval is < 1h)
	staleTime := now.Add(-2 * time.Hour)
	cache.Snapshots[ProviderClaudeCode] = append(cache.Snapshots[ProviderClaudeCode], CacheEntry{
		Profile: Profile{
			ProfileName: "main",
			Active:      true,
			Snapshot:    Snapshot{Status: StatusOK, LastRefreshAt: &staleTime},
		},
	})

	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}

	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{{
			Tool:           "claude",
			Name:           "main",
			Active:         true,
			HasCredentials: true,
			Email:          "main@test",
		}}
	})

	// Advance time so that main is definitely stale and bkp is not
	clock.now = now.Add(30 * time.Minute)

	// Should perform refresh because 'main' is stale
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

	var mainProfile *Profile
	for _, prov := range resp.Providers {
		if prov.ProviderName == ProviderClaudeCode {
			for i := range prov.Profiles {
				if prov.Profiles[i].ProfileName == "main" {
					p := prov.Profiles[i]
					mainProfile = &p
				}
			}
		}
	}

	if mainProfile == nil {
		t.Fatal("could not find main profile in response")
	}

	// Timestamp should have updated from staleTime to clock.now (approx)
	if mainProfile.LastRefreshAt == nil || !mainProfile.LastRefreshAt.Equal(clock.now) {
		t.Fatalf("main profile was not refreshed. LastRefreshAt = %v, want %v", mainProfile.LastRefreshAt, clock.now)
	}
}

// TestActiveCacheEntryPreventsStarvation_Inverse ensures that if the active profile
// is fresh, it is NOT re-fetched even if an inactive profile is stale.
func TestActiveCacheEntryPreventsStarvation_Inverse(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1000, 0)
	clock := &fakeClock{now: now}
	m := NewManager(store, NewRegistry(&fakeProvider{key: ProviderClaudeCode}))
	m.SetClock(clock)

	// Seed history
	cache := EmptyCache()
	// Active profile - fresh
	cache.Snapshots[ProviderClaudeCode] = append(cache.Snapshots[ProviderClaudeCode], CacheEntry{
		Profile: Profile{
			ProfileName: "main",
			Active:      true,
			Snapshot:    Snapshot{Status: StatusOK, LastRefreshAt: &now},
		},
	})
	// Inactive profile - stale
	staleTime := now.Add(-2 * time.Hour)
	cache.Snapshots[ProviderClaudeCode] = append(cache.Snapshots[ProviderClaudeCode], CacheEntry{
		Profile: Profile{
			ProfileName: "bkp",
			Active:      false,
			Snapshot:    Snapshot{Status: StatusOK, LastRefreshAt: &staleTime},
		},
	})

	if err := store.SaveCache(cache); err != nil {
		t.Fatal(err)
	}

	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool != "claude" {
			return nil
		}
		return []ProfileInfo{{
			Tool:           "claude",
			Name:           "main",
			Active:         true,
			HasCredentials: true,
			Email:          "main@test",
		}}
	})

	// Advance time so bkp is stale but main stays within the refresh interval
	// (default 120s): the active profile is fresh and must NOT be re-fetched.
	clock.now = now.Add(30 * time.Second)

	// Should NOT perform refresh because 'main' (active) is NOT stale
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

	var mainProfile *Profile
	for _, prov := range resp.Providers {
		if prov.ProviderName == ProviderClaudeCode {
			for i := range prov.Profiles {
				if prov.Profiles[i].ProfileName == "main" {
					p := prov.Profiles[i]
					mainProfile = &p
				}
			}
		}
	}

	if mainProfile == nil {
		t.Fatal("could not find main profile in response")
	}

	// Timestamp should NOT have updated from now
	if mainProfile.LastRefreshAt == nil || !mainProfile.LastRefreshAt.Equal(now) {
		t.Fatalf("main profile was unnecessarily refreshed. LastRefreshAt = %v, want %v", mainProfile.LastRefreshAt, now)
	}
}
