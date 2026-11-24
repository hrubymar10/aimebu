package usages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
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
	observe(false) // the first risky reading is data-poor: no alert yet
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("the first risky reading must not raise: %+v", got)
	}
	now = now.Add(11 * time.Minute)
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
	observe(false) // the hysteresis holds the re-arm
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("the hysteresis must hold the re-arm: %+v", got)
	}
	now = now.Add(11 * time.Minute)
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "pace" {
		t.Fatalf("a recovered episode re-arms after the hysteresis: %+v", got)
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
	// A reset notice is good news, not a task: it is a toast (Notices),
	// never an Inbox item (OpenAlerts).
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("a reset notice must not be an Inbox alert: %+v", got)
	}
	open := tracker.Notices()
	if len(open) != 1 || open[0].Tier != "reset" || open[0].Window != "weekly" {
		t.Fatalf("reset notice: %+v", open)
	}
	observe(false) // the same cycle never repeats it
	if got := tracker.Notices(); len(got) != 1 || got[0].ID != open[0].ID {
		t.Fatalf("one notice per cycle: %+v", got)
	}
	// The next cycle closes the old notice and raises a fresh one: the
	// previous reset time has passed and the new one is later.
	now = reset.Add(time.Minute)
	reset = reset.Add(7 * 24 * time.Hour)
	response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
	observe(true)
	got := tracker.Notices()
	if len(got) != 1 || got[0].ID == open[0].ID {
		t.Fatalf("the next cycle replaces the notice: %+v", got)
	}
	// Off again closes what it opened.
	response.Settings.AlertResetNoticeEnabled = false
	observe(true)
	if got := tracker.Notices(); len(got) != 0 {
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
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", ExpiresAt: &expires, AlertCredential: "cred-1", ToolMigrated: true}}}}}
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
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", ExpiresAt: &expires, AlertCredential: "cred-1", ToolMigrated: true}}}}}
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
	if _, err := tracker.Observe(response); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute) // the hysteresis: the first risky reading does not raise
	response.Providers[0].Profiles[0].LastRefreshAt = &now
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
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", ExpiresAt: &expires, AlertCredential: "cred-1", ToolMigrated: true}}}}}
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

