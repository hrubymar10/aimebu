// Package harness classifies structured output emitted by supported tools.
package harness

import (
	"strings"

	"github.com/goccy/go-json"
)

type ClaudeEvent struct {
	Type string
	Raw  json.RawMessage
}

var claudeTypes = map[string]bool{
	"assistant": true, "result": true, "system": true, "tool_progress": true,
	"user": true, "rate_limit_event": true,
}

// ParseClaude classifies one stream-json line. Unknown JSON event types are
// retained so callers can surface them as activity without treating them as a
// protocol failure.
func ParseClaude(line []byte) (ClaudeEvent, bool) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return ClaudeEvent{}, false
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &head) != nil || head.Type == "" {
		return ClaudeEvent{}, false
	}
	return ClaudeEvent{Type: head.Type, Raw: append(json.RawMessage(nil), line...)}, claudeTypes[head.Type]
}

type ClaudeCompaction struct {
	PreTokens  int64 `json:"pre_tokens"`
	PostTokens int64 `json:"post_tokens"`
}

func ParseClaudeCompaction(event ClaudeEvent) (ClaudeCompaction, bool) {
	var value struct {
		Type            string            `json:"type"`
		Subtype         string            `json:"subtype"`
		CompactMetadata *ClaudeCompaction `json:"compact_metadata"`
	}
	if json.Unmarshal(event.Raw, &value) != nil || value.Type != "system" || value.Subtype != "compact_boundary" || value.CompactMetadata == nil {
		return ClaudeCompaction{}, false
	}
	return *value.CompactMetadata, true
}

// ClaudeToolUses lists the tool_use IDs an assistant message starts and the
// tool_result IDs a user message finishes.
func ClaudeToolUses(event ClaudeEvent) (started, finished []string) {
	if event.Type != "assistant" && event.Type != "user" {
		return nil, nil
	}
	var value struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(event.Raw, &value) != nil {
		return nil, nil
	}
	var blocks []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		ToolUseID string `json:"tool_use_id"`
	}
	if json.Unmarshal(value.Message.Content, &blocks) != nil {
		return nil, nil
	}
	for _, block := range blocks {
		switch {
		case event.Type == "assistant" && block.Type == "tool_use":
			started = append(started, block.ID)
		case event.Type == "user" && block.Type == "tool_result":
			finished = append(finished, block.ToolUseID)
		}
	}
	return started, finished
}
