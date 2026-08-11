package harnesslog

import "encoding/json"

// claude-code emits message-role event types on its stdout JSON stream.
// Observed types: system, assistant, tool_progress, user, rate_limit_event.
// rate_limit_event carries the quota signal (a "status: rejected" payload);
// it appears in real corpora, so it is a live sample.

var claudeCodeKnown = map[string]bool{
	"system":           true,
	"assistant":        true,
	"tool_progress":    true,
	"user":             true,
	"rate_limit_event": true,
	"result":           true,
}

type ClaudeSystem struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}
type ClaudeAssistant struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
}
type ClaudeToolProgress struct {
	Type string `json:"type"`
	Tool string `json:"tool,omitempty"`
}
type ClaudeUser struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
}
type ClaudeRateLimitEvent struct {
	Type   string `json:"type"`
	Status string `json:"status,omitempty"` // "rejected" when quota-blocked
}
type ClaudeResult struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// ParseClaudeCode classifies one claude-code stdout line.
func ParseClaudeCode(line string) (ParsedEvent, ParseResult) {
	return classify(line, claudeCodeKnown, HarnessClaudeCode)
}