func TestExpiryAlertDismissalSurvivesUntilItEscalates(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(10 * time.Minute)
	response := Response{Providers: []Provider{{ProviderName: "claude-code", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", ExpiresAt: &expires, AlertCredential: "cred-1", ToolMigrated: true}}}}}
	response.Settings.AlertExpiryEnabled = true
	observe := func(want bool) {
		t.Helper()
		got, err := tracker.Observe(response)
		if err != nil || got != want {
			t.Fatalf("observe: created=%v error=%v, want %v", got, err, want)
		}
	}
	observe(true)
	open := tracker.OpenAlerts()
	if len(open) != 1 || open[0].Tier != "expiring" {
		t.Fatalf("expiring: %+v", open)
	}
	// Dismissed stays dismissed: the same expiry at the same level raises nothing again.
	if err := tracker.Dismiss(open[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("dismissed: %+v", got)
	}
	observe(false)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("a dismissed expiry must not come back at its level: %+v", got)
	}
	// The level strengthens to expired: raised once, at the stronger wording.
	now = now.Add(11 * time.Minute)
	observe(true)
	got := tracker.OpenAlerts()
	if len(got) != 1 || got[0].Tier != "expired" {
		t.Fatalf("the stronger level must be raised once: %+v", got)
	}
	// And it, too, stays dismissed once dismissed.
	if err := tracker.Dismiss(got[0].ID); err != nil {
		t.Fatal(err)
	}
	observe(false)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("the dismissed stronger notice must stay dismissed: %+v", got)
	}
}

func TestPaceAlertOffThenOnCanRaiseAgain(t *testing.T) {
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
	observe := func(want bool) {
		t.Helper()
		now = now.Add(time.Minute)
		response.Providers[0].Profiles[0].LastRefreshAt = &now
		got, err := tracker.Observe(response)
		if err != nil || got != want {
			t.Fatalf("observe: created=%v error=%v, want %v", got, err, want)
		}
	}
	observe(false)
	now = now.Add(11 * time.Minute)
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "pace" {
		t.Fatalf("pace alert: %+v", got)
	}
	response.Settings.AlertPaceEnabled = false
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("off closes: %+v", got)
	}
	// On again during the same ongoing episode: it can raise again, after the hysteresis.
	response.Settings.AlertPaceEnabled = true
	observe(false)
	now = now.Add(11 * time.Minute)
	observe(true)
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "pace" {
		t.Fatalf("re-enabling during an episode must raise again: %+v", got)
	}
	// But holding it on does not repeat the alert.
	observe(false)
	if got := tracker.OpenAlerts(); len(got) != 1 {
		t.Fatalf("one alert per episode: %+v", got)
	}
}

func TestExpiryAlertOnlyOnTheTokenCopyModel(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(10 * time.Minute)
	response := Response{Providers: []Provider{
		{ProviderName: "claude-code", Enabled: true, Available: true, Profiles: []Profile{
			{ProfileName: "migrated", Email: "one@example.test", Status: "ok", LastRefreshAt: &now, ExpiresAt: &expires, AlertCredential: "cred-1", ToolMigrated: true},
			{ProfileName: "linked", Email: "two@example.test", Status: "ok", LastRefreshAt: &now, ExpiresAt: &expires, AlertCredential: "cred-2", ToolMigrated: false},
		}}}}
	response.Settings.AlertExpiryEnabled = true
	if changed, err := tracker.Observe(response); err != nil || !changed {
		t.Fatalf("initial: %v %v", changed, err)
	}
	open := tracker.OpenAlerts()
	if len(open) != 1 || open[0].Profile != "migrated" {
		t.Fatalf("only the token-copy profile raises the expiry alert: %+v", open)
	}
}

// Every raise, the first episode of a cycle included, needs the window
// continuously at risk for the hysteresis, however short its time-to-full is; a safe reading restarts the
// clock, and one risky reading long after the recovery does not raise.
func TestPaceAlertRearmNeedsSustainedRisk(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(5 * time.Hour)
	shortEta := (5 * time.Minute).Seconds() // under the hysteresis: eta is time-to-full, not slack
	risk := &Pace{State: "deficit", EtaSeconds: &shortEta}
	safe := &Pace{State: "reserve", LastsToReset: true}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 70, ResetAt: &reset, Pace: risk}}}}}}}
	response.Settings.AlertPaceEnabled = true
	observe := func(after time.Duration, pace *Pace) {
		t.Helper()
		now = now.Add(after)
		response.Providers[0].Profiles[0].LastRefreshAt = &now
		response.Providers[0].Profiles[0].Windows[0].Pace = pace
		if _, err := tracker.Observe(response); err != nil {
			t.Fatal(err)
		}
	}
	open := func(want int, why string) {
		t.Helper()
		if got := tracker.OpenAlerts(); len(got) != want {
			t.Fatalf("%s: %d open, want %d: %+v", why, len(got), want, got)
		}
	}
	observe(time.Minute, risk)
	open(0, "the first risky reading must not raise")
	observe(9*time.Minute, risk)
	open(0, "9 minutes at risk is under the hysteresis")
	observe(2*time.Minute, risk)
	open(1, "11 sustained minutes raise the first episode")
	observe(time.Minute, safe)
	open(0, "recovery closes")
	observe(time.Hour, risk) // one risky reading long after the recovery
	open(0, "one jittery reading must not re-raise")
	observe(5*time.Minute, safe) // safe again: the clock restarts
	observe(6*time.Minute, risk)
	observe(6*time.Minute, risk)
	open(0, "risk, safe, risk restarts the clock: 6 minutes is too soon")
	observe(5*time.Minute, risk)
	open(1, "11 sustained minutes re-raise even with a short eta")
}

func TestExpiryRecordOldFormatCountsAsSeen(t *testing.T) {
	stamp, level, seen := expiryRecord("2026-10-21T12:00:00Z")
	if stamp != "2026-10-21T12:00:00Z" || level != "" || !seen {
		t.Fatalf("old-format record: %q %q %v", stamp, level, seen)
	}
	if _, _, seen := expiryRecord(""); seen {
		t.Fatal("an empty record is not seen")
	}
}

