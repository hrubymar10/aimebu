package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/supervisor"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

type discardSentryTransport struct{}

func (discardSentryTransport) Flush(time.Duration) bool              { return true }
func (discardSentryTransport) FlushWithContext(context.Context) bool { return true }
func (discardSentryTransport) Configure(sentry.ClientOptions)        {}
func (discardSentryTransport) SendEvent(*sentry.Event)               {}
func (discardSentryTransport) Close()                                {}

func telemetryLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func telemetryClient(t *testing.T) *supervisor.Client {
	t.Helper()
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	return supervisor.NewClient("mch_test", "key", "http://127.0.0.1:9897", journal, adapter.NewFake())
}

// A supervisor reports the way the server's settings say: they start it, replace it and stop it; the machine's own AIMEBU2_SENTRY_DSN, a DSN or
// "off", wins over them, and the bridge is told the same through its environment. (Tests never send: the transport here is a discard one.)
func TestSupervisorReportsByTheServersSettingsUnlessItsOwnEnvironmentSaysOtherwise(t *testing.T) {
	t.Cleanup(func() { telemetry.Init(telemetry.Config{}, nil) }) //nolint:errcheck // stops what the test started
	var stderr bytes.Buffer
	client := telemetryClient(t)
	const first, second = "https://k1@o1.ingest.example/1", "https://k2@o2.ingest.example/2"
	on := func(dsn string, rate float64) protocol.TelemetrySettings {
		return protocol.TelemetrySettings{Enabled: true, DSN: dsn, Environment: "prod", TracesSampleRate: rate}
	}
	dsnNow := func() string {
		browser := telemetry.Current().BrowserConfig()
		if browser == nil {
			return ""
		}
		return browser["dsn"].(string)
	}
	none := telemetryLookup(nil)

	applySupervisorTelemetry(on(first, 0.5), "aimebu v9", none, client, discardSentryTransport{}, &stderr)
	if dsnNow() != first || telemetry.Current().BrowserConfig()["traces_sample_rate"] != 0.5 || telemetry.Current().BrowserConfig()["environment"] != "prod" {
		t.Fatalf("started by the server's settings: %v %s", telemetry.Current().BrowserConfig(), stderr.String())
	}
	applySupervisorTelemetry(on(second, 0.25), "aimebu v9", none, client, discardSentryTransport{}, &stderr)
	if dsnNow() != second || telemetry.Current().BrowserConfig()["traces_sample_rate"] != 0.25 {
		t.Fatalf("a change reconfigures it: %v", telemetry.Current().BrowserConfig())
	}
	applySupervisorTelemetry(protocol.TelemetrySettings{}, "aimebu v9", none, client, discardSentryTransport{}, &stderr)
	if telemetry.Current() != nil {
		t.Fatal("turned off by the server's settings, it still reports")
	}

	// The machine's own setting wins: off stays off, a DSN of its own is not replaced.
	applySupervisorTelemetry(on(first, 0.5), "aimebu v9", telemetryLookup(map[string]string{"AIMEBU2_SENTRY_DSN": "off"}), client, discardSentryTransport{}, &stderr)
	if telemetry.Current() != nil {
		t.Fatalf("AIMEBU2_SENTRY_DSN=off on the machine lost to the server: %v", telemetry.Current().BrowserConfig())
	}
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://own@o9.ingest.example/9", Environment: "mine", TracesSampleRate: 1}, discardSentryTransport{}); err != nil {
		t.Fatal(err)
	}
	applySupervisorTelemetry(on(first, 0.5), "aimebu v9", telemetryLookup(map[string]string{"AIMEBU2_SENTRY_DSN": "https://own@o9.ingest.example/9"}), client, discardSentryTransport{}, &stderr)
	if dsnNow() != "https://own@o9.ingest.example/9" {
		t.Fatalf("the machine's own DSN was replaced by the server's: %s", dsnNow())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected output: %s", stderr.String())
	}
}

// The environment handed to the bridge is the configuration as the variables its own process reads, and "off" when reporting is off.
func TestTelemetryEnvOfIsWhatTheBridgeReads(t *testing.T) {
	config := telemetry.Config{DSN: "https://k@o1.ingest.example/1", Environment: "prod", TracesSampleRate: 0.5}
	got := telemetryEnvOf(config)
	if got["AIMEBU2_SENTRY_DSN"] != config.DSN || got["AIMEBU2_SENTRY_ENV"] != "prod" || got["AIMEBU2_SENTRY_TRACES_SAMPLE_RATE"] != "0.5" {
		t.Fatalf("on: %v", got)
	}
	parsed, err := telemetry.FromEnv(telemetryLookup(got), "aimebu v9")
	if err != nil || parsed.DSN != config.DSN || parsed.Environment != "prod" || parsed.TracesSampleRate != 0.5 {
		t.Fatalf("the bridge reads %+v from it: %v", parsed, err)
	}
	if off := telemetryEnvOf(telemetry.Config{}); len(off) != 1 || off["AIMEBU2_SENTRY_DSN"] != "off" {
		t.Fatalf("off: %v", off)
	}
}
