package server

import (
	"context"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/hrubymar10/aimebu/internal/telemetry"
	"net/http"
	"runtime"
	"testing"

	"github.com/goccy/go-json"
)

// The header of the page shows the version the server was built as: a signed-in account reads it, nobody else does.
func TestBuildInfoGivesTheVersionToSignedInAccountsOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.Version = "aimebu v9.8.7-3-gabc123"
	resp := f.request("GET", "/api/buildinfo", nil, false)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("signed in: %d", resp.StatusCode)
	}
	var info map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info["version"] != "aimebu v9.8.7-3-gabc123" || info["go_version"] != runtime.Version() || len(info) != 2 {
		t.Fatalf("answer %v", info)
	}
	anonymous, err := http.Get(f.http.URL + "/api/buildinfo")
	if err != nil {
		t.Fatal(err)
	}
	anonymous.Body.Close()
	if anonymous.StatusCode != 401 {
		t.Fatalf("without a session: %d, want 401", anonymous.StatusCode)
	}
}

type discardTransport struct{}

func (discardTransport) Flush(time.Duration) bool              { return true }
func (discardTransport) FlushWithContext(context.Context) bool { return true }
func (discardTransport) Configure(sentry.ClientOptions)        {}
func (discardTransport) SendEvent(*sentry.Event)               {}
func (discardTransport) Close()                                {}

// With telemetry on, the page is told how to start its SDK; with it off the answer has no `sentry` key at all (the page then never loads
// the SDK).
func TestBuildInfoCarriesTheSentryConfigOnlyWhenTelemetryIsOn(t *testing.T) {
	f := newFixture(t)
	read := func() map[string]any {
		resp := f.request("GET", "/api/buildinfo", nil, false)
		defer resp.Body.Close()
		var info map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	if _, present := read()["sentry"]; present {
		t.Fatal("telemetry off: buildinfo must not mention sentry")
	}
	_, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v9", TracesSampleRate: 0.5}, discardTransport{})
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()
	got, _ := read()["sentry"].(map[string]any)
	if got["dsn"] != "https://public@example.invalid/1" || got["environment"] != "test" || got["release"] != "v9" || got["traces_sample_rate"] != 0.5 {
		t.Fatalf("sentry config %v", got)
	}
}