func TestResetNoticeIgnoresSlidingRollingWindow(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(5 * time.Hour)
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", AlertIdentity: "stable", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 85, ResetAt: &reset}}}}}}}
	response.Settings.AlertResetNoticeEnabled = true
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observe := func() {
		t.Helper()
		response.Providers[0].Profiles[0].LastRefreshAt = &now
		response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
		if _, err := tracker.Observe(response); err != nil {
			t.Fatal(err)
		}
	}
	observe()
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "warning" {
		t.Fatalf("warning: %+v", got)
	}
	id := tracker.OpenAlerts()[0].ID
	// Every refresh reports now+5h: the reset slides, the cycle stays.
	for i := 0; i < 15; i++ {
		now = now.Add(15 * time.Minute)
		reset = now.Add(5 * time.Hour)
		observe()
		got := tracker.OpenAlerts()
		if len(got) != 1 || got[0].ID != id || got[0].Tier != "warning" {
			t.Fatalf("step %d: drift must not notify or re-arm: %+v", i, got)
		}
	}
	// A real reset: the old reset time passes, the new one is later, usage is low.
	now = reset.Add(time.Minute)
	reset = now.Add(5 * time.Hour)
	response.Providers[0].Profiles[0].Windows[0].PercentUsed = 0
	observe()
	got := tracker.Notices()
	if len(got) != 1 || got[0].Tier != "reset" || len(tracker.OpenAlerts()) != 0 {
		t.Fatalf("exactly one reset notice: %+v", got)
	}
	now = now.Add(15 * time.Minute)
	reset = now.Add(5 * time.Hour)
	observe()
	if again := tracker.Notices(); len(again) != 1 || again[0].ID != got[0].ID {
		t.Fatalf("drift after the reset repeats the notice: %+v", again)
	}
}

// Only a reset time that slides LATER is a rolling window's drift. One that moves earlier by more than the tolerance, while the old reset is still
// ahead and usage has not dropped, is a new cycle: the old cycle's alerts close and the reset notice is sent.
func TestResetNoticeTreatsAnEarlierResetAsANewCycle(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(5 * time.Hour)
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", AlertIdentity: "stable", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 85, ResetAt: &reset}}}}}}}
	response.Settings.AlertResetNoticeEnabled = true
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observe := func() []UsageAlert {
		t.Helper()
		response.Providers[0].Profiles[0].LastRefreshAt = &now
		response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
		if _, err := tracker.Observe(response); err != nil {
			t.Fatal(err)
		}
		return append(tracker.OpenAlerts(), tracker.Notices()...)
	}
	first := observe()
	if len(first) != 1 || first[0].Tier != "warning" {
		t.Fatalf("warning: %+v", first)
	}
	// The reset moves an hour EARLIER while the old one is still four hours away and usage is unchanged: not drift.
	now = now.Add(15 * time.Minute)
	reset = reset.Add(-time.Hour)
	got := observe()
	tiers := map[string]int{}
	for _, alert := range got {
		if alert.ID == first[0].ID {
			t.Fatalf("the old cycle's warning must close: %+v", got)
		}
		tiers[alert.Tier]++
	}
	if tiers["reset"] != 1 {
		t.Fatalf("an earlier reset is a new cycle and sends one reset notice: %+v", got)
	}
}

