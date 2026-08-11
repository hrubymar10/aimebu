package harnesslog

import "encoding/json"

// codex emits dotted noun.verb event types on its stdout JSON stream.
// Observed types: item.started, item.completed, thread.started, turn.started,
// turn.completed. (Some non-JSON stdout lines occur and are counted separately.)

var codexKnown = map[string]bool{
	"item.started":   true,
	"item.completed": true,
	"item.updated":   true,
	"thread.started": true,
	"turn.started":   true,
	"turn.completed": true,
	"turn.failed":    true,
	"error":          true,
}

type CodexItemStarted struct {
	Type     string `json:"type"`
	ItemID   string `json:"item_id,omitempty"`
	ItemType string `json:"item_type,omitempty"`
}
type CodexItemCompleted struct {
	Type   string `json:"type"`
	ItemID string `json:"item_id,omitempty"`
	Status string `json:"status,omitempty"`
}
type CodexThreadStarted struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id,omitempty"`
}
type CodexTurnStarted struct {
	Type   string `json:"type"`
	TurnID string `json:"turn_id,omitempty"`
}
type CodexTurnCompleted struct {
	Type   string `json:"type"`
	TurnID string `json:"turn_id,omitempty"`
}
type CodexTurnFailed struct {
	Type   string          `json:"type"`
	TurnID string          `json:"turn_id,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}
type CodexItemUpdated struct {
	Type   string `json:"type"`
	ItemID string `json:"item_id,omitempty"`
}
type CodexError struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// ParseCodex classifies one codex stdout line.
func ParseCodex(line string) (ParsedEvent, ParseResult) {
	return classify(line, codexKnown, HarnessCodex)
}
