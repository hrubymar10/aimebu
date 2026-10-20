package usages

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

func TestConfigChangedOnlyResetsAffectedProvider(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.response.Providers[2].Profiles[0] = Profile{ProfileName: "local", Active: true, Status: "ok", Plan: "Copilot"}
	r.response.Providers[3].Profiles[0] = Profile{ProfileName: "local", Active: true, Status: "ok", Plan: "Mistral"}
	r.response.Providers[4].Profiles[0] = Profile{ProfileName: "local", Active: true, Status: "ok", Plan: "Ollama"}
	r.extraOwners[providers.ProviderGitHubCopilot] = "copilot-owner"
	r.extraOwners[providers.ProviderMistral] = "mistral-owner"
	r.ConfigChanged(providers.ProviderMistral, true, true)
	if r.response.Providers[2].Profiles[0].Status != "ok" || r.response.Providers[4].Profiles[0].Status != "ok" {
		t.Fatal("unrelated provider snapshots were reset")
	}
	if r.response.Providers[3].Profiles[0].Status != "refreshing" || r.extraOwners[providers.ProviderMistral] != "" || r.extraOwners[providers.ProviderGitHubCopilot] != "copilot-owner" {
		t.Fatal("affected provider was not reset in isolation")
	}
	if len(r.pendingExtra) != 1 || !r.pendingExtra[providers.ProviderMistral] {
		t.Fatalf("pending refreshes = %+v", r.pendingExtra)
	}
	r.ConfigChanged(providers.ProviderMistral, false, false)
	if r.response.Providers[3].Profiles[0].Status != "disabled" {
		t.Fatalf("disabled provider status = %q", r.response.Providers[3].Profiles[0].Status)
	}
}

func TestMalformedConfigKeepsDisabledLocalProviders(t *testing.T) {
	root := t.TempDir()
	config := providers.NewStore(root)
	if _, err := config.UpdateConfig(func(cfg *providers.Config) error {
		cfg.Providers[providers.ProviderCodex] = providers.ProviderConfig{Enabled: false}
		cfg.Providers[providers.ProviderClaudeCode] = providers.ProviderConfig{Enabled: false}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	// A valid file with a wrong field type must not turn the last known
	// disabled state back into an enabled default during a refresh.
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"percent_display":{},"providers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r.Refresh(context.Background())
	snapshot := r.Snapshot()
	for _, provider := range snapshot.Providers[:2] {
		if provider.Enabled || provider.Profiles[0].Status != "disabled" {
			t.Errorf("%s after invalid config: enabled=%v, status=%q", provider.ProviderName, provider.Enabled, provider.Profiles[0].Status)
		}
	}
}
