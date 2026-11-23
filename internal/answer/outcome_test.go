package answer

import (
	"errors"
	"strings"
	"testing"
)

// A handoff carries its outcome through the parser; an outcome outside the enum makes the answer invalid with an error that names the
// allowed values without quoting what the agent wrote (a held-reply note keeps only unquoted texts).
func TestParseHandoffOutcome(t *testing.T) {
	for _, outcome := range []string{OutcomeDone, OutcomePartial, OutcomeFailed} {
		parsed, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"report","handoff":true,"outcome":"`+outcome+`"}]}`), false)
		if err != nil || parsed.Messages[0].Outcome != outcome || !parsed.Messages[0].Handoff || len(parsed.Warnings) != 0 {
			t.Fatalf("%s: parsed=%+v err=%v", outcome, parsed, err)
		}
	}
	// The strict response schemas send null for an unused field; a handoff without an outcome still parses, because whether one is required
	// depends on the supervisor that ran the turn, which the store checks.
	for _, raw := range []string{`{"messages":[{"room":"room-1","body":"x","handoff":true,"outcome":null}]}`, `{"messages":[{"room":"room-1","body":"x","handoff":true}]}`} {
		parsed, err := Parse([]byte(raw), false)
		if err != nil || parsed.Messages[0].Outcome != "" {
			t.Fatalf("%s: parsed=%+v err=%v", raw, parsed, err)
		}
	}
	for _, bad := range []string{`"DONE"`, `"success"`, `" done"`} {
		_, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"x","handoff":true,"outcome":`+bad+`}]}`), false)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "message 0 has an invalid outcome; use done, partial or failed") || strings.ContainsAny(err.Error(), `"'`) {
			t.Fatalf("%s: err=%v", bad, err)
		}
	}
	if _, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"x","handoff":true,"outcome":3}]}`), false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a number outcome: err=%v", err)
	}
}

// An outcome means something only on a handoff: on another message it is dropped with a warning instead of costing the reply.
func TestParseDropsOutcomeOutsideAHandoff(t *testing.T) {
	parsed, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"x","outcome":"failed"},{"room":"room-1","body":"y","handoff":false,"outcome":"done"}]}`), false)
	if err != nil || parsed.Messages[0].Outcome != "" || parsed.Messages[1].Outcome != "" {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	if len(parsed.Warnings) != 2 || parsed.Warnings[0] != "message 0: outcome applies only to a handoff and was dropped" {
		t.Fatalf("warnings=%q", parsed.Warnings)
	}
}

func TestValidOutcome(t *testing.T) {
	for value, want := range map[string]bool{"done": true, "partial": true, "failed": true, "": false, "Done": false, "unknown": false} {
		if ValidOutcome(value) != want {
			t.Fatalf("ValidOutcome(%q) = %v", value, !want)
		}
	}
}
