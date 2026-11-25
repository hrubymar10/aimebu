package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-json"
)

// Fixed identities the presets and the checks use, so a scenario file can say {dispatcher} instead of an id.
const (
	WorkerID     = "agt_01EVALWORKER0000000000000"
	DispatcherID = "agt_01EVALDISPATCH000000000000"
	OtherAgentID = "agt_01EVALOTHERAGENT0000000000"
	PersonID     = "usr_01EVALPERSON00000000000000"
	TaskRoom     = "room_01EVALTASKROOM00000000000"
	HomeRoom     = "room_01EVALHOMEROOM00000000000"
)

// Layers.
const (
	LayerReplay = "replay"
	LayerMock   = "mock"
	LayerLive   = "live"
)

// Parse expectations.
const (
	ParseValid      = "valid"      // the candidate is accepted by the answer parser
	ParseRepairable = "repairable" // the parser rejects it with a reason the repair path can send back to the agent
	ParseTerminal   = "terminal"   // the harness ended without an answer a repair could fix
)

// Error classes.
const (
	ClassFormat    = "format"    // not one JSON object: prose around it, a second fence, a bare array, truncated or invalid JSON
	ClassField     = "field"     // JSON, but a field breaks the contract: unknown field, wrong type, a handoff without an outcome
	ClassBehaviour = "behaviour" // a valid answer that does the wrong thing: wrong room, spoke when silence was right, no mention
	ClassHarness   = "harness"   // the CLI crashed, timed out or was limited; not the model's answer
)

// Scenario is one case of any layer. Live scenarios carry the turn input and checks; replay and mock cases carry the harness output and the
// expected parse result.
type Scenario struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Layer string `json:"layer"`
	// Input is the turn the supervisor would hand the adapter (live; also read by the universal checks of a replay case that has one).
	Input *Input `json:"input,omitempty"`
	// OnlyKinds, when set, limits a live scenario to those harness kinds (the unchanged-context marker is sent to Claude only).
	OnlyKinds []string `json:"only_kinds,omitempty"`
	// Output is the harness output (replay and mock).
	Output *Output `json:"output,omitempty"`
	Expect Expect  `json:"expect"`
}

// Input is a turn input. Preset names a built-in context (see Presets); Context keys replace the preset's.
type Input struct {
	Preset       string                     `json:"preset,omitempty"`
	Notes        json.RawMessage            `json:"notes"`
	Context      map[string]json.RawMessage `json:"context,omitempty"`
	AllowedTools []string                   `json:"allowed_tools"`
	// FillerTokens adds a knowledge block of about that many tokens of deterministic filler, then FillerMarker as its last line.
	FillerTokens int    `json:"filler_tokens,omitempty"`
	FillerMarker string `json:"filler_marker,omitempty"`
}

// Output is what the harness prints. Exactly one of StreamFile (a recorded stream, a path relative to the module root) and Candidate (the
// text the model answers with, wrapped in the stream format of each Kinds entry) is set.
type Output struct {
	Kinds      []string `json:"kinds"`
	StreamFile string   `json:"stream_file,omitempty"`
	Candidate  string   `json:"candidate,omitempty"`
	// Fault is "", "crash" (the harness exits 17 after some output, with no answer) or "silent" (it prints nothing and exits 0).
	Fault string `json:"fault,omitempty"`
}

// Expect is the expected result.
type Expect struct {
	Parse string `json:"parse"`
	// ErrorContains is a text the parser's error must hold (repairable).
	ErrorContains string `json:"error_contains,omitempty"`
	Class         string `json:"class,omitempty"`
	// Strict says the answer is accepted with no tolerance warning; WarningContains names a warning it must carry.
	Strict          bool     `json:"strict,omitempty"`
	WarningContains []string `json:"warning_contains,omitempty"`
	// AnswerContains is a text the extracted candidate must hold (valid or repairable); it pins what the adapter cut out of the stream.
	AnswerContains string  `json:"answer_contains,omitempty"`
	Checks         []Check `json:"checks,omitempty"`
	// MaxTurnsToValid is, for a live run, the most turns (the first try plus repairs) the answer may take to be valid.
	MaxTurnsToValid int `json:"max_turns_to_valid,omitempty"`
}

// Check is one predicate on the parsed answer. Value and Values may use {worker}, {dispatcher}, {other_agent}, {person}, {task_room}, {home_room}.
type Check struct {
	Op     string   `json:"op"`
	Value  string   `json:"value,omitempty"`
	Values []string `json:"values,omitempty"`
	Min    int      `json:"min,omitempty"`
	Max    int      `json:"max,omitempty"`
}

// LoadDir reads every *.json scenario of a directory, sorted by file name.
func LoadDir(dir string) ([]Scenario, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []Scenario
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := strictUnmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		out = append(out, s)
	}
	return out, nil
}

