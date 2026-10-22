package switcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestUsageAlertsFollowAnAccountThroughActiveAndStoredProfiles(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("tok-main"))
	must(t, m.SetEnabled(true))
	must(t, m.Import(ToolClaude, "main"))
	must(t, m.Add(ToolClaude, "work"))
	must(t, m.EnsureUsageAlertIdentities())
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "work"), []byte(futureClaude("tok-work")), 0600))
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := usages.NewAlertTracker(t.TempDir(), func() time.Time { return now })
	must(t, err)
	reset := now.Add(24 * time.Hour)
	observe := func(activeUsed float64, inactiveName string, inactiveUsed float64) {
		t.Helper()
		now = now.Add(time.Minute)
		response := usages.Response{Providers: []usages.Provider{{ProviderName: "claude-code", Enabled: true, Available: true,
			Profiles: []usages.Profile{
				{ProfileName: "local", Active: true, Status: "ok", LastRefreshAt: &now, Windows: []usages.Window{{Key: "session", PercentUsed: activeUsed, ResetAt: &reset}}},
				{ProfileName: inactiveName, Status: "ok", LastRefreshAt: &now, Windows: []usages.Window{{Key: "session", PercentUsed: inactiveUsed, ResetAt: &reset}}},
			}}}}
		response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
		must(t, m.OverlayUsages(&response))
		if _, err := tracker.Observe(response); err != nil {
			t.Fatal(err)
		}
	}
	observe(81, "work", 10)
	if alerts := tracker.OpenAlerts(); len(alerts) != 1 || alerts[0].Profile != "main" {
		t.Fatalf("active label: %+v", alerts)
	}
	if _, switched, err := m.Switch(ToolClaude, "work"); err != nil || !switched {
		t.Fatalf("switch to work: %t %v", switched, err)
	}
	observe(82, "main", 81)
	if alerts := tracker.OpenAlerts(); len(alerts) != 2 {
		t.Fatalf("work and stored main: %+v", alerts)
	}
	if _, switched, err := m.Switch(ToolClaude, "main"); err != nil || !switched {
		t.Fatalf("switch to main: %t %v", switched, err)
	}
	observe(82, "work", 82)
	alerts := tracker.OpenAlerts()
	if len(alerts) != 2 {
		t.Fatalf("main repeated after switching back: %+v", alerts)
	}
	seen := map[string]bool{}
	for _, alert := range alerts {
		seen[alert.Profile] = true
	}
	if !seen["main"] || !seen["work"] || seen["local"] {
		t.Fatalf("account labels: %+v", alerts)
	}
}

func TestUsageAlertIdentitySurvivesRenameAndNotNameReuse(t *testing.T) {
	m, _ := newTestManager(t)
	must(t, m.Add(ToolClaude, "first"))
	must(t, m.EnsureUsageAlertIdentities())
	profiles, err := m.List()
	must(t, err)
	first := profiles[0].AlertIdentity
	if first == "" {
		t.Fatal("missing opaque identity")
	}
	if info, err := os.Stat(m.alertIdentityPath(ToolClaude, "first")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private identity file: %v %v", info, err)
	}
	must(t, m.Rename(ToolClaude, "first", "renamed"))
	profiles, err = m.List()
	must(t, err)
	if len(profiles) != 1 || profiles[0].AlertIdentity != first {
		t.Fatalf("rename: %+v", profiles)
	}
	must(t, m.Add(ToolClaude, "first"))
	must(t, m.EnsureUsageAlertIdentities())
	profiles, err = m.List()
	must(t, err)
	if len(profiles) != 2 || profiles[0].AlertIdentity == first || profiles[1].AlertIdentity != first {
		t.Fatalf("reused name: %+v", profiles)
	}
}
