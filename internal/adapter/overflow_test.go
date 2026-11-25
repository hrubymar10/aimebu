package adapter

import "testing"

// Recorded (and shortened) error lines of each harness when the conversation no longer fits the model's context window.
func TestContextOverflowRecognisesEachHarness(t *testing.T) {
	for name, line := range map[string]string{
		"claude":            `{"type":"result","subtype":"success","is_error":true,"result":"Prompt is too long"}`,
		"codex":             `{"type":"error","message":"Codex ran out of room in the model's context window. Start a new thread or clear earlier history before retrying."}`,
		"codex2":            `{"type":"turn.failed","error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}`,
		"opencode":          `{"name":"ContextOverflowError","data":{"message":"Input exceeds context window of this model"}}`,
		"litellm":           `litellm.ContextWindowExceededError: ContextWindowExceededError: AnthropicException - prompt is too long: 213456 tokens > 200000 maximum`,
		"opencode captured": `OpenCode error: ContextOverflowError: litellm.BadRequestError: OpenAIException - Requested token count exceeds the model's maximum context length of 1048576 tokens. You requested a total of 1051866 tokens: 1019866 tokens from the input messages and 32000 tokens for the completion. Please reduce the number of tokens in the input messages or the completion to fit within the limit.`,
		"api":               `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 128000 tokens."}}`,
	} {
		if !ContextOverflow("", line) {
			t.Errorf("%s: %q not classified as context overflow", name, line)
		}
	}
}

func TestContextOverflowLeavesOtherErrorsAlone(t *testing.T) {
	for _, line := range []string{
		"", "Failed to authenticate: OAuth session expired · Please run /login", "Overloaded: try again later",
		"exit status 1", "the command line is too long", "context deadline exceeded", "rate limit reached, context unchanged",
		"Process exited with code 137.",
	} {
		if ContextOverflow(line) {
			t.Errorf("%q wrongly classified as context overflow", line)
		}
	}
}
