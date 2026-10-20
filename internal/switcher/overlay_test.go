package switcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hrubymar10/aimebu/internal/usages"
)

func TestReadOnlyManagerNeverWritesProfileMeta(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "main"))
	must(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"live@example.com"}}`), 0o600))
	metaPath := m.profileMetaPath(ToolClaude, "main")
	if _, err := os.Stat(metaPath); err == nil {
		t.Fatal("fixture already has profile metadata")
	}

	ro := &Manager{root: m.root, homeDir: home, readOnly: true}
	resp := usages.Response{Providers: []usages.Provider{{ProviderName: "claude-code", Profiles: []usages.Profile{{ProfileName: "local", Active: true, Status: "ok"}}}}}
	must(t, ro.OverlayUsages(&resp))
	if got := resp.Providers[0].Profiles; len(got) != 1 || got[0].ProfileName != "main" || got[0].Email != "live@example.com" {
		t.Fatalf("overlay profiles = %+v", got)
	}
	if _, err := os.Stat(metaPath); err == nil {
		t.Fatal("read-only overlay wrote profile metadata")
	}

	if _, err := m.List(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("a normal List should record the live email (control for this test): %v", err)
	}
}