// Validate says what is wrong with a scenario, or nothing.
func (s Scenario) Validate() error {
	if s.ID == "" || strings.ContainsAny(s.ID, " /\\") {
		return fmt.Errorf("scenario id %q must be a non-empty word", s.ID)
	}
	switch s.Layer {
	case LayerLive:
		for _, kind := range s.OnlyKinds {
			if !validKind(kind) {
				return fmt.Errorf("%s: unknown harness kind %q in only_kinds", s.ID, kind)
			}
		}
		if s.Input == nil || len(s.Input.Notes) == 0 {
			return fmt.Errorf("%s: a live scenario needs input.notes", s.ID)
		}
		if s.Output != nil {
			return fmt.Errorf("%s: a live scenario has no output", s.ID)
		}
		if s.Input.AllowedTools == nil {
			return fmt.Errorf("%s: input.allowed_tools is required (an empty list means no tools)", s.ID)
		}
		if _, _, err := s.Input.Build(); err != nil {
			return fmt.Errorf("%s: %w", s.ID, err)
		}
	case LayerReplay, LayerMock:
		if s.Output == nil || len(s.Output.Kinds) == 0 {
			return fmt.Errorf("%s: output.kinds is required", s.ID)
		}
		for _, kind := range s.Output.Kinds {
			if !validKind(kind) {
				return fmt.Errorf("%s: unknown harness kind %q", s.ID, kind)
			}
		}
		if (s.Output.StreamFile == "") == (s.Output.Candidate == "" && s.Output.Fault == "") {
			return fmt.Errorf("%s: set output.stream_file, or output.candidate / output.fault, not both and not neither", s.ID)
		}
		if s.Layer == LayerReplay && s.Output.StreamFile == "" {
			return fmt.Errorf("%s: a replay case replays a recorded stream_file", s.ID)
		}
		if s.Layer == LayerMock && s.Output.StreamFile != "" {
			return fmt.Errorf("%s: a mock case plays a canned candidate or fault", s.ID)
		}
		switch s.Output.Fault {
		case "", "crash", "silent":
		default:
			return fmt.Errorf("%s: unknown fault %q", s.ID, s.Output.Fault)
		}
		if s.Output.Fault != "" && s.Output.Candidate != "" {
			return fmt.Errorf("%s: a fault has no candidate", s.ID)
		}
		if s.Input != nil {
			if _, _, err := s.Input.Build(); err != nil {
				return fmt.Errorf("%s: %w", s.ID, err)
			}
		}
	default:
		return fmt.Errorf("%s: unknown layer %q", s.ID, s.Layer)
	}
	switch s.Expect.Parse {
	case ParseValid:
		if s.Expect.Class != "" || s.Expect.ErrorContains != "" {
			return fmt.Errorf("%s: a valid expectation has no class or error", s.ID)
		}
	case ParseRepairable:
		if s.Expect.ErrorContains == "" || (s.Expect.Class != ClassFormat && s.Expect.Class != ClassField) {
			return fmt.Errorf("%s: a repairable expectation needs error_contains and class format or field", s.ID)
		}
		if len(s.Expect.Checks) > 0 || s.Expect.Strict {
			return fmt.Errorf("%s: a repairable answer has no checks and is not strict", s.ID)
		}
	case ParseTerminal:
		if s.Expect.Class != ClassHarness {
			return fmt.Errorf("%s: a terminal expectation has class harness", s.ID)
		}
	default:
		return fmt.Errorf("%s: unknown parse expectation %q", s.ID, s.Expect.Parse)
	}
	for _, check := range s.Expect.Checks {
		if err := check.validate(); err != nil {
			return fmt.Errorf("%s: %w", s.ID, err)
		}
	}
	if s.Layer == LayerLive && s.Expect.Parse != ParseValid {
		return fmt.Errorf("%s: a live scenario expects a valid answer after at most max_turns_to_valid turns", s.ID)
	}
	if s.Layer == LayerLive && s.Expect.MaxTurnsToValid < 1 {
		return fmt.Errorf("%s: a live scenario needs max_turns_to_valid >= 1", s.ID)
	}
	if s.Layer != LayerLive && s.Expect.MaxTurnsToValid != 0 {
		return fmt.Errorf("%s: max_turns_to_valid is for live scenarios", s.ID)
	}
	return nil
}

// ValidateAll validates every scenario and says if two share an id.
func ValidateAll(all []Scenario) error {
	seen := map[string]bool{}
	for _, s := range all {
		if err := s.Validate(); err != nil {
			return err
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate scenario id %s", s.ID)
		}
		seen[s.ID] = true
	}
	return nil
}

func strictUnmarshal(data []byte, v any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}
