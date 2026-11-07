package server

import (
	"os"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

func decodeTelemetry(t *testing.T, f *fixture, method string, body any) (int, telemetryView, map[string]any) {
	t.Helper()
	resp := f.request(method, "/api/telemetry/settings", body, method != "GET")
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(raw)
	var view telemetryView
	_ = json.Unmarshal(encoded, &view)
	return resp.StatusCode, view, raw
}

// The environment variable is "off" in tests (testenv); a test about Settings takes it away for its length.
func withoutEnvDSN(t *testing.T) {
	t.Helper()
	previous, had := os.LookupEnv("AIMEBU2_SENTRY_DSN")
	if err := os.Unsetenv("AIMEBU2_SENTRY_DSN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		telemetry.Init(telemetry.Config{}, nil) //nolint:errcheck // stops a reporter the test started
		if had {
			os.Setenv("AIMEBU2_SENTRY_DSN", previous)
		}
	})
}

func TestErrorReportsSettingsAndTheConsentPopup(t *testing.T) {
	f := newFixture(t) // not parallel: the reporter is process-wide
	withoutEnvDSN(t)
	f.srv.telemetryTransport = discardTransport{}
	saved := telemetry.DefaultDSN
	telemetry.DefaultDSN = "https://shipped@h.example/1"
	t.Cleanup(func() { telemetry.DefaultDSN = saved })

	// A new install: the popup is due, the toggle starts on because a default ships, the field holds the default, and nothing is sent yet.
	status, view, _ := decodeTelemetry(t, f, "GET", nil)
	if status != 200 || view.Prompted || !view.Enabled || view.DSN != "https://shipped@h.example/1" || view.DefaultDSN != view.DSN || view.EnvLocked || view.Active {
		t.Fatalf("fresh: %d %+v", status, view)
	}
	me := f.request("GET", "/api/me", nil, false)
	var session map[string]any
	_ = json.NewDecoder(me.Body).Decode(&session)
	me.Body.Close()
	if session["telemetry_prompt"] != true {
		t.Fatalf("the owner is asked once: %v", session)
	}
	if err := f.srv.ApplyTelemetry(t.Context()); err != nil || telemetry.Current() != nil {
		t.Fatalf("nothing is sent before the popup was answered: %v %v", err, telemetry.Current())
	}

	// A bad DSN is refused and changes nothing.
	if status, _, raw := decodeTelemetry(t, f, "PUT", map[string]any{"dsn": "not a dsn"}); status != 400 || raw["code"] != "invalid_dsn" && !strings.Contains(toString(raw), "invalid_dsn") {
		t.Fatalf("bad DSN: %d %v", status, raw)
	}
	// Answering with the toggle on starts reporting at once, no restart.
	status, view, _ = decodeTelemetry(t, f, "PUT", map[string]any{"prompted": true, "enabled": true})
	if status != 200 || !view.Prompted || !view.Active || telemetry.Current() == nil {
		t.Fatalf("saved on: %d %+v", status, view)
	}
	me = f.request("GET", "/api/me", nil, false)
	session = nil
	_ = json.NewDecoder(me.Body).Decode(&session)
	me.Body.Close()
	if _, asked := session["telemetry_prompt"]; asked {
		t.Fatalf("the popup comes once: %v", session)
	}
	// Turning it off stops it, an own DSN replaces the default, and clearing the field goes back to the default.
	if _, view, _ = decodeTelemetry(t, f, "PUT", map[string]any{"enabled": false}); view.Active || view.Enabled || telemetry.Current() != nil {
		t.Fatalf("off: %+v", view)
	}
	if _, view, _ = decodeTelemetry(t, f, "PUT", map[string]any{"enabled": true, "dsn": " https://mine@h.example/9 "}); !view.Active || view.DSN != "https://mine@h.example/9" {
		t.Fatalf("own DSN: %+v", view)
	}
	// Saving with the default showing stores nothing: the install keeps following the build's default.
	if _, view, _ = decodeTelemetry(t, f, "PUT", map[string]any{"dsn": "https://shipped@h.example/1"}); view.DSN != "https://shipped@h.example/1" {
		t.Fatalf("default as chosen: %+v", view)
	}
	if stored, _ := f.srv.storedTelemetry(t.Context()); stored.DSN != "" {
		t.Fatalf("the default DSN must not be stored as a choice, got %q", stored.DSN)
	}
	if status, _, _ := decodeTelemetry(t, f, "PUT", map[string]any{"dsn": "http://k@sentry.example.com/1"}); status != 400 {
		t.Fatalf("a plain-http DSN to another host is refused: %d", status)
	}
	if _, view, _ = decodeTelemetry(t, f, "PUT", map[string]any{"dsn": ""}); view.DSN != "https://shipped@h.example/1" {
		t.Fatalf("an empty field means the default: %+v", view)
	}
	// The environment wins, and says so.
	os.Setenv("AIMEBU2_SENTRY_DSN", "off")
	if _, view, _ = decodeTelemetry(t, f, "PUT", map[string]any{"enabled": true}); view.Active || !view.EnvLocked {
		t.Fatalf("env off must win: %+v", view)
	}
}

func TestErrorReportsSettingsAreForAdminsOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	for _, method := range []string{"GET", "PUT"} {
		resp := f.requestAs(t, cookie, csrf, method, "/api/telemetry/settings", map[string]any{"prompted": true})
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("%s as a plain member: %d", method, resp.StatusCode)
		}
	}
	me := f.requestAs(t, cookie, csrf, "GET", "/api/me", nil)
	var session map[string]any
	_ = json.NewDecoder(me.Body).Decode(&session)
	me.Body.Close()
	if _, asked := session["telemetry_prompt"]; asked {
		t.Fatal("only the owner is asked")
	}
}

func toString(v any) string { b, _ := json.Marshal(v); return string(b) }
