package pricing

import (
	"math"
	"net/url"
	"testing"
	"time"
)

func TestPriceListAndEstimate(t *testing.T) {
	for model, rate := range list {
		if model == "" || rate.Input <= 0 || rate.Output <= 0 || rate.CacheRead <= 0 || rate.CacheWrite <= 0 {
			t.Fatalf("invalid rate for %q: %+v", model, rate)
		}
		if _, err := time.Parse("2006-01-02", rate.AsOf); err != nil {
			t.Fatalf("invalid price date for %q: %v", model, err)
		}
		if source, err := url.Parse(rate.Source); err != nil || source.Scheme != "https" || source.Host == "" {
			t.Fatalf("invalid price source for %q: %q", model, rate.Source)
		}
	}
	input, output, read, write := int64(20), int64(7), int64(80), int64(5)
	got := Estimate("gpt-6-sol", &input, &output, &read, &write)
	want := (20*2.0 + 7*10.0 + 80*0.2 + 5*2.5) / 1_000_000
	if got == nil || math.Abs(*got-want) > 1e-12 {
		t.Fatalf("estimate = %v, want %g", got, want)
	}
	if Estimate("unlisted-model", &input, &output, &read, &write) != nil || Estimate("gpt-6-sol", nil, &output, nil, nil) != nil {
		t.Fatal("an unlisted model or incomplete usage was priced")
	}
}