// A reset notice is delivered as a toast, never as an Inbox item: only the
// threshold, pace and expiry tiers are OpenAlerts. The notice stays readable
// for an hour, then the next observation drops it, and a reset notice an
// older version left open in alerts.json is out of the Inbox at once and
// closes on the next observation once it is an hour old.
func TestResetNoticeIsNotAnInboxAlertAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	tracker, err := NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(5 * time.Hour)
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", AlertIdentity: "stable", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 50, ResetAt: &reset}}}}}}}
	response.Settings.AlertResetNoticeEnabled = true
	observe := func() {
		t.Helper()
		response.Providers[0].Profiles[0].LastRefreshAt = &now
		response.Providers[0].Profiles[0].Windows[0].ResetAt = &reset
		if _, err := tracker.Observe(response); err != nil {
			t.Fatal(err)
		}
	}
	observe()
	// A new cycle that is already past the warning threshold: the reset is
	// a notice, the warning stays an Inbox alert.
	now = reset.Add(time.Minute)
	reset = now.Add(5 * time.Hour)
	response.Providers[0].Profiles[0].Windows[0].PercentUsed = 85
	observe()
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "warning" {
		t.Fatalf("only the warning is an Inbox alert: %+v", got)
	}
	notices := tracker.Notices()
	if len(notices) != 1 || notices[0].Tier != "reset" {
		t.Fatalf("one reset notice: %+v", notices)
	}
	now = now.Add(59 * time.Minute)
	observe()
	if got := tracker.Notices(); len(got) != 1 {
		t.Fatalf("the notice stays readable within the hour: %+v", got)
	}
	now = now.Add(time.Minute)
	if got := tracker.Notices(); len(got) != 0 {
		t.Fatalf("an hour-old notice is no longer offered: %+v", got)
	}
	observe()
	if _, kept := tracker.state.Alerts[notices[0].ID]; kept {
		t.Fatal("the next observation drops an hour-old notice")
	}
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].Tier != "warning" {
		t.Fatalf("the warning outlives the notice: %+v", got)
	}

	// A reset notice an older version stored as an open Inbox alert.
	legacy := UsageAlert{ID: "usage_alert_legacyreset", Provider: "codex", Profile: "main", Window: "session", Tier: "reset",
		CreatedAt: now.Add(-3 * time.Hour), Open: true, BaseKey: notices[0].BaseKey}
	tracker.mu.Lock()
	tracker.state.Alerts[legacy.ID] = legacy
	if err := tracker.save(); err != nil {
		tracker.mu.Unlock()
		t.Fatal(err)
	}
	tracker.mu.Unlock()
	reloaded, err := NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range append(reloaded.OpenAlerts(), reloaded.Notices()...) {
		if alert.ID == legacy.ID {
			t.Fatalf("a stored reset notice is out of the Inbox at once: %+v", alert)
		}
	}
	tracker = reloaded
	now = now.Add(time.Minute)
	changed, err := tracker.Observe(response)
	if err != nil || !changed {
		t.Fatalf("closing the stored notice is a visible change: changed=%v err=%v", changed, err)
	}
	if _, kept := tracker.state.Alerts[legacy.ID]; kept {
		t.Fatal("the next usage refresh closes a stored reset notice")
	}
}

// A first risky reading that recovers before the hysteresis never alerts.
func TestPaceAlertFirstReadingRecoversBeforeHysteresis(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(5 * time.Hour)
	eta := (2 * time.Hour).Seconds()
	risk := &Pace{State: "deficit", EtaSeconds: &eta}
	safe := &Pace{State: "reserve", LastsToReset: true}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "main", Email: "one@example.test", Status: "ok", LastRefreshAt: &now,
			Windows: []Window{{Key: "session", PercentUsed: 2, ResetAt: &reset, Pace: risk}}}}}}}
	response.Settings.AlertPaceEnabled = true
	observe := func(after time.Duration, pace *Pace) {
		t.Helper()
		now = now.Add(after)
		response.Providers[0].Profiles[0].LastRefreshAt = &now
		response.Providers[0].Profiles[0].Windows[0].Pace = pace
		if _, err := tracker.Observe(response); err != nil {
			t.Fatal(err)
		}
	}
	observe(0, risk) // t0
	observe(5*time.Minute, safe)
	observe(6*time.Minute, risk) // 11 minutes since t0, but the run restarted
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("a recovered first reading must not alert: %+v", got)
	}
}

func rejectedProfile(name string, active bool, status string) Profile {
	// A switcher-managed profile: the overlay gives every configured profile
	// its opaque alert identity before the tracker sees it.
	return Profile{ProfileName: name, Active: active, Status: status, AlertIdentity: "ident\x00" + name,
		Error: "Codex usage request failed with HTTP 401."}
}

func observeLogin(t *testing.T, tracker *AlertTracker, response Response, want bool) {
	t.Helper()
	got, err := tracker.Observe(response)
	if err != nil || got != want {
		t.Fatalf("observe: created=%v error=%v, want %v", got, err, want)
	}
}

func drainedLogins(t *testing.T, tracker *AlertTracker) []RejectedLogin {
	t.Helper()
	got := tracker.DrainRejectedLogins()
	if len(got) == 0 {
		return nil
	}
	again := tracker.DrainRejectedLogins()
	if len(again) != 0 {
		t.Fatalf("a drain must empty the queue: %+v", again)
	}
	return got
}

