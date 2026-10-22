package usages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageAlertsSurviveRestartAndRearmOnReset(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(24 * time.Hour)
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "private@example.test", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 79, ResetAt: &reset}}}}}}}
	response.Settings.AlertWarningRemaining = 20
	response.Settings.AlertCriticalRemaining = 10
	observe := func(want bool) {
		t.Helper()
		got, err := tracker.Observe(response)
		if err != nil || got != want {
			t.Fatalf("observe: created=%v error=%v, want %v", got, err, want)
		}
	}
	observe(false)
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	response.Providers[0].Profiles[0].Windows[0].PercentUsed = 81
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "warning" || got[0].Remaining != 19 {
		t.Fatalf("warning: %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "alerts.json"))
	if err != nil || strings.Contains(string(data), "private@example.test") {
		t.Fatalf("alert state must be durable without email: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "alerts.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private alert file: %v %v", info, err)
	}
	tracker, err = NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observe(false)
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	response.Providers[0].Profiles[0].Windows[0].PercentUsed = 91
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "critical" || got[0].Remaining != 9 {
		t.Fatalf("critical replaces warning: %+v", got)
	}
	// Switching away and back to the same account cannot repeat the tier.
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].ProfileName = "backup"
	response.Providers[0].Profiles[0].Email = "other@example.test"
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	observe(true)
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].ProfileName = "main"
	response.Providers[0].Profiles[0].Email = "private@example.test"
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("missing accounts leave no open alert: %+v", got)
	}
	// A stale reading neither creates an alert nor resets the cycle.
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].Stale = true
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	response.Providers[0].Profiles[0].Windows[0].PercentUsed = 0
	observe(false)
	response.Providers[0].Profiles[0].Stale = false
	reset = reset.Add(24 * time.Hour)
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
	observe(false)
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	response.Providers[0].Profiles[0].Windows[0].PercentUsed = 82
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "warning" {
		t.Fatalf("new reset cycle: %+v", got)
	}
}

func TestUsageAlertRenameAndReusedName(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(24 * time.Hour)
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "old", Email: "same@example.test", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 81, ResetAt: &reset}}}}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("initial: %v %v", changed, err)
	}
	original := tracker.OpenAlerts()[0].ID
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].ProfileName = "renamed"
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	if _, err := tracker.Observe(response); err != nil {
		t.Fatal(err)
	}
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].ID != original || got[0].Profile != "renamed" {
		t.Fatalf("rename changed account alert: %+v", got)
	}
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].ProfileName = "old"
	response.Providers[0].Profiles[0].Email = "other@example.test"
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("reused name: %v %v", changed, err)
	}
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].ID == original || got[0].Profile != "old" {
		t.Fatalf("reused name inherited account state: %+v", got)
	}
}

func TestUsageAlertIdentityIgnoresLateEmail(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(24 * time.Hour)
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", AlertIdentity: "opaque-profile", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 81, ResetAt: &reset}}}}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("initial: %v %v", changed, err)
	}
	id := tracker.OpenAlerts()[0].ID
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].Email = "newly-known@example.test"
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	if changed, err := tracker.Observe(response); err != nil || changed {
		t.Fatalf("late email changed alert: %v %v", changed, err)
	}
	if alerts := tracker.OpenAlerts(); len(alerts) != 1 || alerts[0].ID != id {
		t.Fatalf("late email duplicated alert: %+v", alerts)
	}
}

func TestLocalUsageAlertIdentityIgnoresLateEmail(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "github-copilot", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "local", AlertCredential: alertHash("stable-token"), Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "monthly", PercentUsed: 81}}}}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("initial: %v %v", changed, err)
	}
	id := tracker.OpenAlerts()[0].ID
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].Email = "newly-known@example.test"
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	if changed, err := tracker.Observe(response); err != nil || changed {
		t.Fatalf("late local email changed alert: %v %v", changed, err)
	}
	if alerts := tracker.OpenAlerts(); len(alerts) != 1 || alerts[0].ID != id {
		t.Fatalf("late local email duplicated alert: %+v", alerts)
	}
}

func TestUsageAlertCredentialReplacementRearmsSameCycle(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "mistral", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "local", AlertCredential: alertHash("cookie-one"), Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "monthly", PercentUsed: 81}}}}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("first cookie: %v %v", changed, err)
	}
	initial := tracker.OpenAlerts()[0]
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].AlertCredential = alertHash("cookie-two")
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("replacement cookie: %v %v", changed, err)
	}
	if alerts := tracker.OpenAlerts(); len(alerts) != 1 || alerts[0].ID == initial.ID || !alerts[0].CreatedAt.After(initial.CreatedAt) {
		t.Fatalf("replacement retained old fired state: %+v", alerts)
	}
	if err := tracker.Dismiss(initial.ID); err != ErrAlertNotFound {
		t.Fatalf("old card must not dismiss replacement: %v", err)
	}
}

