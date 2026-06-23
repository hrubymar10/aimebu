package usages

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStoreDefaultsAndModes(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	now := time.Now().UTC()
	cache := EmptyCache()
	cache.Snapshots[ProviderCodex] = CacheEntry{
		Snapshot:      Snapshot{Provider: ProviderCodex, Status: StatusOK, Error: "token-secret leaked"},
		LastRefreshAt: &now,
	}
	cfg.Providers[ProviderCodex] = ProviderConfig{Token: "token-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig token: %v", err)
	}
	if err := store.SaveCache(cache); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	assertMode(t, store.ConfigPath(), 0o600)
	assertMode(t, store.CachePath(), 0o644)
	data, err := os.ReadFile(store.CachePath())
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if strings.Contains(string(data), "token-secret") {
		t.Fatalf("cache contains secret: %s", data)
	}
	if !strings.Contains(string(data), redacted) {
		t.Fatalf("cache did not contain redaction marker: %s", data)
	}
}

func TestNormalizeProviderOrder(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil defaults canonical", in: nil, want: knownProviders},
		{name: "empty defaults canonical", in: []string{}, want: knownProviders},
		{
			name: "custom order",
			in:   []string{ProviderOllamaCloud, ProviderCodex, ProviderClaudeCode, ProviderGitHubCopilot},
			want: []string{ProviderOllamaCloud, ProviderCodex, ProviderClaudeCode, ProviderGitHubCopilot, ProviderMistral},
		},
		{
			name: "drops unknowns and appends missing",
			in:   []string{"bogus", ProviderOllamaCloud, ProviderCodex},
			want: []string{ProviderOllamaCloud, ProviderCodex, ProviderClaudeCode, ProviderGitHubCopilot, ProviderMistral},
		},
		{
			name: "dedupes first occurrence",
			in:   []string{ProviderClaudeCode, ProviderCodex, ProviderClaudeCode, ProviderOllamaCloud},
			want: []string{ProviderClaudeCode, ProviderCodex, ProviderOllamaCloud, ProviderGitHubCopilot, ProviderMistral},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeProviderOrder(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizeProviderOrder(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestStoreNormalizesProviderOrder(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.ProviderOrder = []string{"unknown", ProviderOllamaCloud, ProviderCodex, ProviderOllamaCloud}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := store.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{ProviderOllamaCloud, ProviderCodex, ProviderClaudeCode, ProviderGitHubCopilot, ProviderMistral}
	if !reflect.DeepEqual(got.ProviderOrder, want) {
		t.Fatalf("ProviderOrder = %v, want %v", got.ProviderOrder, want)
	}
}

func TestProviderInfosRespectsProviderOrder(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProviderOrder = []string{ProviderOllamaCloud, ProviderGitHubCopilot, ProviderClaudeCode, ProviderCodex, ProviderMistral}
	infos := ProviderInfos(cfg, DefaultRegistry())
	var got []string
	for _, info := range infos {
		got = append(got, info.Key)
	}
	if !reflect.DeepEqual(got, cfg.ProviderOrder) {
		t.Fatalf("ProviderInfos order = %v, want %v", got, cfg.ProviderOrder)
	}
}

func TestRefreshIntervalEnvOverride(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.RefreshIntervalSec = 60
	t.Setenv(EnvRefreshInterval, "3")
	d, info := store.RefreshInterval(cfg)
	if d != MinRefreshSec*time.Second {
		t.Fatalf("duration = %v", d)
	}
	if !info.EnvOverride || info.RefreshIntervalSec != MinRefreshSec {
		t.Fatalf("info = %+v", info)
	}
}

func TestLockFileIsStable(t *testing.T) {
	root := t.TempDir()
	store := NewStoreAt(root)
	if err := store.WithLock(func() error { return nil }); err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	assertMode(t, filepath.Join(root, ".lock"), 0o644)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
