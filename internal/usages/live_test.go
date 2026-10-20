package usages

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

func TestUpdateHookFiresOnSnapshotChange(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.SetUpdateHook(func() { calls++ })
	r.Invalidate(providers.ProviderCodex)
	if calls != 1 {
		t.Fatalf("Invalidate calls = %d, want 1", calls)
	}
	r.ConfigChanged(providers.ProviderMistral, true, true)
	if calls != 2 {
		t.Fatalf("ConfigChanged calls = %d, want 2", calls)
	}
}

func TestForceCooldownIsGlobalAndExpires(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if retry := r.CheckForceCooldown(); retry != 0 {
		t.Fatalf("first force retry = %d, want 0", retry)
	}
	retry := r.CheckForceCooldown()
	if retry < 1 || retry > int(MinForceCooldown/time.Second) {
		t.Fatalf("second force retry = %d, want 1..%d", retry, int(MinForceCooldown/time.Second))
	}
	r.forceMu.Lock()
	r.lastForce = time.Now().Add(-MinForceCooldown)
	r.forceMu.Unlock()
	if retry := r.CheckForceCooldown(); retry != 0 {
		t.Fatalf("force after cooldown retry = %d, want 0", retry)
	}
}

func TestReadCacheKeepsAllProvidersAndConfig(t *testing.T) {
	dir := t.TempDir()
	resp := defaultResponse()
	resp.Providers[1].Profiles = []Profile{{ProfileName: "local", Active: true, Status: "ok", Plan: "Max", Windows: []Window{{Key: "session", PercentUsed: 40}}}}
	resp.Providers[3].Profiles = []Profile{{ProfileName: "local", Active: true, Status: "ok", Plan: "Pro", HasCredentials: true}}
	data, _ := json.Marshal(resp)
	if err := os.WriteFile(filepath.Join(dir, "cache.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"percent_display":"used","providers":{"mistral":{"enabled":true,"cookie":"c=1"},"codex":{"enabled":false}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	got, written, err := ReadCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if written.IsZero() {
		t.Fatal("cache write time is zero")
	}
	byName := map[string]Provider{}
	for _, p := range got.Providers {
		byName[p.ProviderName] = p
	}
	if p := byName["claude-code"].Profiles[0]; p.Plan != "Max" || len(p.Windows) != 1 || p.Stale {
		t.Fatalf("claude profile = %+v", p)
	}
	if p := byName["mistral"]; !p.Enabled || p.Profiles[0].Plan != "Pro" {
		t.Fatalf("mistral = %+v", p)
	}
	if p := byName["codex"]; p.Enabled || p.Profiles[0].Status != "disabled" {
		t.Fatalf("codex = %+v", p)
	}
	if got.Settings.PercentDisplay != "used" {
		t.Fatalf("percent display = %q", got.Settings.PercentDisplay)
	}
}

func TestReadCacheWithoutFilesIsDefaultAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	got, written, err := ReadCache(dir)
	if err != nil || !written.IsZero() || len(got.Providers) != 5 {
		t.Fatalf("ReadCache = %+v, %v, %v", got, written, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("ReadCache wrote %d files", len(entries))
	}
}
