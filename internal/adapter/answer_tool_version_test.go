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
