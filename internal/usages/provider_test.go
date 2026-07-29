package usages

import (
	"math"
	"testing"
)

func TestNormalizedWindowClampsPercentUsed(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  float64
	}{
		{name: "negative", value: -1, want: 0},
		{name: "not a number", value: math.NaN(), want: 0},
		{name: "zero", value: 0, want: 0},
		{name: "fractional", value: 42.5, want: 42.5},
		{name: "full", value: 100, want: 100},
		{name: "over", value: 101, want: 100},
		{name: "positive infinity", value: math.Inf(1), want: 100},
		{name: "negative infinity", value: math.Inf(-1), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizedWindow("test", tt.value, nil, 0).PercentUsed; got != tt.want {
				t.Fatalf("PercentUsed = %v, want %v", got, tt.want)
			}
		})
	}
}
