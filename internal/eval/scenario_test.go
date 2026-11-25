package eval

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestValidateRejects(t *testing.T) {
	live := func(mutate func(*Scenario)) Scenario {
		s := Scenario{ID: "x", Layer: LayerLive, Input: &Input{Notes: json.RawMessage(`[]`), AllowedTools: []string{}},
			Expect: Expect{Parse: ParseValid, MaxTurnsToValid: 1}}
		mutate(&s)
		return s
	}
	mock := func(mutate func(*Scenario)) Scenario {
		s := Scenario{ID: "m", Layer: LayerMock, Output: &Output{Kinds: []string{KindClaude}, Candidate: `{"messages":[]}`}, Expect: Expect{Parse: ParseValid}}
		mutate(&s)
		return s
	}
	for want, s := range map[string]Scenario{
		"non-empty word":            live(func(s *Scenario) { s.ID = "a b" }),
		"unknown layer":             live(func(s *Scenario) { s.Layer = "paid" }),
		"input.notes":               live(func(s *Scenario) { s.Input.Notes = nil }),
		"allowed_tools is required": live(func(s *Scenario) { s.Input.AllowedTools = nil }),
		"unknown input preset":      live(func(s *Scenario) { s.Input.Preset = "nope" }),
		"must be an array":          live(func(s *Scenario) { s.Input.Notes = json.RawMessage(`{}`) }),
		"unknown check op":          live(func(s *Scenario) { s.Expect.Checks = []Check{{Op: "vibes"}} }),
		"needs value":               live(func(s *Scenario) { s.Expect.Checks = []Check{{Op: "room_is"}} }),
		"is not an outcome":         live(func(s *Scenario) { s.Expect.Checks = []Check{{Op: "handoff_outcome", Value: "ok"}} }),
		"max_turns_to_valid >= 1":   live(func(s *Scenario) { s.Expect.MaxTurnsToValid = 0 }),
		"expects a valid answer":    live(func(s *Scenario) { s.Expect = Expect{Parse: ParseTerminal, Class: ClassHarness, MaxTurnsToValid: 1} }),
		"unknown harness kind":      mock(func(s *Scenario) { s.Output.Kinds = []string{"emacs"} }),
		"not both and not neither":  mock(func(s *Scenario) { s.Output.Candidate = "" }),
		"unknown fault":             mock(func(s *Scenario) { s.Output.Candidate, s.Output.Fault = "", "boom" }),
		"replays a recorded":        mock(func(s *Scenario) { s.Layer = LayerReplay }),
		"needs error_contains":      mock(func(s *Scenario) { s.Expect = Expect{Parse: ParseRepairable} }),
		"class harness":             mock(func(s *Scenario) { s.Expect = Expect{Parse: ParseTerminal} }),
		"unknown parse expectation": mock(func(s *Scenario) { s.Expect.Parse = "maybe" }),
		"for live scenarios":        mock(func(s *Scenario) { s.Expect.MaxTurnsToValid = 2 }),
		"no class or error":         mock(func(s *Scenario) { s.Expect.Class = ClassField }),
	} {
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error holding %q, got %v", want, err)
		}
	}
	if err := ValidateAll([]Scenario{mock(func(*Scenario) {}), mock(func(*Scenario) {})}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate ids: %v", err)
	}
}

func TestInputBuildExpandsPlaceholdersAndFiller(t *testing.T) {
	in := &Input{Notes: json.RawMessage(`[{"from":"{person}","room_id":"{task_room}"}]`), FillerTokens: 500, FillerMarker: "END-MARK"}
	notes, context, err := in.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(notes), PersonID) || !strings.Contains(string(notes), TaskRoom) || strings.Contains(string(notes), "{") && strings.Contains(string(notes), "{person}") {
		t.Fatalf("notes %s", notes)
	}
	if !strings.Contains(string(context), WorkerID) || !strings.Contains(string(context), "END-MARK") || len(context) < 2000 {
		t.Fatalf("context has %d bytes: %.200s", len(context), context)
	}
}
