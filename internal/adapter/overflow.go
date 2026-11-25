package adapter

import "regexp"

// contextOverflowPattern matches what the harnesses and providers say when a conversation no longer fits the model's context window:
// OpenCode's ContextOverflowError, LiteLLM's ContextWindowExceededError, the API's "context_length_exceeded" code, Claude's "Prompt is
// too long", and Codex's and providers' "ran out of room in the model's context window", "exceeds the context window" and "maximum
// context length". It is deliberately narrow: a bare "context" or "too long" (a timeout, a long command line) must not match.
var contextOverflowPattern = regexp.MustCompile(`(?i)ContextOverflowError|ContextWindowExceeded(?:Error)?|context[_ ]length[_ ]exceeded|prompt is too long|` +
	`ran out of room in the model.?s context window|exceeds? the (?:model.?s )?context (?:window|length|limit)|maximum context length|` +
	`context (?:window|length|limit) (?:was |is )?(?:exceeded|reached)`)

// ContextOverflow says whether a harness's error text (its error result, error event or stderr) reports that the conversation outgrew the
// model's context window. Such a session cannot be resumed or retried: it fails the same way at once, so recovery offers a fresh session.
func ContextOverflow(texts ...string) bool {
	for _, text := range texts {
		if contextOverflowPattern.MatchString(text) {
			return true
		}
	}
	return false
}
