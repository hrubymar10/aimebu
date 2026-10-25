// Package harness classifies structured output emitted by supported tools.
package harness

import "github.com/goccy/go-json"

type CodexEvent struct {
	Type string
	Raw  json.RawMessage
}

var codexTypes = map[string]bool{
	"thread.started": true, "turn.started": true, "turn.completed": true,
	"turn.failed": true, "item.started": true, "item.updated": true,
	"item.completed": true, "error": true,
}

// ParseCodex retains the exact JSONL line for event-specific decoders.
func ParseCodex(line []byte) (CodexEvent, bool) {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &head) != nil || head.Type == "" {
		return CodexEvent{}, false
	}
	return CodexEvent{Type: head.Type, Raw: append(json.RawMessage(nil), line...)}, codexTypes[head.Type]
}

func CodexThreadID(event CodexEvent) string {
	if event.Type != "thread.started" {
		return ""
	}
	var value struct {
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(event.Raw, &value) != nil {
		return ""
	}
	return value.ThreadID
}

func CodexAgentMessage(event CodexEvent) (string, bool) {
	if event.Type != "item.completed" {
		return "", false
	}
	var value struct {
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal(event.Raw, &value) != nil || value.Item.Type != "agent_message" {
		return "", false
	}
	return value.Item.Text, true
}

// CodexCachedInput is the part of the turn's input the harness says was read from the prompt cache, when it says (nil otherwise).
func CodexCachedInput(event CodexEvent) *int64 {
	if event.Type != "turn.completed" {
		return nil
	}
	var value struct {
		Usage struct {
			CachedInputTokens *int64 `json:"cached_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(event.Raw, &value) != nil {
		return nil
	}
	return value.Usage.CachedInputTokens
}

// CodexCacheWrite is the share of input_tokens written to the prompt cache,
// when the CLI reports it.
func CodexCacheWrite(event CodexEvent) *int64 {
	if event.Type != "turn.completed" {
		return nil
	}
	var value struct {
		Usage struct {
			CacheWrite *int64 `json:"cache_write_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(event.Raw, &value) != nil {
		return nil
	}
	return value.Usage.CacheWrite
}

func CodexUsage(event CodexEvent) (input, output *int64) {
	if event.Type != "turn.completed" {
		return nil, nil
	}
	var value struct {
		Usage struct {
			InputTokens  *int64 `json:"input_tokens"`
			OutputTokens *int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(event.Raw, &value) != nil {
		return nil, nil
	}
	return value.Usage.InputTokens, value.Usage.OutputTokens
}

// CodexToolItem reports whether an item.started / item.completed event is a
// tool execution (command, MCP call, file change, search, ...) rather than
// model text or reasoning, along with the item ID.
func CodexToolItem(event CodexEvent) (id string, tool bool) {
	if event.Type != "item.started" && event.Type != "item.completed" {
		return "", false
	}
	var value struct {
		Item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"item"`
	}
	if json.Unmarshal(event.Raw, &value) != nil {
		return "", false
	}
	switch value.Item.Type {
	case "", "agent_message", "reasoning", "todo_list", "error":
		return "", false
	}
	return value.Item.ID, true
}
