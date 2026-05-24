package main

import "testing"

func TestPiStateDetectorDetect(t *testing.T) {
	tests := []struct {
		name   string
		lines  []string
		states []string
	}{
		{
			name: "thinking tool and turn end",
			lines: []string{
				`{"type":"thinking_start"}`,
				`{"type":"thinking_delta","text":"reasoning"}`,
				`{"type":"tool_execution_start","name":"bus_wait"}`,
				`{"type":"tool_execution_end","name":"bus_wait"}`,
				`{"type":"turn_end"}`,
			},
			states: []string{"thinking", "", "tool_call", "thinking", "idle"},
		},
		{
			name: "tool from idle restores idle",
			lines: []string{
				`{"type":"tool_execution_start","name":"read_file"}`,
				`{"type":"tool_execution_end","name":"read_file"}`,
			},
			states: []string{"tool_call", "idle"},
		},
		{
			name: "unknown and invalid events ignored",
			lines: []string{
				`{"type":"response_created"}`,
				`not-json`,
			},
			states: []string{"", ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			det := &piStateDetector{}
			for i, line := range tt.lines {
				if got := det.Detect([]byte(line)); got != tt.states[i] {
					t.Fatalf("Detect line %d = %q, want %q", i, got, tt.states[i])
				}
			}
		})
	}
}

func TestNewStateDetectorPi(t *testing.T) {
	if got := newStateDetector("pi"); got == nil || got.Name() != "pi" {
		t.Fatalf("newStateDetector(pi) = %T, want pi detector", got)
	}
}
