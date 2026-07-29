package main

import (
	"encoding/json"
	"strings"
)

type codexStateDetector struct{}

func (codexStateDetector) Name() string {
	return "codex"
}

func (codexStateDetector) Detect(line []byte) string {
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return ""
	}
	eventType, _ := event["type"].(string)
	if eventType == "" {
		return ""
	}
	itemType := codexItemType(event)
	switch eventType {
	case "item.started":
		switch itemType {
		case "mcp_tool_call", "command_execution", "file_change":
			return "tool_call"
		case "bus_wait":
			return "idle"
		}
	case "item.completed":
		switch itemType {
		case "bus_wait":
			if codexHasNonEmptyMessages(event) {
				return "thinking"
			}
		case "agent_message":
			return "idle"
		}
	}
	return ""
}

func codexItemType(event map[string]any) string {
	if item, ok := event["item"].(map[string]any); ok {
		if typ, ok := item["type"].(string); ok {
			// MCP server names are user-configurable. The documented name is
			// "aimebu", but the tool name is the stable contract here.
			if typ == "mcp_tool_call" && codexToolName(item) == "bus_wait" {
				return "bus_wait"
			}
			return typ
		}
	}
	if typ, ok := event["item_type"].(string); ok {
		return typ
	}
	return ""
}

func codexToolName(item map[string]any) string {
	tool, _ := item["tool"].(string)
	return tool
}

func codexHasNonEmptyMessages(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for key, value := range x {
			if key == "messages" {
				if messages, ok := value.([]any); ok && len(messages) > 0 {
					return true
				}
			}
			if codexHasNonEmptyMessages(value) {
				return true
			}
		}
	case string:
		// Codex wraps MCP text results as strings. bus_wait appends a listener
		// hint after the JSON response, so decode only the first JSON value.
		var decoded any
		if err := json.NewDecoder(strings.NewReader(x)).Decode(&decoded); err == nil {
			return codexHasNonEmptyMessages(decoded)
		}
	case []any:
		for _, value := range x {
			if codexHasNonEmptyMessages(value) {
				return true
			}
		}
	}
	return false
}
