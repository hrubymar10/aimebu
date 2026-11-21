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

func TestPaceAlertFiresPerEpisodeAndRearmsAfterRecovery(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(5 * time.Hour)
	eta := 2 * time.Hour
	etaSeconds := eta.Seconds()
	risk := &Pace{ExpectedPercent: 40, DeltaPercent: 30, State: "deficit", EtaSeconds: &etaSeconds}
	safe := &Pace{ExpectedPercent: 40, DeltaPercent: -30, State: "reserve", LastsToReset: true}
	response := Response{Providers: []Provider{
		{ProviderName: "codex", Enabled: true, Available: true,
			Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", LastRefreshAt: &now,
				Windows: []Window{{Key: "session", PercentUsed: 70, ResetAt: &reset, Pace: risk}}}}},
		{ProviderName: "mistral", Enabled: true, Available: true,
			Profiles: []Profile{{ProfileName: "local", Status: "ok", LastRefreshAt: &now,
				Windows: []Window{{Key: "local", PercentUsed: 70, ResetAt: &reset, Pace: risk}}}}},
	}}
	response.Settings.AlertPaceEnabled = true
	observe := func(want bool) {
		t.Helper()
		now = now.Add(time.Minute)
		for i := range response.Providers {
			response.Providers[i].Profiles[0].LastRefreshAt = &now
		}
		got, err := tracker.Observe(response)
		if err != nil || got != want {
			t.Fatalf("observe: created=%v error=%v, want %v", got, err, want)
		}
	}
	observe(true)
	open := tracker.OpenAlerts()
	if len(open) != 1 || open[0].Tier != "pace" || open[0].Provider != "codex" || open[0].Window != "session" {
		t.Fatalf("pace alert: %+v", open)
	}
	if open[0].Remaining != 30 || open[0].ResetAt == nil || !open[0].ResetAt.Equal(reset) {
		t.Fatalf("pace alert detail: %+v", open[0])
	}
	observe(false) // still the same episode: no second alert
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].ID != open[0].ID {
		t.Fatalf("episode must hold one alert: %+v", got)
	}
	// Recovery closes the episode; a new risk re-arms it in the same cycle.
	response.Providers[0].Profiles[0].Windows[0].Pace = safe
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("recovery must close the pace alert: %+v", got)
	}
	response.Providers[0].Profiles[0].Windows[0].Pace = risk
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "pace" {
		t.Fatalf("a recovered episode re-arms: %+v", got)
	}
	// Off again closes what it opened.
	response.Settings.AlertPaceEnabled = false
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("the pace toggle must close its alerts: %+v", got)
	}
}

func TestResetNoticeOncePerCycle(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(5 * time.Hour)
	response := Response{Providers: []Provider{{ProviderName: "claude-code", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "weekly", PercentUsed: 10, ResetAt: &reset}}}}}}}
	response.Settings.AlertResetNoticeEnabled = true
	observe := func(want bool) {
		t.Helper()
		now = now.Add(time.Minute)
		response.Providers[0].Profiles[0].LastRefreshAt = &now
		got, err := tracker.Observe(response)
		if err != nil || got != want {
			t.Fatalf("observe: created=%v error=%v, want %v", got, err, want)
		}
	}
	observe(false) // the first observation starts a cycle, it does not announce one
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("no notice for the first observation: %+v", got)
	}
	reset = reset.Add(5 * time.Hour)
	response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
	response.Providers[0].Profiles[0].Windows[0].PercentUsed = 1
	observe(true)
	open := tracker.OpenAlerts()
	if len(open) != 1 || open[0].Tier != "reset" || open[0].Window != "weekly" {
		t.Fatalf("reset notice: %+v", open)
	}
	observe(false) // the same cycle never repeats it
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].ID != open[0].ID {
		t.Fatalf("one notice per cycle: %+v", got)
	}
	// The next cycle closes the old notice and raises a fresh one.
	reset = reset.Add(7 * 24 * time.Hour)
	response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
	observe(true)
	got := tracker.OpenAlerts()
	if len(got) != 1 || got[0].ID == open[0].ID {
		t.Fatalf("the next cycle replaces the notice: %+v", got)
	}
	// Off again closes what it opened.
	response.Settings.AlertResetNoticeEnabled = false
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("the reset toggle must close its notices: %+v", got)
	}
}

