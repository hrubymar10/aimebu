package usages

import "time"

const (
	PaceStateReserve     = "reserve"
	PaceStateDeficit     = "deficit"
	PaceStateOnTrack     = "on_track"
	paceOnTrackThreshold = 2.0
	paceMinElapsedPct    = 3.0
)

// computeWindowPace returns the linear-spend pace for w at now,
// or nil when no pace can be computed (no duration, no reset, window not started).
func computeWindowPace(w Window, now time.Time) *Pace {
	if w.WindowDurationSeconds <= 0 || w.ResetAt == nil {
		return nil
	}
	duration := float64(w.WindowDurationSeconds)
	secondsToReset := w.ResetAt.Sub(now).Seconds()
	if secondsToReset <= 0 || secondsToReset > duration {
		return nil
	}
	elapsed := duration - secondsToReset
	expectedPct := elapsed / duration * 100
	if expectedPct < paceMinElapsedPct && w.PercentUsed == 0 {
		return nil
	}
	delta := w.PercentUsed - expectedPct
	state := PaceStateOnTrack
	if delta > paceOnTrackThreshold {
		state = PaceStateDeficit
	} else if delta < -paceOnTrackThreshold {
		state = PaceStateReserve
	}
	p := &Pace{
		ExpectedPercent: expectedPct,
		DeltaPercent:    delta,
		State:           state,
	}
	if elapsed > 0 && w.PercentUsed > 0 {
		burnRate := w.PercentUsed / elapsed
		remaining := 100 - w.PercentUsed
		if remaining <= 0 {
			zero := 0.0
			p.EtaSeconds = &zero
		} else {
			secondsTillFull := remaining / burnRate
			if secondsTillFull >= secondsToReset {
				p.LastsToReset = true
			} else {
				p.EtaSeconds = &secondsTillFull
			}
		}
	} else {
		p.LastsToReset = true
	}
	return p
}
