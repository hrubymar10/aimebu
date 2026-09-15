package usages

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseWarmupSchedule(t *testing.T) {
	valid := []string{
		"",
		"0 8 * * *",
		"* * * * *",
		"30 9 1 1 7",
	}
	for _, raw := range valid {
		t.Run("valid_"+raw, func(t *testing.T) {
			if _, err := parseWarmupSchedule(raw); err != nil {
				t.Fatalf("parseWarmupSchedule(%q): %v", raw, err)
			}
		})
	}

	invalid := []string{
		"0 8 * *",
		"60 8 * * *",
		"0 24 * * *",
		"0 8 0 * *",
		"0 8 * 13 *",
		"0 8 * * 8",
		"*/5 * * * *",
		"0 8 * * 1,2",
	}
	for _, raw := range invalid {
		t.Run("invalid_"+raw, func(t *testing.T) {
			if _, err := parseWarmupSchedule(raw); err == nil {
				t.Fatalf("parseWarmupSchedule(%q) succeeded, want error", raw)
			}
		})
	}
}

func TestWarmupScheduleMatchesCronDays(t *testing.T) {
	schedule, err := parseWarmupSchedule("30 9 15 6 1")
	if err != nil {
		t.Fatal(err)
	}
	// When both day fields are restricted, standard cron matches either one.
	if !schedule.matches(time.Date(2026, time.June, 15, 9, 30, 0, 0, time.Local)) {
		t.Fatal("schedule should match the configured day of month")
	}
	if !schedule.matches(time.Date(2026, time.June, 22, 9, 30, 0, 0, time.Local)) {
		t.Fatal("schedule should match the configured Monday")
	}
	if schedule.matches(time.Date(2026, time.June, 16, 9, 30, 0, 0, time.Local)) {
		t.Fatal("schedule matched neither configured day")
	}
}

func TestWarmupScheduleDueOncePerMatchingMinute(t *testing.T) {
	m := NewManager(nil, nil)
	now := time.Date(2026, time.September, 15, 8, 0, 5, 0, time.Local)
	if !m.warmupScheduleDue("0 8 * * *", now) {
		t.Fatal("first matching tick should be due")
	}
	if m.warmupScheduleDue("0 8 * * *", now.Add(40*time.Second)) {
		t.Fatal("second tick in the same minute should not be due")
	}
	if !m.warmupScheduleDue("0 8 * * *", now.Add(24*time.Hour)) {
		t.Fatal("next day's matching minute should be due")
	}
	if m.warmupScheduleDue("", now) {
		t.Fatal("empty scheduled-mode schedule should not fire")
	}
}

func TestWarmupScheduleMultipleLinesAnyMatch(t *testing.T) {
	now := time.Date(2026, time.September, 15, 8, 30, 0, 0, time.Local)
	schedules, err := parseWarmupSchedules("0 7 * * *\n\n30 8 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !warmupSchedulesMatch(schedules, now) {
		t.Fatal("second schedule line should match")
	}
	if _, err := parseWarmupSchedules("\n61 8 * * *"); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("invalid second line error = %v", err)
	}
}

func TestManagerTriggerClaudeAutoWarmupScheduled(t *testing.T) {
	m, store, exec, now := warmupTriggerFixture(t)
	setWarmupSettings(t, store, true, WarmupModeScheduled)
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	scheduled := now.Add(time.Minute)
	cfg.WarmupSchedule = fmt.Sprintf("0 0 * * *\n%d %d * * *", scheduled.Minute(), scheduled.Hour())
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 0)

	m.clock.(*fakeClock).now = scheduled
	m.triggerClaudeAutoWarmup(context.Background())
	waitForCalls(t, exec, 2)
}
