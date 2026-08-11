package harnesslog

import (
	"encoding/json"
	"strings"
)

// classify extracts the inner "type" from a harness stdout line and decides
// whether it is a recognised event (Parsed), an unrecognised type (UnknownType),
// or not JSON at all (NonJSON — often a truncated long line, counted separately
// so it never blocks the unknown bucket from reaching zero for struct reasons).
// Raw holds the inner line bytes for later field-shape refinement.
func classify(line string, known map[string]bool, h Harness) (ParsedEvent, ParseResult) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ParsedEvent{}, NonJSON
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(trimmed), &head); err != nil {
		return ParsedEvent{}, NonJSON
	}
	ev := ParsedEvent{Harness: h, Type: head.Type, Raw: json.RawMessage(trimmed)}
	if head.Type == "" {
		return ev, NonJSON
	}
	if !known[head.Type] {
		return ev, UnknownType
	}
	return ev, Parsed
}
