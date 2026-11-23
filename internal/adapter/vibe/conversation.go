package vibe

import (
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// transcribe writes one entry of this turn to the conversation log: the model's messages and reasoning, an effect (a tool call, reported
// once it has finished, so one entry is both the call and its result) and an error notice. The turn's own prompt entry is not written:
// the supervisor has already recorded what the agent was asked.
func transcribe(conv *adapter.Conversation, item entry, line []byte) {
	if !conv.Active() {
		return
	}
	switch item.Type {
	case "message":
		if item.Role == "assistant" && item.GenerationStatus == "completed" {
			conv.Assistant(item.text())
		}
	case "reasoning":
		var value struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(line, &value) == nil {
			conv.Thinking(value.Text)
		}
	case "effect":
		var effect struct {
			Title  string `json:"title"`
			Detail struct {
				ToolName string          `json:"toolName"`
				Input    json.RawMessage `json:"input"`
			} `json:"detail"`
			State struct {
				Status     string          `json:"status"`
				OutputText string          `json:"outputText"`
				Error      json.RawMessage `json:"error"`
			} `json:"state"`
		}
		if json.Unmarshal(line, &effect) != nil {
			return
		}
		name := effect.Detail.ToolName
		if name == "" {
			name = effect.Title
		}
		conv.ToolCall(item.ID, name, adapter.ConversationJSON(effect.Detail.Input))
		output, failed := effect.State.OutputText, effect.State.Status != "" && effect.State.Status != "completed"
		if problem := adapter.ConversationJSON(effect.State.Error); problem != "" {
			failed = true
			if output == "" {
				output = problem
			} else {
				output += "\n" + problem
			}
		}
		conv.ToolResult(item.ID, name, output, failed)
	case "notice":
		if item.Level == "error" {
			conv.Error(item.Message)
		}
	}
}
