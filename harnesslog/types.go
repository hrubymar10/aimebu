// Package harnesslog parses the structured stdout that AI harnesses emit, as
// captured by the aimebu agent wrapper's JSONL debug logs.
//
// The wrapper writes one JSON object per line to <config>/agents/agent-logs/
// <agent-id>-<spawn>.log when AIMEBU_AGENT_DEBUG is enabled. The event type
// "harness_stdout_raw" carries a "line" field whose value is the harness's own
// raw stdout line — and THAT inner payload is what differs per harness:
//
//   - pi:          flat snake_case types (message_update, tool_execution_start…)
//   - codex:       dotted noun.verb types (item.started, thread.started…)
//   - claude-code: message-role types (system, assistant, tool_progress,
//     rate_limit_event…)
//
// The structs here are the typed shapes for each harness's inner events. The
// corpus runner (see Runner) walks a log directory, classifies every line, and
// prints only aggregates — it never loads raw log content into a model's
// context. Iterate the structs until the unknown bucket approaches zero.
//
// This package imports only the Go standard library so it can be copied into
// another project verbatim.
package harnesslog

import "encoding/json"

// Harness is the harness a log file belongs to.
type Harness string

const (
	HarnessPi         Harness = "pi"
	HarnessClaudeCode Harness = "claude-code"
	HarnessCodex      Harness = "codex"
	HarnessVibe       Harness = "vibe"
	HarnessUnknown    Harness = ""
)

// Envelope is the outer aimebu wrapper JSONL record. Only the fields used for
// harness detection and stdout extraction are typed; the rest are ignored.
type Envelope struct {
	Event     string `json:"event"`
	Harness   string `json:"harness,omitempty"` // wrapper_start / session_id_parsed
	Command   string `json:"command,omitempty"` // harness_spawn
	Line      string `json:"line,omitempty"`    // harness_stdout_raw
	LineIndex int    `json:"line_index,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ParsedEvent is a classified inner harness event. Raw holds the inner line
// bytes so field shapes can be refined later without re-reading source logs.
type ParsedEvent struct {
	Harness Harness         `json:"-"`
	Type    string          `json:"type"`
	Raw     json.RawMessage `json:"-"`
}

// ParseResult is the outcome of classifying one harness_stdout_raw line.
type ParseResult int

const (
	Parsed      ParseResult = iota // inner JSON with a recognised type
	UnknownType                    // inner JSON whose type is not in the harness's struct set
	NonJSON                        // not valid JSON (often a truncated long line)
)

// FileReport is the aggregate for one .log file.
type FileReport struct {
	Path    string
	Harness Harness
	// Inner event-type histogram (only for Parsed lines).
	Types map[string]int
	// Unknown inner types (JSON with a type not in the harness's struct set).
	UnknownTypes map[string]int
	// Counts.
	Parsed  int
	Unknown int
	NonJSON int
	// Bounded samples of unknown/non-JSON lines (truncated), for struct iteration.
	Samples []string
}

// Report is the aggregate across a whole log directory.
type Report struct {
	Files        []FileReport
	ByCount      map[string]int            // parsed/unknown/nonjson totals
	Histogram    map[string]map[string]int // harness -> type -> count (parsed)
	UnknownTypes map[string]map[string]int // harness -> type -> count (unknown)
}

func newFileReport(path string, h Harness) *FileReport {
	return &FileReport{Path: path, Harness: h, Types: map[string]int{}, UnknownTypes: map[string]int{}}
}
