package main

import "encoding/json"

type claudeCodeStateDetector struct{}

func (claudeCodeStateDetector) Name() string {
	return "claude-code"
}

func (claudeCodeStateDetector) Detect(line []byte) string {
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return ""
	}
	eventType, _ := event["type"].(string)
	switch eventType {
	case "assistant":
		if claudeAssistantHasToolUse(event) {
			return "tool_call"
		}
		if claudeAssistantHasTextOrThinking(event) {
			return "thinking"
		}
	case "system":
		if subtype, _ := event["subtype"].(string); subtype == "status" {
			if status, _ := event["status"].(string); status == "requesting" {
				return "thinking"
			}
		}
	case "stream_event":
		if claudeStreamEventType(event) == "message_start" {
			return "thinking"
		}
	case "result":
		return "idle"
	}
	return ""
}

// claudeStreamEventType returns the nested stream event type
// (e.g. "message_start"). Claude always nests it under "event"; there is no
// flattened top-level form.
func claudeStreamEventType(event map[string]any) string {
	nested, _ := event["event"].(map[string]any)
	if nested == nil {
		return ""
	}
	typ, _ := nested["type"].(string)
	return typ
}

func claudeAssistantHasToolUse(event map[string]any) bool {
	return claudeAssistantContentHasType(event, "tool_use")
}

func claudeAssistantHasTextOrThinking(event map[string]any) bool {
	return claudeAssistantContentHasType(event, "text") || claudeAssistantContentHasType(event, "thinking")
}

func claudeAssistantContentHasType(event map[string]any, want string) bool {
	message, _ := event["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, item := range content {
		block, _ := item.(map[string]any)
		if blockType, _ := block["type"].(string); blockType == want {
			return true
		}
	}
	return false
}
