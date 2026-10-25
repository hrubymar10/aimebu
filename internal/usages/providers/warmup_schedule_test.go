package providers

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInvalidWarmupConfigCanBeRepairedOnSave(t *testing.T) {
	for _, field := range []string{`"warmup_mode":"broken"`, `"warmup_mode":42`, `"warmup_schedule":"bad schedule"`, `"warmup_schedule":false`} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			body := `{"refresh_interval_sec":120,"providers":{},"warmup_mode":"scheduled","warmup_schedule":"30 9 * * 1",` + field + `}`
			if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewStore(root)
			cfg, err := store.LoadConfig()
			if err != nil || cfg.WarmupMode != "off" || cfg.WarmupSchedule != "" {
				t.Fatalf("repaired config=%+v err=%v", cfg, err)
			}
			if _, err := store.UpdateConfig(func(cfg *Config) error {
				cfg.PercentDisplay = "used"
				cfg.WarmupMode = "force"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err = store.LoadConfig()
			if err != nil || cfg.WarmupMode != "force" || cfg.WarmupSchedule != "" || cfg.PercentDisplay != "used" {
				t.Fatalf("saved config=%+v err=%v", cfg, err)
			}
		})
	}
}

func TestWarmupScheduleSimpleCronAndValidation(t *testing.T) {
	lines, err := ParseWarmupSchedule("\n30 9 * * 1\n0 12 15 * 7\n")
	if err != nil || len(lines) != 2 {
		t.Fatalf("parse: %v, lines=%d", err, len(lines))
	}
	monday := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)
	if !WarmupScheduleMatches(lines, monday) || WarmupScheduleMatches(lines, monday.Add(time.Minute)) {
		t.Fatal("Monday minute did not match exactly")
	}
	sunday := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if !WarmupScheduleMatches(lines, sunday) {
		t.Fatal("7 must mean Sunday")
	}
	for _, invalid := range []string{"*/5 * * * *", "0,30 * * * *", "61 * * * *", "0 0 * *", "0 0 * * 8"} {
		if _, err := ParseWarmupSchedule(invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	if err := ValidateWarmup("scheduled", ""); err == nil {
		t.Fatal("empty scheduled mode accepted")
	}
}
