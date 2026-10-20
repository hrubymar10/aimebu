package store

import (
	"context"
	"errors"
	"testing"
)

func TestMessageExpiryDefaultsToSixHoursAndRoundTrips(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if got, err := s.MessageExpirySeconds(ctx); err != nil || got != 21600 {
		t.Fatalf("a fresh store must have expiry on at six hours: %d %v", got, err)
	}
	for _, n := range []int{0, 60, 3600, 2592000} {
		if err := s.SetMessageExpirySeconds(ctx, n); err != nil {
			t.Fatalf("set %d: %v", n, err)
		}
		if got, _ := s.MessageExpirySeconds(ctx); got != n {
			t.Fatalf("round trip %d = %d", n, got)
		}
	}
}

func TestMessageExpiryBoundsAreExact(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	for _, bad := range []int{-1, 1, 59, 2592001, 100000000} {
		if err := s.SetMessageExpirySeconds(ctx, bad); !errors.Is(err, ErrInvalidMessageExpiry) {
			t.Errorf("%d accepted: %v", bad, err)
		}
	}
	if got, _ := s.MessageExpirySeconds(ctx); got != DefaultMessageExpirySeconds {
		t.Fatalf("a rejected value changed the setting: %d", got)
	}
}

// A hand-edited or corrupt stored value must not switch expiry off or hide history:
// it reads as the default.
func TestAnUnreadableStoredExpiryReadsAsTheDefault(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	for _, value := range []string{"abc", "-5", "30", "9999999999", ""} {
		if _, err := s.db.Exec(`INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, MessageExpiryKey, value, timestamp()); err != nil {
			t.Fatal(err)
		}
		if got, err := s.MessageExpirySeconds(ctx); err != nil || got != DefaultMessageExpirySeconds {
			t.Errorf("stored %q read as %d %v, want the default", value, got, err)
		}
	}
}