func TestLoginRejectedAlertOncePerEpisode(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{rejectedProfile("work", true, "auth_missing")}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observeLogin(t, tracker, response, true)
	alerts := tracker.OpenAlerts()
	if len(alerts) != 1 || alerts[0].Tier != "login_rejected" || alerts[0].Profile != "work" || alerts[0].Hint != "Run codex login." {
		t.Fatalf("first rejection: %+v", alerts)
	}
	logins := drainedLogins(t, tracker)
	if len(logins) != 1 || logins[0].Tool != "codex" || logins[0].Profile != "work" || logins[0].Hint != "Run codex login." {
		t.Fatalf("one bell event per episode: %+v", logins)
	}
	first := alerts[0].ID
	// Further rejected polls raise nothing new.
	observeLogin(t, tracker, response, false)
	observeLogin(t, tracker, response, false)
	if logins := drainedLogins(t, tracker); len(logins) != 0 {
		t.Fatalf("once per episode, not per poll: %+v", logins)
	}
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].ID != first {
		t.Fatalf("the episode's item stays open: %+v", got)
	}
	// Recovery closes the item; a fresh rejection is a new episode with a new item and event.
	response.Providers[0].Profiles[0] = Profile{ProfileName: "work", Active: true, Status: "ok", AlertIdentity: "ident\x00work"}
	observeLogin(t, tracker, response, true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("recovery resolves the item: %+v", got)
	}
	if logins := drainedLogins(t, tracker); len(logins) != 0 {
		t.Fatalf("recovery raises nothing: %+v", logins)
	}
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0] = rejectedProfile("work", true, "scope_missing")
	observeLogin(t, tracker, response, true)
	alerts = tracker.OpenAlerts()
	if len(alerts) != 1 || alerts[0].ID == first || alerts[0].Tier != "login_rejected" {
		t.Fatalf("a new episode raises a new item: %+v", alerts)
	}
	if logins := drainedLogins(t, tracker); len(logins) != 1 {
		t.Fatalf("a new episode rings the bell again: %+v", logins)
	}
}

func TestLoginRejectedHintPerTool(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claude := rejectedProfile("main", true, "auth_missing")
	claude.Error = "Claude usage request failed with HTTP 401."
	mistral := rejectedProfile("local", true, "auth_missing")
	mistral.Error = "Mistral session has expired. Sign in again and paste a fresh Cookie request header."
	response := Response{Providers: []Provider{
		{ProviderName: "claude-code", Enabled: true, Available: true, Profiles: []Profile{claude}},
		{ProviderName: "mistral", Enabled: true, Available: true, Profiles: []Profile{mistral}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observeLogin(t, tracker, response, true)
	alerts := tracker.OpenAlerts()
	byProvider := map[string]string{}
	for _, alert := range alerts {
		byProvider[alert.Provider] = alert.Hint
	}
	if byProvider["claude-code"] != "Run claude /login." || byProvider["mistral"] != mistral.Error {
		t.Fatalf("hints: %+v", byProvider)
	}
	logins := drainedLogins(t, tracker)
	tools := map[string]string{}
	for _, login := range logins {
		tools[login.Tool] = login.Hint
	}
	if tools["claude"] != "Run claude /login." || tools["mistral"] != mistral.Error {
		t.Fatalf("event tools and hints: %+v", logins)
	}
}

func TestLoginRejectedInactiveProfileGetsTheBellOnly(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{rejectedProfile("spare", false, "auth_missing")}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	// A bell without an Inbox item is no visible change.
	observeLogin(t, tracker, response, false)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("an inactive profile gets no Inbox item: %+v", got)
	}
	if logins := drainedLogins(t, tracker); len(logins) != 1 || logins[0].Profile != "spare" {
		t.Fatalf("an inactive profile still rings the bell: %+v", logins)
	}
	// The same rejection, now on the active profile, raises the item once.
	response.Providers[0].Profiles[0] = rejectedProfile("spare", true, "auth_missing")
	observeLogin(t, tracker, response, true)
	if got := tracker.OpenAlerts(); len(got) != 1 {
		t.Fatalf("activation during the episode raises the item: %+v", got)
	}
	if logins := drainedLogins(t, tracker); len(logins) != 0 {
		t.Fatalf("activation does not ring the bell again: %+v", logins)
	}
	observeLogin(t, tracker, response, false)
	if got := tracker.OpenAlerts(); len(got) != 1 {
		t.Fatalf("once per episode: %+v", got)
	}
}

func TestLoginRejectedEpisodeSurvivesRestartAndDismissal(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{rejectedProfile("work", true, "auth_missing")}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observeLogin(t, tracker, response, true)
	drainedLogins(t, tracker)
	first := tracker.OpenAlerts()[0].ID
	// A restart in the middle of the episode raises nothing new.
	tracker, err = NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observeLogin(t, tracker, response, false)
	if logins := drainedLogins(t, tracker); len(logins) != 0 {
		t.Fatalf("no second bell after a restart: %+v", logins)
	}
	if got := tracker.OpenAlerts(); len(got) != 1 || got[0].ID != first {
		t.Fatalf("the item survives the restart: %+v", got)
	}
	// A dismissal stays closed for the rest of the episode.
	if err := tracker.Dismiss(first); err != nil {
		t.Fatal(err)
	}
	observeLogin(t, tracker, response, false)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("a dismissed episode stays dismissed: %+v", got)
	}
	// Recovery, then a new rejection: a new item.
	response.Providers[0].Profiles[0] = Profile{ProfileName: "work", Active: true, Status: "ok", AlertIdentity: "ident\x00work"}
	observeLogin(t, tracker, response, true)
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0] = rejectedProfile("work", true, "auth_missing")
	observeLogin(t, tracker, response, true)
	got := tracker.OpenAlerts()
	if len(got) != 1 || got[0].ID == first {
		t.Fatalf("a new episode after recovery: %+v", got)
	}
	if logins := drainedLogins(t, tracker); len(logins) != 1 {
		t.Fatalf("a new episode after recovery rings once: %+v", logins)
	}
}