func TestUsageAlertFirstCredentialObservationRekeysOpenAlert(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "local", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 81}}}}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("old observation: %v %v", changed, err)
	}
	oldID := tracker.OpenAlerts()[0].ID
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	response.Providers[0].Profiles[0].AlertCredential = alertHash("new-live-token")
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("first credential observation: %v %v", changed, err)
	}
	if alerts := tracker.OpenAlerts(); len(alerts) != 1 || alerts[0].ID == oldID {
		t.Fatalf("open alert was not re-keyed: %+v", alerts)
	}
}

func TestUsageAlertsCloseAndDismiss(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(time.Hour)
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "local", Email: "first@example.test", AlertCredential: alertHash("first-login"), Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 81, ResetAt: &reset}}}}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observe := func() {
		t.Helper()
		if _, err := tracker.Observe(response); err != nil {
			t.Fatal(err)
		}
	}
	observe()
	id := tracker.OpenAlerts()[0].ID
	if err := tracker.Dismiss(id); err != nil {
		t.Fatal(err)
	}
	if len(tracker.OpenAlerts()) != 0 {
		t.Fatal("dismissed alert visible")
	}
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	observe()
	if len(tracker.OpenAlerts()) != 0 {
		t.Fatal("dismissed tier rearmed in same cycle")
	}
	response.Providers[0].Profiles[0].Email = "second@example.test"
	response.Providers[0].Profiles[0].AlertCredential = alertHash("second-login")
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	observe()
	if len(tracker.OpenAlerts()) != 1 {
		t.Fatal("changed local account did not alert")
	}
	response.Providers[0].Enabled = false
	observe()
	if len(tracker.OpenAlerts()) != 0 {
		t.Fatal("disabled provider retained alert")
	}
	response.Providers[0].Enabled = true
	response.Providers[0].Profiles = nil
	observe()
	if len(tracker.OpenAlerts()) != 0 {
		t.Fatal("removed profile retained alert")
	}
	response.Providers[0].Profiles = []Profile{{ProfileName: "third", Email: "third@example.test", Status: "ok", LastRefreshAt: &now,
		Windows: []Window{{Key: "session", PercentUsed: 81, ResetAt: &reset}}}}
	observe()
	if len(tracker.OpenAlerts()) != 1 {
		t.Fatal("third account did not alert")
	}
	now = reset.Add(time.Second)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	observe()
	if len(tracker.OpenAlerts()) != 0 {
		t.Fatal("reset passed but alert remained")
	}
}

func TestUsageAlertWithoutResetTimeRearmsAfterObservedDrop(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "mistral", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "local", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "monthly", PercentUsed: 81}}}}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observe := func(used float64, want bool) {
		t.Helper()
		now = now.Add(time.Minute)
		response.Providers[0].Profiles[0].LastRefreshAt = &now
		response.Providers[0].Profiles[0].Windows[0].PercentUsed = used
		changed, err := tracker.Observe(response)
		if err != nil || changed != want {
			t.Fatalf("used %.0f: changed=%v err=%v, want %v", used, changed, err, want)
		}
	}
	observe(81, true)
	observe(82, false)
	observe(20, true)
	observe(81, true)
	alerts := tracker.OpenAlerts()
	if len(alerts) != 1 || alerts[0].Remaining != 19 {
		t.Fatalf("new cycle alert: %+v", alerts)
	}
}

func TestUsageAlertResetJitterDoesNotRearm(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(24*time.Hour + 50*time.Second)
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", AlertIdentity: "stable-profile", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 81, ResetAt: &reset}}}}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("initial: %v %v", changed, err)
	}
	now = now.Add(time.Minute)
	reset = reset.Add(90 * time.Second)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
	if changed, err := tracker.Observe(response); err != nil || changed {
		t.Fatalf("minute boundary jitter rearmed: %v %v", changed, err)
	}
	// A state written by the earlier exact-timestamp implementation also
	// stays in this cycle after the next refresh.
	for key, state := range tracker.state.Windows {
		state.Cycle = reset.UTC().Format(time.RFC3339Nano)
		state.ResetAt = time.Time{}
		tracker.state.Windows[key] = state
	}
	now = now.Add(time.Minute)
	reset = reset.Add(90 * time.Second)
	response.Providers[0].Profiles[0].LastRefreshAt = &now
	response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
	if changed, err := tracker.Observe(response); err != nil || changed {
		t.Fatalf("legacy timestamp jitter rearmed: %v %v", changed, err)
	}
	if len(tracker.OpenAlerts()) != 1 {
		t.Fatalf("alerts: %+v", tracker.OpenAlerts())
	}
}

func TestCorruptUsageAlertsAreMovedAside(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "alerts.json")
	if err := os.WriteFile(path, []byte(`{"windows":`), 0600); err != nil {
		t.Fatal(err)
	}
	tracker, err := NewAlertTracker(root)
	if err != nil {
		t.Fatalf("corrupt alerts stopped startup: %v", err)
	}
	if len(tracker.OpenAlerts()) != 0 {
		t.Fatalf("corrupt state survived: %+v", tracker.OpenAlerts())
	}
	backups, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("moved files: %v %v", backups, err)
	}
	data, err := os.ReadFile(backups[0])
	if err != nil || string(data) != `{"windows":` {
		t.Fatalf("saved corrupt state: %q %v", data, err)
	}
}
