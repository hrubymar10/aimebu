package claude

import (
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/harness"
)

// transcribe writes what one stream-json event says to the turn's conversation log: the model's text and thinking, each tool call with
// its input, each tool result, and an error result. The CLI's repeat of this turn's own input (a live process only) is not written: the
// supervisor has already recorded what the agent was asked.
func (t *turn) transcribe(event harness.ClaudeEvent) {
	if !t.conv.Active() {
		return
	}
	switch event.Type {
	case "assistant", "user":
		var value struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(event.Raw, &value) != nil {
			return
		}
		var text string
		if json.Unmarshal(value.Message.Content, &text) == nil {
			t.transcribeText(event.Type, text)
			return
		}
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(value.Message.Content, &blocks) != nil {
			return
		}
		for _, block := range blocks {
			switch block.Type {
			case "text":
				t.transcribeText(event.Type, block.Text)
			case "thinking":
				t.conv.Thinking(block.Thinking)
			case "tool_use", "server_tool_use":
				t.conv.ToolCall(block.ID, block.Name, adapter.ConversationJSON(block.Input))
			case "tool_result":
				t.conv.ToolResult(block.ToolUseID, "", blockText(block.Content), block.IsError)
			}
		}
	case "result":
		var result struct {
			Result  *string `json:"result"`
			IsError bool    `json:"is_error"`
			Subtype string  `json:"subtype"`
		}
		if json.Unmarshal(event.Raw, &result) != nil || !result.IsError {
			return
		}
		text := result.Subtype
		if result.Result != nil && *result.Result != "" {
			text = *result.Result
		}
		t.conv.Error(text)
	}
}

func (t *turn) transcribeText(role, text string) {
	switch {
	case role == "assistant":
		t.conv.Assistant(text)
	case t.echoMarker != "" && strings.HasPrefix(text, t.echoMarker):
		// the CLI's repeat of this turn's input
	default:
		// Input the CLI gave the model on its own (the end of a background task).
		t.conv.Prompt(text)
	}
}

// blockText is the text of a tool result's content: a string, or its text blocks joined; anything else as the JSON it is.
func blockText(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, block := range blocks {
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
