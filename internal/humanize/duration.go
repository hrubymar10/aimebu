// Package humanize formats values for people to read.
package humanize

import (
	"strconv"
	"time"
)

const (
	secondsPerMinute = 60
	secondsPerHour   = 60 * secondsPerMinute
	secondsPerDay    = 24 * secondsPerHour
)

// Duration formats d as a short, compact text in days, hours, minutes and seconds ("d", "h", "m", "s"), showing the largest unit that is not
// zero and, when it is not zero, the unit right below it: 1h 1m, 2d 3h, 5m 7s. The units below those are dropped, never rounded into them
// (2d 0h 5m is "2d"), and a second unit that is zero is left out (1h 0m is "1h").
//
// d is first rounded to the nearest second, half away from zero (1.5s is "2s", 499ms is "0s"). Zero after rounding is "0s". A negative
// duration is the same text behind a "-" (-90s is "-1m 30s"); every duration is handled, math.MinInt64 included.
func Duration(d time.Duration) string {
	// Whole seconds and the rest, both with the sign of d, so nothing is negated before it is known to fit: -d overflows for the smallest duration.
	seconds, rest := int64(d/time.Second), int64(d%time.Second)
	if rest >= int64(time.Second/2) {
		seconds++
	} else if rest <= -int64(time.Second/2) {
		seconds--
	}
	if seconds == 0 {
		return "0s"
	}
	negative := seconds < 0
	// The magnitude of the rounded seconds fits an int64 (the largest is about 9.2e9), so the negation is safe.
	remaining := uint64(seconds)
	if negative {
		remaining = uint64(-seconds)
	}
	units := [4]uint64{
		remaining / secondsPerDay,
		remaining % secondsPerDay / secondsPerHour,
		remaining % secondsPerHour / secondsPerMinute,
		remaining % secondsPerMinute,
	}
	suffixes := [4]string{"d", "h", "m", "s"}
	text := ""
	if negative {
		text = "-"
	}
	for i, value := range units {
		if value == 0 {
			continue
		}
		text += strconv.FormatUint(value, 10) + suffixes[i]
		if i+1 < len(units) && units[i+1] != 0 {
			text += " " + strconv.FormatUint(units[i+1], 10) + suffixes[i+1]
		}
		break
	}
	return text
}
