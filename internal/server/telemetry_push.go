package server

import (
	"context"
	"os"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// telemetryForMachine is the telemetry configuration a supervisor is told: nil for one that does not take it (an older one, or a client that
// leaves OnTelemetry unset).
func (s *Server) telemetryForMachine(ctx context.Context, machine *machineConn) *protocol.TelemetrySettings {
	if machine == nil || !machine.supportsTelemetryConfig {
		return nil
	}
	settings, err := s.effectiveTelemetry(ctx)
	if err != nil {
		return nil
	}
	return settings
}

// effectiveTelemetry is the configuration the server reports under (its environment, else the stored settings, once the owner answered the
// consent popup), as a supervisor gets it. It holds no identity of a person.
func (s *Server) effectiveTelemetry(ctx context.Context) (*protocol.TelemetrySettings, error) {
	stored, err := s.storedTelemetry(ctx)
	if err != nil {
		return nil, err
	}
	config, err := telemetry.FromEnv(os.LookupEnv, s.Version)
	if err != nil {
		return nil, err
	}
	config = config.Resolve(stored)
	if !config.Enabled() {
		return &protocol.TelemetrySettings{}, nil
	}
	return &protocol.TelemetrySettings{Enabled: true, DSN: config.DSN, Environment: config.Environment, TracesSampleRate: config.TracesSampleRate}, nil
}

// pushTelemetry sends the effective telemetry configuration to every connected supervisor that takes it; a supervisor that connects later gets
// it in hello_ack. It is called when the settings change.
func (s *Server) pushTelemetry(ctx context.Context) {
	settings, err := s.effectiveTelemetry(ctx)
	if err != nil {
		return
	}
	s.machineMu.Lock()
	machines := make([]*machineConn, 0, len(s.machines))
	for _, machine := range s.machines {
		if machine.supportsTelemetryConfig {
			machines = append(machines, machine)
		}
	}
	s.machineMu.Unlock()
	for _, machine := range machines {
		_ = machine.send(protocol.Frame{V: protocol.Version, Type: protocol.TelemetryConfig, Telemetry: settings})
	}
}
