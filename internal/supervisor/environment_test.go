package supervisor

import "testing"

func TestEnvironmentFingerprintTracksTurnSettingsWithoutSendingValues(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/private/home", "AIMEBU_TURN_MAX=2h", "OPENAI_API_KEY=secret-one", "AIMEBU_URL=http://127.0.0.1:9997"}
	first := EnvironmentFingerprint(base)
	if first == "" || len(first) != 64 {
		t.Fatalf("fingerprint = %q", first)
	}
	changedURL := append([]string(nil), base...)
	changedURL[4] = "AIMEBU_URL=http://127.0.0.1:9998"
	if got := EnvironmentFingerprint(changedURL); got != first {
		t.Fatal("transport URL changed the turn environment fingerprint")
	}
	changedKey := append([]string(nil), base...)
	changedKey[3] = "OPENAI_API_KEY=secret-two"
	if got := EnvironmentFingerprint(changedKey); got == first {
		t.Fatal("credential change was missed")
	}
	changedLimit := append([]string(nil), base...)
	changedLimit[2] = "AIMEBU_TURN_MAX=3h"
	if got := EnvironmentFingerprint(changedLimit); got == first {
		t.Fatal("turn limit change was missed")
	}
	if got := EnvironmentFingerprint([]string{base[3], base[2], base[1], base[0], base[4]}); got != first {
		t.Fatal("environment order changed the fingerprint")
	}
}
