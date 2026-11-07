package store

import (
	"context"
	"regexp"
	"testing"
)

func TestTelemetryIdentityIsRandomLazyAndRollable(t *testing.T) {
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	for _, user := range []string{"u1", "u2"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, user, "login-"+user); err != nil {
			t.Fatal(err)
		}
	}
	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	var empty string
	if err := s.db.QueryRow(`SELECT telemetry_fingerprint FROM user WHERE id='u1'`).Scan(&empty); err != nil || empty != "" {
		t.Fatalf("an account has no fingerprint until one is needed: %q %v", empty, err)
	}
	one, err := s.UserTelemetry(ctx, "u1")
	if err != nil || !hex16.MatchString(one.Fingerprint) || one.SendName || one.Login != "login-u1" || one.ReportID() != one.Fingerprint {
		t.Fatalf("first read: %+v %v", one, err)
	}
	again, _ := s.UserTelemetry(ctx, "u1")
	two, _ := s.UserTelemetry(ctx, "u2")
	if again.Fingerprint != one.Fingerprint || two.Fingerprint == one.Fingerprint {
		t.Fatalf("stable for one person, different between people: %+v %+v %+v", one, again, two)
	}
	if err := s.SetTelemetrySendName(ctx, "u1", true); err != nil {
		t.Fatal(err)
	}
	named, _ := s.UserTelemetry(ctx, "u1")
	if !named.SendName || named.ReportID() != "login-u1" || named.Fingerprint != one.Fingerprint {
		t.Fatalf("the login replaces the fingerprint, never both: %+v", named)
	}
	rolled, err := s.RollTelemetryFingerprint(ctx, "u1")
	if err != nil || rolled.Fingerprint == one.Fingerprint || !hex16.MatchString(rolled.Fingerprint) || rolled.SendName || rolled.ReportID() != rolled.Fingerprint {
		t.Fatalf("a roll gives a new anonymous id and turns the name off: %+v %v", rolled, err)
	}
	if other, _ := s.UserTelemetry(ctx, "u2"); other.Fingerprint != two.Fingerprint {
		t.Fatal("rolling one person's fingerprint changed another's")
	}
	if _, err := s.RollTelemetryFingerprint(ctx, "nobody"); err == nil {
		t.Fatal("rolling an unknown account must fail")
	}
}
