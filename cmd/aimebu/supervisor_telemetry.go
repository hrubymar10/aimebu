package main

import (
	"fmt"
	"io"
	"strconv"

	"github.com/getsentry/sentry-go"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// applySupervisorTelemetry takes the telemetry settings the server sent (in hello_ack, and again whenever they change) and starts, replaces or stops
// this supervisor's own reporting by them, and sets the environment the agent-tools bridge is started with, so the bridge reports the same way. A
// DSN in this machine's own environment (AIMEBU_SENTRY_DSN, a DSN or "off") wins over the server's settings, for the supervisor and for the bridge.
// transport replaces the network transport (tests); nil sends to the DSN.
func applySupervisorTelemetry(settings protocol.TelemetrySettings, version string, lookup func(string) (string, bool), client *supervisor.Client, transport sentry.Transport, stderr io.Writer) {
	config, err := telemetry.FromEnv(lookup, version)
	if err != nil {
		fmt.Fprintln(stderr, "telemetry:", err)
		return
	}
	if config.EnvSet {
		// This machine's own setting decides; its process already started by it. The bridge gets the same.
		client.SetTelemetryEnv(telemetryEnvOf(config))
		return
	}
	if !settings.Enabled || settings.DSN == "" {
		if _, err := telemetry.Init(telemetry.Config{}, nil); err != nil {
			fmt.Fprintln(stderr, "telemetry:", err)
		}
		client.SetTelemetryEnv(map[string]string{"AIMEBU_SENTRY_DSN": "off"})
		return
	}
	config.DSN, config.Source = settings.DSN, "server"
	if settings.Environment != "" {
		config.Environment = settings.Environment
	}
	config.TracesSampleRate = settings.TracesSampleRate
	if _, err := telemetry.Init(config, transport); err != nil {
		fmt.Fprintln(stderr, "telemetry:", err)
		client.SetTelemetryEnv(map[string]string{"AIMEBU_SENTRY_DSN": "off"})
		return
	}
	client.SetTelemetryEnv(telemetryEnvOf(config))
}

// telemetryEnvOf is a configuration as the variables the bridge's own process reads it from (see telemetry.FromEnv): the DSN, or "off".
func telemetryEnvOf(config telemetry.Config) map[string]string {
	if !config.Enabled() {
		return map[string]string{"AIMEBU_SENTRY_DSN": "off"}
	}
	return map[string]string{
		"AIMEBU_SENTRY_DSN": config.DSN, "AIMEBU_SENTRY_ENV": config.Environment,
		"AIMEBU_SENTRY_TRACES_SAMPLE_RATE": strconv.FormatFloat(config.TracesSampleRate, 'f', -1, 64),
	}
}
