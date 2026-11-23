package opencode

import (
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// transcribeTool writes a tool_use event to the conversation log. OpenCode reports a tool part once, when it has finished (completed or
// failed), with its input and its output, so one event is both the call and its result.
func transcribeTool(conv *adapter.Conversation, line []byte) {
	if !conv.Active() {
		return
	}
	var event struct {
		Part struct {
			Tool   string `json:"tool"`
			CallID string `json:"callID"`
			State  struct {
				Status string          `json:"status"`
				Input  json.RawMessage `json:"input"`
				Output json.RawMessage `json:"output"`
				Error  json.RawMessage `json:"error"`
			} `json:"state"`
		} `json:"part"`
	}
	if json.Unmarshal(line, &event) != nil || event.Part.Tool == "" && event.Part.CallID == "" {
		conv.Unreadable("tool_use")
		return
	}
	state := event.Part.State
	conv.ToolCall(event.Part.CallID, event.Part.Tool, adapter.ConversationJSON(state.Input))
	failed := state.Status == "error"
	output := adapter.ConversationJSON(state.Output)
	if problem := adapter.ConversationJSON(state.Error); problem != "" {
		failed, output = true, problem
	}
	conv.ToolResult(event.Part.CallID, event.Part.Tool, output, failed)
}
