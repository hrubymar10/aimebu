package humanize

import (
	"math"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// TestMain isolates the tests from the person's home and credentials, as every package with tests does. It is here, not in a file of its own,
// because this package is exactly its two files.
func TestMain(m *testing.M) { testenv.Main(m) }

func TestDuration(t *testing.T) {
	t.Parallel()
	const second = time.Second
	for _, tc := range []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"499ms rounds down to zero", 499 * time.Millisecond, "0s"},
		{"500ms rounds up", 500 * time.Millisecond, "1s"},
		{"999ms", 999 * time.Millisecond, "1s"},
		{"1.5s rounds away from zero", 1500 * time.Millisecond, "2s"},
		{"2.5s rounds away from zero too", 2500 * time.Millisecond, "3s"},
		{"-499ms is zero without a sign", -499 * time.Millisecond, "0s"},
		{"-500ms rounds away from zero", -500 * time.Millisecond, "-1s"},
		{"-1.5s", -1500 * time.Millisecond, "-2s"},
		{"59s", 59 * second, "59s"},
		{"59.5s rounds into the minute", 59500 * time.Millisecond, "1m"},
		{"60s", 60 * second, "1m"},
		{"61s", 61 * second, "1m 1s"},
		{"90s", 90 * second, "1m 30s"},
		{"-90s", -90 * second, "-1m 30s"},
		{"5m7s", 5*time.Minute + 7*second, "5m 7s"},
		{"3599s", 3599 * second, "59m 59s"},
		{"3600s", 3600 * second, "1h"},
		{"3660s has no seconds to show", 3660 * second, "1h 1m"},
		{"3661s drops the third unit", 3661 * second, "1h 1m"},
		{"1h0m5s drops what is below a zero second unit", time.Hour + 5*second, "1h"},
		{"86399s", 86399 * second, "23h 59m"},
		{"86400s", 86400 * second, "1d"},
		{"90061s", 90061 * second, "1d 1h"},
		{"2d3h", 51 * time.Hour, "2d 3h"},
		{"172800s+5m is only the days", 172800*second + 5*time.Minute, "2d"},
		{"-172800s-5m", -(172800*second + 5*time.Minute), "-2d"},
		{"1d0h5m", 24*time.Hour + 5*time.Minute, "1d"},
		{"1d0h0m7s", 24*time.Hour + 7*second, "1d"},
		{"100 days 3 hours", 100*24*time.Hour + 3*time.Hour + 4*time.Minute, "100d 3h"},
		{"max duration", math.MaxInt64, "106751d 23h"},
		{"min duration", math.MinInt64, "-106751d 23h"},
	} {
		if got := Duration(tc.in); got != tc.want {
			t.Errorf("%s: Duration(%d) = %q, want %q", tc.name, int64(tc.in), got, tc.want)
		}
	}
}

// The smallest duration cannot be negated in an int64; it must format, and as the mirror of the largest.
func TestDurationExtremesDoNotPanic(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{math.MinInt64, math.MinInt64 + 1, math.MaxInt64, math.MaxInt64 - 1} {
		if got := Duration(d); got == "" {
			t.Errorf("Duration(%d) is empty", int64(d))
		}
	}
	if low, high := Duration(math.MinInt64+1), Duration(math.MaxInt64); low != "-"+high {
		t.Errorf("Duration(MinInt64+1) = %q, want the mirror of %q", low, high)
	}
}

// Whatever the input, the text uses only digits, spaces, d/h/m/s and a leading minus, at most two units, and never a third.
func TestDurationShape(t *testing.T) {
	t.Parallel()
	for seconds := int64(-200000); seconds <= 200000; seconds += 37 {
		for _, extra := range []time.Duration{0, 499 * time.Millisecond, 500 * time.Millisecond} {
			text := Duration(time.Duration(seconds)*time.Second + extra)
			units := 0
			for _, r := range text {
				switch {
				case r >= '0' && r <= '9', r == ' ', r == '-':
				case r == 'd' || r == 'h' || r == 'm' || r == 's':
					units++
				default:
					t.Fatalf("Duration(%ds+%v) = %q has the character %q", seconds, extra, text, r)
				}
			}
			if units < 1 || units > 2 || text == "-0s" {
				t.Fatalf("Duration(%ds+%v) = %q has %d units", seconds, extra, text, units)
			}
		}
	}
}
