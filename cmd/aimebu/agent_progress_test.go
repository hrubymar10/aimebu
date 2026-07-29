package main

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestAgentProgressConfigFromLookup(t *testing.T) {
	defaults, err := agentProgressConfigFromLookup(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if defaults.IdleTimeout != 600*time.Second || defaults.ForwardTimeout != 30*time.Minute {
		t.Fatalf("defaults = %#v", defaults)
	}

	values := map[string]string{
		agentProgressIdleEnv:    "2m",
		agentProgressForwardEnv: "20m",
	}
	config, err := agentProgressConfigFromLookup(func(name string) string {
		return values[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.IdleTimeout != 2*time.Minute || config.ForwardTimeout != 20*time.Minute {
		t.Fatalf("config = %#v", config)
	}

	values[agentProgressIdleEnv] = "nope"
	if _, err := agentProgressConfigFromLookup(func(name string) string {
		return values[name]
	}); err == nil {
		t.Fatal("expected invalid duration error")
	}
}

func TestPiWatchdogEventLine(t *testing.T) {
	for _, line := range []string{
		`{"type":"turn_start"}`,
		`{"type": "turn_start"}`,
		`{"type":"turn_end"}`,
		`{"type":"tool_execution_start","toolName":"read"}`,
		`{"type":"tool_execution_end","toolName":"read"}`,
	} {
		if !piWatchdogEventLine([]byte(line)) {
			t.Fatalf("watchdog event was filtered: %s", line)
		}
	}
	if piWatchdogEventLine([]byte(`{"type":"message_update","partial":"large"}`)) {
		t.Fatal("message_update should bypass watchdog JSON parsing")
	}
}

func TestAgentProgressDeadline(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	config := agentProgressConfig{
		IdleTimeout:    90 * time.Second,
		ForwardTimeout: 15 * time.Minute,
	}
	tests := []struct {
		name       string
		snapshot   agentProgressSnapshot
		wantReason agentProgressStall
		wantDelay  time.Duration
	}{
		{
			name: "idle detector catches silent turn",
			snapshot: agentProgressSnapshot{
				InTurn:       true,
				LastOutput:   now.Add(-2 * time.Minute),
				LastProgress: now.Add(-2 * time.Minute),
			},
			wantReason: agentProgressStallIdle,
			wantDelay:  -30 * time.Second,
		},
		{
			name: "forward detector catches continuous reasoning",
			snapshot: agentProgressSnapshot{
				InTurn:       true,
				LastOutput:   now,
				LastProgress: now.Add(-16 * time.Minute),
			},
			wantReason: agentProgressStallForward,
			wantDelay:  -time.Minute,
		},
		{
			name: "ordinary tool suspends both detectors",
			snapshot: agentProgressSnapshot{
				InTurn:        true,
				ToolsInFlight: 1,
				LastOutput:    now.Add(-time.Hour),
				LastProgress:  now.Add(-time.Hour),
			},
		},
		{
			name: "healthy bus wait uses its own deadline",
			snapshot: agentProgressSnapshot{
				InTurn:           true,
				ToolsInFlight:    1,
				BusWaitsInFlight: 1,
				BusWaitDeadline:  now.Add(5 * time.Minute),
			},
			wantDelay: 5 * time.Minute,
		},
		{
			name: "overdue bus wait is detected",
			snapshot: agentProgressSnapshot{
				InTurn:           true,
				ToolsInFlight:    1,
				BusWaitsInFlight: 1,
				BusWaitDeadline:  now.Add(-time.Second),
			},
			wantReason: agentProgressStallBusWait,
			wantDelay:  -time.Second,
		},
		{
			name: "parallel ordinary tool keeps overdue bus wait exempt",
			snapshot: agentProgressSnapshot{
				InTurn:           true,
				ToolsInFlight:    2,
				BusWaitsInFlight: 1,
				BusWaitDeadline:  now.Add(-time.Second),
			},
		},
		{
			name: "between turns has no deadline",
			snapshot: agentProgressSnapshot{
				LastOutput:   now.Add(-time.Hour),
				LastProgress: now.Add(-time.Hour),
			},
		},
		{
			name: "turn end gets an exit grace",
			snapshot: agentProgressSnapshot{
				LastOutput:   now,
				LastProgress: now,
				TurnEndedAt:  now,
			},
			wantDelay: agentProgressTurnEndGrace,
		},
		{
			name: "turn end expiry is detected",
			snapshot: agentProgressSnapshot{
				LastOutput:   now.Add(-agentProgressTurnEndGrace - time.Second),
				LastProgress: now.Add(-agentProgressTurnEndGrace - time.Second),
				TurnEndedAt:  now.Add(-agentProgressTurnEndGrace - time.Second),
			},
			wantReason: agentProgressStallTurnEnd,
			wantDelay:  -time.Second,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reason, deadline := agentProgressDeadline(test.snapshot, config, now)
			if reason != test.wantReason {
				t.Fatalf("reason = %q, want %q", reason, test.wantReason)
			}
			if test.wantDelay == 0 {
				if !deadline.IsZero() {
					t.Fatalf("deadline = %v, want zero", deadline)
				}
				return
			}
			if got := deadline.Sub(now); got != test.wantDelay {
				t.Fatalf("deadline delay = %v, want %v", got, test.wantDelay)
			}
		})
	}
}

func TestAgentProgressMonitorPiEvents(t *testing.T) {
	config := agentProgressConfig{
		IdleTimeout:    25 * time.Millisecond,
		ForwardTimeout: 80 * time.Millisecond,
	}

	t.Run("silent turn stalls", func(t *testing.T) {
		monitor := newAgentProgressMonitor(config)
		defer monitor.Close()
		_, _ = monitor.Write([]byte("{\"type\":\"turn_start\"}\n"))
		select {
		case reason := <-monitor.Stalled():
			if reason != agentProgressStallIdle {
				t.Fatalf("reason = %q", reason)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("silent turn did not stall")
		}
	})

	t.Run("thinking bytes avoid idle but not forward stall", func(t *testing.T) {
		monitor := newAgentProgressMonitor(config)
		defer monitor.Close()
		_, _ = monitor.Write([]byte("{\"type\":\"turn_start\"}\n"))
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		timeout := time.NewTimer(250 * time.Millisecond)
		defer timeout.Stop()
		for {
			select {
			case reason := <-monitor.Stalled():
				if reason != agentProgressStallForward {
					t.Fatalf("reason = %q, want %q", reason, agentProgressStallForward)
				}
				return
			case <-ticker.C:
				_, _ = monitor.Write([]byte("{\"type\":\"message_update\"}\n"))
			case <-timeout.C:
				t.Fatal("continuous output never hit forward-progress deadline")
			}
		}
	})

	t.Run("tool in flight is exempt", func(t *testing.T) {
		monitor := newAgentProgressMonitor(config)
		defer monitor.Close()
		_, _ = monitor.Write([]byte(
			"{\"type\":\"turn_start\"}\n" +
				"{\"type\":\"tool_execution_start\",\"toolName\":\"bash\",\"args\":{}}\n",
		))
		select {
		case reason := <-monitor.Stalled():
			t.Fatalf("tool was incorrectly stalled: %q", reason)
		case <-time.After(2 * config.ForwardTimeout):
		}
	})

	t.Run("tool start resets forward progress", func(t *testing.T) {
		monitor := newAgentProgressMonitor(config)
		defer monitor.Close()
		_, _ = monitor.Write([]byte("{\"type\":\"turn_start\"}\n"))
		time.Sleep(config.ForwardTimeout / 2)
		_, _ = monitor.Write([]byte(
			"{\"type\":\"tool_execution_start\",\"toolName\":\"read\",\"args\":{}}\n" +
				"{\"type\":\"tool_execution_end\",\"toolName\":\"read\"}\n",
		))
		select {
		case reason := <-monitor.Stalled():
			if reason != agentProgressStallIdle {
				t.Fatalf("reason = %q, want idle after output stops", reason)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("monitor did not restart after tool completion")
		}
	})
}

func TestPiBusWaitTimeout(t *testing.T) {
	if !piToolIsBusWait("aimebu_bus_wait") || !piToolIsBusWait("bus_wait") {
		t.Fatal("expected bus_wait tool aliases")
	}
	if got := piBusWaitTimeout(map[string]any{"timeout": float64(600)}); got != 600*time.Second {
		t.Fatalf("timeout = %v", got)
	}
	if got := piBusWaitTimeout(map[string]any{"timeout": float64(999)}); got != 600*time.Second {
		t.Fatalf("clamped timeout = %v", got)
	}
	if got := piBusWaitTimeout(nil); got != 30*time.Second {
		t.Fatalf("default timeout = %v", got)
	}
}

type piProgressFixtureRecord struct {
	At    time.Time       `json:"at"`
	Event piProgressEvent `json:"event"`
}

func replayPiProgressFixture(t *testing.T, path string) agentProgressSnapshot {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var snapshot agentProgressSnapshot
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record piProgressFixtureRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		snapshot.LastOutput = record.At
		snapshot = snapshot.observe(record.Event, record.At)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestAgentProgressCapturedPiFixtures(t *testing.T) {
	config := agentDefaultProgressConfig
	tests := []struct {
		name       string
		path       string
		now        time.Time
		wantReason agentProgressStall
	}{
		{
			name:       "mona resumed output and remained healthy until interrupted",
			path:       "testdata/pi-progress-mona.jsonl",
			now:        time.Date(2026, 7, 27, 9, 2, 11, 0, time.UTC),
			wantReason: "",
		},
		{
			name:       "the same session would hit the backstop if output stopped",
			path:       "testdata/pi-progress-mona.jsonl",
			now:        time.Date(2026, 7, 27, 9, 12, 10, 0, time.UTC),
			wantReason: agentProgressStallIdle,
		},
		{
			name:       "olive kept emitting without action or turn completion",
			path:       "testdata/pi-progress-olive.jsonl",
			now:        time.Date(2026, 7, 27, 11, 43, 51, 0, time.UTC),
			wantReason: agentProgressStallForward,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := replayPiProgressFixture(t, test.path)
			reason, _ := agentProgressDeadline(snapshot, config, test.now)
			if reason != test.wantReason {
				t.Fatalf("reason = %q, want %q; snapshot = %#v", reason, test.wantReason, snapshot)
			}
		})
	}
}

func TestAgentProgressMustNotFire(t *testing.T) {
	config := agentDefaultProgressConfig

	t.Run("drew slow active turn", func(t *testing.T) {
		started := time.Date(2026, 7, 27, 8, 5, 38, 0, time.UTC)
		snapshot := agentProgressSnapshot{
			InTurn:       true,
			LastOutput:   started,
			LastProgress: started,
		}
		if reason, _ := agentProgressDeadline(snapshot, config, started.Add(11*time.Second)); reason != "" {
			t.Fatalf("slow working turn stalled after 11 seconds: %q", reason)
		}
	})

	t.Run("healthy 600 second bus wait", func(t *testing.T) {
		started := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
		snapshot := agentProgressSnapshot{
			InTurn:       true,
			LastOutput:   started,
			LastProgress: started,
		}.observe(piProgressEvent{
			Type:     "tool_execution_start",
			ToolName: "aimebu_bus_wait",
			Args:     map[string]any{"timeout": float64(600)},
		}, started)
		if reason, deadline := agentProgressDeadline(snapshot, config, started.Add(659*time.Second)); reason != "" {
			t.Fatalf("healthy bus_wait stalled: %q at %v", reason, deadline)
		}
	})

	t.Run("parallel tool remains exempt until every tool ends", func(t *testing.T) {
		started := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
		snapshot := agentProgressSnapshot{
			InTurn:       true,
			LastOutput:   started,
			LastProgress: started,
		}
		snapshot = snapshot.observe(piProgressEvent{Type: "tool_execution_start", ToolName: "read"}, started)
		snapshot = snapshot.observe(piProgressEvent{Type: "tool_execution_start", ToolName: "bash"}, started)
		snapshot = snapshot.observe(piProgressEvent{Type: "tool_execution_end", ToolName: "read"}, started.Add(time.Hour))
		if reason, _ := agentProgressDeadline(snapshot, config, started.Add(time.Hour)); reason != "" {
			t.Fatalf("remaining parallel tool was not exempt: %q", reason)
		}
	})
}
