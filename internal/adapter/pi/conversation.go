package pi

import (
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// transcribe writes what one `pi --mode json` event says to the conversation log: the model's text and thinking when a message ends, a
// tool call with its arguments when it starts and its result when it ends, and a provider failure. Pi reports an assistant message at
// message_end and again at turn_end; only the first is written.
func transcribe(conv *adapter.Conversation, event piEvent, line []byte) {
	if !conv.Active() {
		return
	}
	switch event.Type {
	case "message_end":
		if event.Message.Role != "assistant" {
			return
		}
		var blocks []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		}
		_ = json.Unmarshal(event.Message.Content, &blocks)
		var thinking, text strings.Builder
		for _, block := range blocks {
			switch block.Type {
			case "thinking":
				thinking.WriteString(block.Thinking)
			case "text":
				text.WriteString(block.Text)
			}
		}
		conv.Thinking(thinking.String())
		conv.Assistant(text.String())
		if event.Message.StopReason == "error" {
			conv.Error(event.Message.ErrorMessage)
		}
	case "tool_execution_start", "tool_execution_end":
		var tool struct {
			ToolName string          `json:"toolName"`
			Args     json.RawMessage `json:"args"`
			Result   json.RawMessage `json:"result"`
			IsError  bool            `json:"isError"`
		}
		if json.Unmarshal(line, &tool) != nil {
			return
		}
		if event.Type == "tool_execution_start" {
			conv.ToolCall(event.ToolCallID, tool.ToolName, adapter.ConversationJSON(tool.Args))
			return
		}
		conv.ToolResult(event.ToolCallID, tool.ToolName, resultText(tool.Result), tool.IsError)
	case "auto_retry_start":
		conv.Error(event.ErrorMessage)
	case "auto_retry_end":
		conv.Error(event.FinalError)
	}
}

// resultText is the text of a tool's result: the text blocks of its content, or the result as the JSON it is.
func resultText(raw json.RawMessage) string {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &result) == nil && len(result.Content) > 0 {
		var parts []string
		for _, block := range result.Content {
			if block.Type == "text" {
				parts = append(parts, block.Text)
			} else if block.Type != "" {
				parts = append(parts, "["+block.Type+"]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return adapter.ConversationJSON(raw)
}
