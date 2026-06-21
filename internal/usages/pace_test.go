package usages

import (
	"testing"
	"time"
)

func TestComputeWindowPace(t *testing.T) {
	const duration = 5 * 3600 // 5h session window, in seconds

	// makeWindow builds a Window with reset relative to a fixed now so the
	// same now can be passed to computeWindowPace without floating-point drift.
	makeWindow := func(now time.Time, durationSec int64, percentUsed float64, secondsToReset float64) Window {
		reset := now.Add(time.Duration(secondsToReset * float64(time.Second)))
		return Window{
			Key:                   "session",
			PercentUsed:           percentUsed,
			ResetAt:               &reset,
			WindowDurationSeconds: durationSec,
		}
	}

	now := time.Now()

	t.Run("no duration returns nil", func(t *testing.T) {
		reset := now.Add(time.Hour)
		w := Window{Key: "session", PercentUsed: 50, ResetAt: &reset}
		if got := computeWindowPace(w, now); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("no reset_at returns nil", func(t *testing.T) {
		w := Window{Key: "session", PercentUsed: 50, WindowDurationSeconds: duration}
		if got := computeWindowPace(w, now); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("seconds_to_reset > duration returns nil", func(t *testing.T) {
		reset := now.Add(6 * time.Hour)
		w := Window{Key: "session", PercentUsed: 10, ResetAt: &reset, WindowDurationSeconds: duration}
		if got := computeWindowPace(w, now); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("freshly reset with zero usage returns nil", func(t *testing.T) {
		// Very small elapsed (30s of 18000s), zero used — suppress
		w := makeWindow(now, duration, 0, float64(duration)-30)
		if got := computeWindowPace(w, now); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("on track", func(t *testing.T) {
		// Exactly halfway: 50% elapsed, 50% used → on track, lasts to reset
		w := makeWindow(now, duration, 50, float64(duration)/2)
		p := computeWindowPace(w, now)
		if p == nil {
			t.Fatal("expected non-nil pace")
		}
		if p.State != PaceStateOnTrack {
			t.Errorf("state = %q, want on_track", p.State)
		}
		if p.DeltaPercent < -paceOnTrackThreshold || p.DeltaPercent > paceOnTrackThreshold {
			t.Errorf("delta = %.2f out of on_track band", p.DeltaPercent)
		}
		if !p.LastsToReset {
			t.Error("expected lasts_to_reset=true for perfectly on-track window")
		}
	})

	t.Run("reserve: under pace, lasts to reset", func(t *testing.T) {
		// 75% elapsed (3.75h), only 30% used → well under pace → reserve + lasts to reset
		// burn rate = 30%/13500s; secondsTillFull = 70*13500/30 = 31500 >> 4500 (to reset)
		w := makeWindow(now, duration, 30, float64(duration)*0.25)
		p := computeWindowPace(w, now)
		if p == nil {
			t.Fatal("expected non-nil pace")
		}
		if p.State != PaceStateReserve {
			t.Errorf("state = %q, want reserve", p.State)
		}
		if p.DeltaPercent >= 0 {
			t.Errorf("delta = %.2f, expected negative for reserve", p.DeltaPercent)
		}
		if !p.LastsToReset {
			t.Error("expected lasts_to_reset=true for under-pace window")
		}
	})

	t.Run("deficit: over pace, runs out before reset", func(t *testing.T) {
		// 20% elapsed (3600s), 80% used → heavy deficit
		// burn rate = 80%/3600s; secondsTillFull = 20*3600/80 = 900s; to reset = 14400s → 900 < 14400
		w := makeWindow(now, duration, 80, float64(duration)*0.8)
		p := computeWindowPace(w, now)
		if p == nil {
			t.Fatal("expected non-nil pace")
		}
		if p.State != PaceStateDeficit {
			t.Errorf("state = %q, want deficit", p.State)
		}
		if p.DeltaPercent <= 0 {
			t.Errorf("delta = %.2f, expected positive for deficit", p.DeltaPercent)
		}
		if p.EtaSeconds == nil {
			t.Error("expected eta_seconds when running out before reset")
		}
		if p.LastsToReset {
			t.Error("expected lasts_to_reset=false when running out early")
		}
	})

	t.Run("deficit but lasts to reset: mild over-pace, projects finishing after reset", func(t *testing.T) {
		// 50% elapsed, 48% used → tiny deficit: burn rate = 48%/9000s
		// secondsTillFull = 52*9000/48 = 9750 > 9000 (to reset) → lasts to reset
		w := makeWindow(now, duration, 48, float64(duration)*0.5)
		p := computeWindowPace(w, now)
		if p == nil {
			t.Fatal("expected non-nil pace")
		}
		// 48 vs expected 50 → delta = -2 → on_track or reserve; either way lasts_to_reset
		if !p.LastsToReset {
			t.Errorf("expected lasts_to_reset=true for mild under-pace, got eta_seconds=%v", p.EtaSeconds)
		}
	})

	t.Run("weekly window uses 7d duration", func(t *testing.T) {
		const weekDuration = 7 * 24 * 3600
		// 3.5d elapsed, 50% used → on track
		w := makeWindow(now, weekDuration, 50, float64(weekDuration)/2)
		w.Key = "weekly"
		p := computeWindowPace(w, now)
		if p == nil {
			t.Fatal("expected non-nil pace")
		}
		if p.State != PaceStateOnTrack {
			t.Errorf("state = %q, want on_track", p.State)
		}
	})
}
