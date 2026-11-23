package codex

import (
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// transcribeItem writes one item of a Codex turn to the conversation log. `codex exec --json` and the app-server report the same items
// with differently spelled keys (command_execution and aggregated_output, commandExecution and aggregatedOutput), so both spellings are
// read. A tool item is a call when it starts and a result when it completes; one that is only ever reported complete is written as both.
func transcribeItem(conv *adapter.Conversation, raw json.RawMessage, completed bool) {
	if !conv.Active() {
		return
	}
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return
	}
	text := func(keys ...string) string {
		for _, key := range keys {
			if value := adapter.ConversationJSON(item[key]); value != "" {
				return value
			}
		}
		return ""
	}
	id := text("id")
	kind := strings.ToLower(strings.ReplaceAll(text("type"), "_", ""))
	status := text("status")
	switch kind {
	case "", "usermessage", "todolist":
		return
	case "agentmessage":
		if completed {
			conv.Assistant(text("text"))
		}
		return
	case "reasoning":
		if completed {
			conv.Thinking(joinedText(text("text", "summary", "content")))
		}
		return
	case "error":
		conv.Error(text("message"))
		return
	}
	name, input, output := text("type"), "", status
	failed := status == "failed" || status == "declined"
	switch kind {
	case "commandexecution":
		name, input, output = "shell", text("command"), text("aggregated_output", "aggregatedOutput")
		if code := text("exit_code", "exitCode"); code != "" && code != "0" {
			failed = true
			output = strings.TrimRight(output, "\n") + "\n(exit " + code + ")"
		}
	case "mcptoolcall":
		name, input = text("server")+"."+text("tool"), text("arguments")
		if problem := text("error"); problem != "" {
			failed, output = true, problem
		} else if result := text("result"); result != "" {
			output = result
		}
	case "filechange":
		name, input = "file_change", text("changes")
	case "websearch":
		name, input = "web_search", text("query")
	default:
		rest := map[string]json.RawMessage{}
		for key, value := range item {
			if key != "id" && key != "type" && key != "status" {
				rest[key] = value
			}
		}
		if encoded, err := json.Marshal(rest); err == nil {
			input = string(encoded)
		}
	}
	if !completed {
		conv.ToolCall(id, name, input)
		return
	}
	if !conv.Called(id) {
		conv.ToolCall(id, name, input)
	}
	conv.ToolResult(id, name, output, failed)
}

// joinedText is the text of a reasoning item: its text, or the strings of its summary list.
func joinedText(value string) string {
	var parts []string
	if strings.HasPrefix(value, "[") && json.Unmarshal([]byte(value), &parts) == nil {
		return strings.Join(parts, "\n")
	}
	if value == "[]" {
		return ""
	}
	return value
}

// transcribeFailure writes a turn.failed or error event: the message it carries, or the event itself when it has none.
func transcribeFailure(conv *adapter.Conversation, raw json.RawMessage) {
	if !conv.Active() {
		return
	}
	var value struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &value)
	switch {
	case value.Error.Message != "":
		conv.Error(value.Error.Message)
	case value.Message != "":
		conv.Error(value.Message)
	case string(raw) != "null":
		conv.Error(string(raw))
	}
}

// itemOf is the item of an item event.
func itemOf(raw json.RawMessage) json.RawMessage {
	var value struct {
		Item json.RawMessage `json:"item"`
	}
	_ = json.Unmarshal(raw, &value)
	return value.Item
}