func TestExpiryAlertOncePerCredentialUntilRenewed(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(10 * time.Minute)
	response := Response{Providers: []Provider{{ProviderName: "claude-code", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", ExpiresAt: &expires, AlertCredential: "cred-1"}}}}}
	response.Settings.AlertExpiryEnabled = true
	observe := func(want bool) {
		t.Helper()
		now = now.Add(time.Minute)
		got, err := tracker.Observe(response)
		if err != nil || got != want {
			t.Fatalf("observe: created=%v error=%v, want %v", got, err, want)
		}
	}
	observe(true)
	open := tracker.OpenAlerts()
	if len(open) != 1 || open[0].Tier != "expiring" || open[0].ExpiresAt == nil || !open[0].ExpiresAt.Equal(expires) {
		t.Fatalf("expiry alert: %+v", open)
	}
	observe(false) // one notice per credential and expiry
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].ID != open[0].ID {
		t.Fatalf("one expiry notice per credential: %+v", got)
	}
	// A renewed credential closes the notice; a later decay raises a new one.
	renewed := now.Add(30 * 24 * time.Hour)
	response.Providers[0].Profiles[0].ExpiresAt = &renewed
	response.Providers[0].Profiles[0].AlertCredential = "cred-2"
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("a renewal must close the expiry notice: %+v", got)
	}
	decayed := now.Add(5 * time.Minute)
	response.Providers[0].Profiles[0].ExpiresAt = &decayed
	response.Providers[0].Profiles[0].AlertCredential = "cred-2"
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].ID == open[0].ID {
		t.Fatalf("a new decay re-arms: %+v", got)
	}
	// Off again closes what it opened.
	response.Settings.AlertExpiryEnabled = false
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("the expiry toggle must close its notices: %+v", got)
	}
}

func TestExpiryAlertSurvivesASaveFailure(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	tracker, err := NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(10 * time.Minute)
	response := Response{Providers: []Provider{{ProviderName: "claude-code", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", ExpiresAt: &expires, AlertCredential: "cred-1"}}}}}
	response.Settings.AlertExpiryEnabled = true
	locked := filepath.Join(root, "alerts.json")
	if err := os.Mkdir(locked, 0700); err != nil { // a directory where the state file must be written: every save fails
		t.Fatal(err)
	}
	if changed, err := tracker.Observe(response); err == nil {
		t.Fatalf("the failing save must surface: changed=%v", changed)
	}
	if got := len(tracker.OpenAlerts()); got != 0 {
		t.Fatalf("nothing is open while the save failed: %+v", tracker.OpenAlerts())
	}
	// The save failure rolled the state back; observing the same expiry again
	// must not panic on a nil Expiry map.
	os.Remove(locked)
	if _, err := tracker.Observe(response); err != nil {
		t.Fatal(err)
	}
	open := tracker.OpenAlerts()
	if len(open) != 1 || open[0].Tier != "expiring" {
		t.Fatalf("the expiry alert survived the save failure: %+v", open)
	}
}

func TestAlertTogglesCloseOpenAlertsWithoutARefresh(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(5 * time.Hour)
	etaSeconds := 2 * 3600.0
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 70, ResetAt: &reset,
				Pace: &Pace{ExpectedPercent: 40, DeltaPercent: 30, State: "deficit", EtaSeconds: &etaSeconds}}}}}}}}
	response.Settings.AlertPaceEnabled = true
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("initial: %v %v", changed, err)
	}
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "pace" {
		t.Fatalf("pace alert: %+v", got)
	}
	// Off without advancing LastRefreshAt: the open pace alerts close at once.
	response.Settings.AlertPaceEnabled = false
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("toggling off must be a visible change: %v %v", changed, err)
	}
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("the pace toggle must close its alerts at once: %+v", got)
	}
}

func TestExpiryAlertWordTurnsToExpired(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(10 * time.Minute)
	response := Response{Providers: []Provider{{ProviderName: "claude-code", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", ExpiresAt: &expires, AlertCredential: "cred-1"}}}}}
	response.Settings.AlertExpiryEnabled = true
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("initial: %v %v", changed, err)
	}
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "expiring" {
		t.Fatalf("expiring: %+v", got)
	}
	// The clock passes the expiry without a new snapshot: the wording turns to expired.
	now = now.Add(11 * time.Minute)
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("expired: %v %v", changed, err)
	}
	got := tracker.OpenAlerts()
	if len(got) != 1 || got[0].Tier != "expired" {
		t.Fatalf("the notice must say expired once the expiry passed: %+v", got)
	}
}
