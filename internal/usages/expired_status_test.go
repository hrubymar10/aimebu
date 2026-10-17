package usages

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// An access token past its expiry is never sent: Anthropic answers an expired
// bearer with HTTP 429, which the retry loop used to turn into minutes of
// "timeout". The profile reports `expired`, keeps its last windows (stale), and
// exposes expires_at, for stored and live (active) rows alike.
func TestExpiredTokensAreNotFetched(t *testing.T) {
	now := time.Unix(2_000_000, 0).UTC()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	reset := now.Add(2 * time.Hour)
	lastGood := now.Add(-3 * time.Hour)

	cases := []struct {
		name        string
		tool, key   string
		active      bool
		expiresAt   *time.Time
		wantFetch   bool
		wantStatus  Status
		wantStale   bool
		wantWindows int
	}{
		{"stored claude expired", "claude", ProviderClaudeCode, false, &past, false, StatusExpired, true, 1},
		{"live claude expired", "claude", ProviderClaudeCode, true, &past, false, StatusExpired, true, 1},
		{"stored codex expired", "codex", ProviderCodex, false, &past, false, StatusExpired, true, 1},
		{"live codex expired", "codex", ProviderCodex, true, &past, false, StatusExpired, true, 1},
		{"expiring exactly now", "claude", ProviderClaudeCode, false, &now, false, StatusExpired, true, 1},
		{"stored claude valid", "claude", ProviderClaudeCode, false, &future, true, StatusOK, false, 0},
		{"live claude valid", "claude", ProviderClaudeCode, true, &future, true, StatusOK, false, 0},
		{"unknown expiry is fetched", "claude", ProviderClaudeCode, false, nil, true, StatusOK, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var storedCalls atomic.Int32
			oldClaude, oldCodex := fetchClaudeSnapshotFromPathFunc, fetchCodexSnapshotFromPathFunc
			t.Cleanup(func() { fetchClaudeSnapshotFromPathFunc, fetchCodexSnapshotFromPathFunc = oldClaude, oldCodex })
			ok := func(context.Context, string) (Snapshot, error) {
				storedCalls.Add(1)
				return Snapshot{Status: StatusOK}, nil
			}
			fetchClaudeSnapshotFromPathFunc, fetchCodexSnapshotFromPathFunc = ok, ok

			store := NewStoreAt(t.TempDir())
			cfg := DefaultConfig()
			cfg.Providers[tc.key] = ProviderConfig{Enabled: true}
			if err := store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			provider := &fakeProvider{key: tc.key}
			m := NewManager(store, NewRegistry(provider))
			m.SetClock(&fakeClock{now: now})
			cache := EmptyCache()
			cache.Snapshots[tc.key] = []CacheEntry{{Profile: Profile{
				ProfileName: "main", Active: tc.active,
				Snapshot: Snapshot{Status: StatusOK, LastRefreshAt: &lastGood, Windows: []Window{{Key: "session", PercentUsed: 40, ResetAt: &reset}}},
			}}}
			if err := store.SaveCache(cache); err != nil {
				t.Fatal(err)
			}
			credPath := filepathJoin(t, tc.tool)
			m.SetProfileLister(func(tool string) ([]ProfileInfo, error) {
				if tool != tc.tool {
					return nil, nil
				}
				return []ProfileInfo{{Tool: tc.tool, Name: "main", Active: tc.active, CredPath: credPath, HasCredentials: true, ExpiresAt: tc.expiresAt}}, nil
			})

			if _, _, err := m.refresh(context.Background(), tc.key, true); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			calls := int(storedCalls.Load()) + int(provider.calls)
			if tc.wantFetch && calls == 0 {
				t.Fatal("a valid token was not fetched")
			}
			if !tc.wantFetch && calls != 0 {
				t.Fatalf("network fetch happened with an expired token (%d calls)", calls)
			}
			resp, err := m.Snapshot(context.Background(), tc.key)
			if err != nil {
				t.Fatal(err)
			}
			var got *Profile
			for _, prov := range resp.Providers {
				if prov.ProviderName != tc.key {
					continue
				}
				for i := range prov.Profiles {
					if prov.Profiles[i].ProfileName == "main" {
						p := prov.Profiles[i]
						got = &p
					}
				}
			}
			if got == nil {
				t.Fatal("profile missing from the response")
			}
			if got.Status != tc.wantStatus || got.Stale != tc.wantStale || len(got.Windows) != tc.wantWindows {
				t.Fatalf("profile = status %q stale %v windows %d, want %q %v %d", got.Status, got.Stale, len(got.Windows), tc.wantStatus, tc.wantStale, tc.wantWindows)
			}
			if tc.expiresAt != nil && (got.ExpiresAt == nil || !got.ExpiresAt.Equal(*tc.expiresAt)) {
				t.Fatalf("expires_at = %v, want %v", got.ExpiresAt, tc.expiresAt)
			}
			if tc.wantStatus == StatusExpired && got.Error == "" {
				t.Fatal("expired profile has no explanation")
			}
		})
	}
}

// Repeated ticks keep the last good windows: expiry does not erase the quota.
func TestExpiredEntryKeepsWindowsAcrossTicks(t *testing.T) {
	now := time.Unix(2_000_000, 0).UTC()
	past := now.Add(-time.Minute)
	prev := CacheEntry{Profile: Profile{ProfileName: "main", Snapshot: Snapshot{Status: StatusOK, Windows: []Window{{Key: "weekly", PercentUsed: 70}}}}}
	profile := ProfileInfo{Tool: "claude", Name: "main", ExpiresAt: &past}
	first, ok := expiredEntry(ProviderClaudeCode, profile, prev, now)
	if !ok || first.Profile.Status != StatusExpired || !first.Profile.Stale || len(first.Profile.Windows) != 1 {
		t.Fatalf("first = %+v ok=%v", first.Profile, ok)
	}
	second, ok := expiredEntry(ProviderClaudeCode, profile, first, now.Add(time.Minute))
	if !ok || len(second.Profile.Windows) != 1 || !second.Profile.Stale {
		t.Fatalf("second tick lost the windows: %+v", second.Profile)
	}
	if _, ok := expiredEntry(ProviderGitHubCopilot, profile, prev, now); ok {
		t.Fatal("a non-OAuth provider must never report expired")
	}
	empty, ok := expiredEntry(ProviderCodex, ProfileInfo{Tool: "codex", Name: "x", ExpiresAt: &past}, CacheEntry{}, now)
	if !ok || empty.Profile.Status != StatusExpired || empty.Profile.Stale || len(empty.Profile.Windows) != 0 {
		t.Fatalf("no previous snapshot: %+v", empty.Profile)
	}
}

func filepathJoin(t *testing.T, tool string) string {
	t.Helper()
	return t.TempDir() + "/" + tool + ".json"
}
