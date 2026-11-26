package adapter

import (
	"strings"
	"testing"
)

func TestAnswerToolSupport(t *testing.T) {
	for _, tc := range []struct {
		tool, version string
		supported     bool
	}{
		{"pi", "1.5.9", false}, {"pi", "1.6.0", false}, {"pi", "pi 1.6.1", true},
		{"opencode", "1.18.9", false}, {"opencode", "1.19.5", false}, {"opencode", "1.19.6", true},
		{"pi", "", false}, {"pi", "broken", false},
	} {
		got, reason := AnswerToolSupport(tc.tool, tc.version)
		if got != tc.supported {
			t.Fatalf("%s %q: got %t", tc.tool, tc.version, got)
		}
		if !got && tc.version != "" && !strings.Contains(reason, "update "+tc.tool+" to ") {
			t.Fatalf("missing upgrade reason: %q", reason)
		}
	}
}

func TestAnswerToolEndpoint(t *testing.T) {
	if got := AnswerToolEndpoint("http://127.0.0.1:9897/api/agent/mcp"); got != "http://127.0.0.1:9897/api/agent/mcp?answer_tool=1" {
		t.Fatalf("got %q", got)
	}
}

// OffersAnswerTool is the one test the OpenCode and Pi adapters apply before they list the tool for a turn and the supervisor
// reports on the turn's answer frame: a normal-mode turn of a supported harness version, and no other.
func TestOffersAnswerTool(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		spec       TurnSpec
		want       bool
	}{
		{"opencode at the supported version", "opencode", TurnSpec{Mode: ModeNormal, HarnessVersion: "1.19.6"}, true},
		{"opencode above the minimum", "opencode", TurnSpec{Mode: ModeNormal, HarnessVersion: "1.20.0"}, true},
		{"opencode below the minimum", "opencode", TurnSpec{Mode: ModeNormal, HarnessVersion: "1.19.5"}, false},
		{"pi at the supported version", "pi", TurnSpec{Mode: ModeNormal, HarnessVersion: "1.6.1"}, true},
		{"pi below the minimum", "pi", TurnSpec{Mode: ModeNormal, HarnessVersion: "1.6.0"}, false},
		{"an unprobed version", "opencode", TurnSpec{Mode: ModeNormal}, false},
		{"a harness with no answer tool", "claude", TurnSpec{Mode: ModeNormal, HarnessVersion: "2.0.0"}, false},
		{"a correction turn", "opencode", TurnSpec{Mode: ModeCorrection, HarnessVersion: "1.19.6"}, false},
		{"a diagnostic turn", "pi", TurnSpec{Mode: ModeDiagnostic, HarnessVersion: "1.6.1"}, false},
		{"a modeless spec (the adapters mark the endpoint only for a normal turn)", "opencode", TurnSpec{HarnessVersion: "1.19.6"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OffersAnswerTool(tc.tool, tc.spec); got != tc.want {
				t.Fatalf("OffersAnswerTool=%t, want %t", got, tc.want)
			}
		})
	}
}