func TestLoginRejectedEndsWithTheAccount(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{rejectedProfile("work", true, "auth_missing")}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observeLogin(t, tracker, response, true)
	drainedLogins(t, tracker)
	// The profile disappears (removed, or its provider disabled): the item closes.
	response.Providers[0].Profiles = nil
	observeLogin(t, tracker, response, true)
	if got := tracker.OpenAlerts(); len(got) != 0 {
		t.Fatalf("a disappeared account closes its alert: %+v", got)
	}
	// A reappeared account starts a fresh episode, with a fresh item and bell.
	now = now.Add(time.Minute)
	response.Providers[0].Profiles = []Profile{rejectedProfile("work", true, "auth_missing")}
	observeLogin(t, tracker, response, true)
	if got := tracker.OpenAlerts(); len(got) != 1 {
		t.Fatalf("a reappeared account raises again: %+v", got)
	}
	if logins := drainedLogins(t, tracker); len(logins) != 1 {
		t.Fatalf("a reappeared account rings again: %+v", logins)
	}
}

func TestLoginRejectedRenameKeepsTheEpisode(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	profile := rejectedProfile("old", false, "auth_missing")
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{profile}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observeLogin(t, tracker, response, false) // the bell only: no visible change
	drainedLogins(t, tracker)
	profile.ProfileName = "renamed"
	response.Providers[0].Profiles[0] = profile
	observeLogin(t, tracker, response, false)
	if logins := drainedLogins(t, tracker); len(logins) != 0 {
		t.Fatalf("a rename is not a new episode: %+v", logins)
	}
	// The profile becomes active under its new name: the item carries it.
	profile.Active = true
	response.Providers[0].Profiles[0] = profile
	observeLogin(t, tracker, response, true)
	got := tracker.OpenAlerts()
	if len(got) != 1 || got[0].Profile != "renamed" {
		t.Fatalf("the item follows the name: %+v", got)
	}
}

