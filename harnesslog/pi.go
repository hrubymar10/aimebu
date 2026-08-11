package harnesslog

// pi emits flat snake_case event types on its stdout JSON stream.
// Observed types (real corpus, one session): message_update, message_start,
// message_end, turn_start, turn_end, tool_execution_start, tool_execution_end,
// session. Structs carry Type plus plausible fields; refine from the runner's
// unknown/non-JSON samples rather than by reading raw logs.

var piKnown = map[string]bool{
	"message_update":        true,
	"message_start":         true,
	"message_end":           true,
	"turn_start":            true,
	"turn_end":              true,
	"tool_execution_start":  true,
	"tool_execution_end":    true,
	"tool_execution_update": true,
	"session":               true,
	"agent_start":           true,
	"agent_end":             true,
	"agent_settled":         true,
	"auto_retry_start":      true,
	"auto_retry_end":        true,
	"compaction_start":      true,
	"compaction_end":        true,
}

type PiMessageUpdate struct {
	Type    string `json:"type"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}
type PiMessageStart struct {
	Type string `json:"type"`
	Role string `json:"role,omitempty"`
}
type PiMessageEnd struct {
	Type         string `json:"type"`
	FinishReason string `json:"finish_reason,omitempty"`
}
type PiTurnStart struct {
	Type string `json:"type"`
}
type PiTurnEnd struct {
	Type string `json:"type"`
}
type PiToolExecutionStart struct {
	Type string `json:"type"`
	Tool string `json:"tool,omitempty"`
}
type PiToolExecutionEnd struct {
	Type    string `json:"type"`
	Tool    string `json:"tool,omitempty"`
	Success bool   `json:"success,omitempty"`
}
type PiToolExecutionUpdate struct {
	Type   string `json:"type"`
	Tool   string `json:"tool,omitempty"`
	Update string `json:"update,omitempty"`
}
type PiSession struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}
type PiAgentStart struct {
	Type      string `json:"type"`
	AgentID   string `json:"agent_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}
type PiAgentEnd struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}
type PiAgentSettled struct {
	Type    string `json:"type"`
	AgentID string `json:"agent_id,omitempty"`
}
type PiAutoRetryStart struct {
	Type    string `json:"type"`
	Attempt int    `json:"attempt,omitempty"`
}
type PiAutoRetryEnd struct {
	Type    string `json:"type"`
	Attempt int    `json:"attempt,omitempty"`
	Success bool   `json:"success,omitempty"`
}
type PiCompactionStart struct {
	Type string `json:"type"`
}
type PiCompactionEnd struct {
	Type string `json:"type"`
}

// ParsePi classifies one pi stdout line.
func ParsePi(line string) (ParsedEvent, ParseResult) {
	return classify(line, piKnown, HarnessPi)
}
