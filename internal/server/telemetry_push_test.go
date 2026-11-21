package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// A supervisor that takes telemetry settings gets the server's effective configuration when it connects (hello_ack) and again when the owner
// changes it in Settings; it carries no identity of a person; one that does not announce the capability is sent nothing.
func TestSupervisorsGetTheServersTelemetryConfigurationWhenTheyConnectAndWhenItChanges(t *testing.T) {
	withoutEnvDSN(t)
	f := newFixture(t)
	f.srv.telemetryTransport = discardTransport{}
	const dsn = "https://publickey@o1.ingest.example/7"
	if status, _, raw := decodeTelemetry(t, f, "PUT", map[string]any{"enabled": true, "dsn": dsn, "prompted": true}); status != 200 {
		t.Fatalf("save: %d %v", status, raw)
	}
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	got := make(chan protocol.TelemetrySettings, 8)
	client.OnTelemetry = func(settings protocol.TelemetrySettings) { got <- settings }
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = client.RunOnce(ctx) }()
	next := func(what string) protocol.TelemetrySettings {
		t.Helper()
		select {
		case settings := <-got:
			return settings
		case <-time.After(machineWaitGuard):
			t.Fatalf("no telemetry settings: %s", what)
			return protocol.TelemetrySettings{}
		}
	}
	first := next("on connect")
	if !first.Enabled || first.DSN != dsn || first.Environment == "" || first.TracesSampleRate <= 0 {
		t.Fatalf("on connect: %+v", first)
	}
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not become ready")
	}
	if status, _, raw := decodeTelemetry(t, f, "PUT", map[string]any{"enabled": false}); status != 200 {
		t.Fatalf("turn off: %d %v", status, raw)
	}
	if second := next("after turning reporting off"); second.Enabled || second.DSN != "" {
		t.Fatalf("after turning it off: %+v", second)
	}
	if status, _, raw := decodeTelemetry(t, f, "PUT", map[string]any{"enabled": true}); status != 200 {
		t.Fatalf("turn on: %d %v", status, raw)
	}
	if third := next("after turning it on again"); !third.Enabled || third.DSN != dsn {
		t.Fatalf("after turning it on again: %+v", third)
	}
}

// What the server tells a supervisor is configuration only: a machine that does not announce the capability is told nothing.
func TestTelemetryIsNotSentToAMachineThatDoesNotTakeIt(t *testing.T) {
	f := newFixture(t)
	if settings := f.srv.telemetryForMachine(t.Context(), &machineConn{}); settings != nil {
		t.Fatalf("a machine that does not announce it: %+v", settings)
	}
	if settings := f.srv.telemetryForMachine(t.Context(), nil); settings != nil {
		t.Fatalf("no machine: %+v", settings)
	}
}