func TestLoginRejectedFirstSeenAndTailAreRecordedOnce(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{rejectedProfileWithTail("work", true, "auth_missing", "a1b2c3")}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observeLogin(t, tracker, response, true)
	alerts := tracker.OpenAlerts()
	if len(alerts) != 1 || alerts[0].FirstSeen == nil || !alerts[0].FirstSeen.Equal(now) || alerts[0].TokenTail != "a1b2c3" {
		t.Fatalf("the item carries the first-seen time and the hash tail: %+v", alerts[0])
	}
	logins := drainedLogins(t, tracker)
	if len(logins) != 1 || logins[0].FirstSeen != now || logins[0].TokenTail != "a1b2c3" {
		t.Fatalf("the bell event carries the first-seen time and the hash tail: %+v", logins)
	}
	// Later polls of the same episode change neither.
	now = now.Add(5 * time.Minute)
	response.Providers[0].Profiles[0].TokenTail = "f9e8d7"
	observeLogin(t, tracker, response, false)
	alerts = tracker.OpenAlerts()
	if len(alerts) != 1 || !alerts[0].FirstSeen.Equal(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)) || alerts[0].TokenTail != "a1b2c3" {
		t.Fatalf("a later poll must not move the first-seen time or the tail: %+v", alerts[0])
	}
	if logins := drainedLogins(t, tracker); len(logins) != 0 {
		t.Fatalf("once per episode: %+v", logins)
	}
	// Recovery, then a fresh rejection with a fresh credential: a new episode
	// records a new first-seen time and tail.
	response.Providers[0].Profiles[0] = Profile{ProfileName: "work", Active: true, Status: "ok", AlertIdentity: "ident\x00work"}
	observeLogin(t, tracker, response, true)
	now = now.Add(time.Minute)
	response.Providers[0].Profiles[0] = rejectedProfileWithTail("work", true, "scope_missing", "0f1e2d")
	observeLogin(t, tracker, response, true)
	alerts = tracker.OpenAlerts()
	if len(alerts) != 1 || !alerts[0].FirstSeen.Equal(now) || alerts[0].TokenTail != "0f1e2d" {
		t.Fatalf("a new episode records its own first-seen time and tail: %+v", alerts[0])
	}
	if logins := drainedLogins(t, tracker); len(logins) != 1 || logins[0].FirstSeen != now || logins[0].TokenTail != "0f1e2d" {
		t.Fatalf("the new episode's bell event carries them too: %+v", logins)
	}
}

func TestLoginRejectedOldStateWithoutTheFieldsLoads(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{rejectedProfile("work", true, "auth_missing")}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observeLogin(t, tracker, response, true)
	drainedLogins(t, tracker)
	// Rewrite the saved state the way an older version wrote it: no
	// first_seen and no token_tail on the episode.
	data, err := os.ReadFile(tracker.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	var auth map[string]map[string]any
	if err := json.Unmarshal(saved["auth"], &auth); err != nil {
		t.Fatal(err)
	}
	if len(auth) != 1 {
		t.Fatalf("one episode expected: %+v", auth)
	}
	for _, episode := range auth {
		delete(episode, "first_seen")
		delete(episode, "token_tail")
	}
	saved["auth"], err = json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracker.path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// The old file loads and the ongoing episode continues without a second
	// bell; the first-seen time is backfilled from the next observation.
	tracker, err = NewAlertTracker(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	observeLogin(t, tracker, response, false)
	if logins := drainedLogins(t, tracker); len(logins) != 0 {
		t.Fatalf("an old state must not raise a second bell: %+v", logins)
	}
	times := tracker.RejectionTimes()
	if got := times["codex\x00work"]; got.IsZero() || !got.Equal(now) {
		t.Fatalf("the episode's first-seen time is backfilled: %+v", times)
	}
}

func TestLoginRejectedRejectionTimesFollowTheEpisode(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{rejectedProfile("work", true, "auth_missing")}}}}
	response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
	observeLogin(t, tracker, response, true)
	if got := tracker.RejectionTimes()["codex\x00work"]; !got.Equal(now) {
		t.Fatalf("the episode's first-seen time is listed: %v", got)
	}
	// Recovery keeps it listed, so the gap the episode left stays explainable.
	response.Providers[0].Profiles[0] = Profile{ProfileName: "work", Active: true, Status: "ok", AlertIdentity: "ident\x00work"}
	observeLogin(t, tracker, response, true)
	if got := tracker.RejectionTimes()["codex\x00work"]; !got.Equal(now) {
		t.Fatalf("a recovered episode stays listed: %v", got)
	}
	// The profile disappears: the record goes with it (the episode was
	// already closed by the recovery, so nothing visible changes).
	response.Providers[0].Profiles = nil
	observeLogin(t, tracker, response, false)
	if times := tracker.RejectionTimes(); len(times) != 0 {
		t.Fatalf("a disappeared account is no longer listed: %+v", times)
	}
}

func rejectedProfileWithTail(name string, active bool, status string, tail string) Profile {
	profile := rejectedProfile(name, active, status)
	profile.TokenTail = tail
	return profile
}
