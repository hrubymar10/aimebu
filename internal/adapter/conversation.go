package adapter

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

// ConversationKind names what one record of an agent's conversation log is.
type ConversationKind string

const (
	ConversationTurnStart  ConversationKind = "turn_start"
	ConversationPrompt     ConversationKind = "prompt"
	ConversationAssistant  ConversationKind = "assistant"
	ConversationThinking   ConversationKind = "thinking"
	ConversationToolCall   ConversationKind = "tool_call"
	ConversationToolResult ConversationKind = "tool_result"
	ConversationError      ConversationKind = "error"
	ConversationTurnEnd    ConversationKind = "turn_end"
)

const (
	// ConversationTextLimit is what one record may hold of a prompt, of the model's own text or of an error.
	ConversationTextLimit = 64 << 10
	// ConversationToolLimit is what one record may hold of a tool call's input or of a tool result: a tool can print megabytes, and the
	// log is for reading what happened, not for keeping the output.
	ConversationToolLimit = 16 << 10
	// conversationSlack is how far past a limit the text is still searched for secrets, so one that straddles the cut is seen whole.
	conversationSlack = 4 << 10
	// conversationTools bounds the tool calls whose names are remembered for their results.
	conversationTools = 256
)

// ConversationSink receives the records of one turn's conversation, in order: what the agent was asked, what the model said, each tool
// call with its input and each tool result. The first line of text is the record's own line; the rest is its body. It is
// supervisor-owned and process-only like TurnSpec.StderrSink. A sink never fails the turn it is fed from.
type ConversationSink interface {
	Record(kind ConversationKind, text string)
}

// Conversation is how an adapter writes a turn's conversation: every text passes RedactSecrets and a size cap here, so no adapter can
// write a record that skipped them. One made from a spec without a sink records nothing, and its methods are safe on nil.
type Conversation struct {
	sink ConversationSink

	mu    sync.Mutex
	names map[string]string // tool call id to tool name, until its result
	last  string            // the last assistant text, so an event that repeats it is written once
}

// NewConversation wraps a sink; nil when there is none.
func NewConversation(sink ConversationSink) *Conversation {
	if sink == nil {
		return nil
	}
	return &Conversation{sink: sink, names: map[string]string{}}
}

// ConversationFor is the conversation writer of a turn (nil when the turn has no sink, or is a keep-alive ping, which is not work).
func ConversationFor(spec TurnSpec) *Conversation {
	if spec.KeepAlive {
		return nil
	}
	return NewConversation(spec.ConversationSink)
}

// Active says whether records go anywhere: an adapter may skip decoding an event it would only decode for the log.
func (c *Conversation) Active() bool { return c != nil }

// TurnStart and TurnEnd frame a turn; the supervisor writes them, around whatever the adapter records.
func (c *Conversation) TurnStart(text string) {
	c.record(ConversationTurnStart, text, ConversationTextLimit)
}
func (c *Conversation) TurnEnd(text string) {
	c.record(ConversationTurnEnd, text, ConversationTextLimit)
}

// Prompt is what the agent was asked this turn.
func (c *Conversation) Prompt(text string) { c.record(ConversationPrompt, text, ConversationTextLimit) }

// Assistant is text the model wrote. The same text twice in a row (a harness that reports a message at its end and again at the end of
// its turn) is written once.
func (c *Conversation) Assistant(text string) {
	if c == nil || strings.TrimSpace(text) == "" {
		return
	}
	c.mu.Lock()
	repeat := c.last == text
	c.last = text
	c.mu.Unlock()
	if !repeat {
		c.record(ConversationAssistant, text, ConversationTextLimit)
	}
}

// Thinking is reasoning the harness reported as text.
func (c *Conversation) Thinking(text string) {
	if strings.TrimSpace(text) != "" {
		c.record(ConversationThinking, text, ConversationTextLimit)
	}
}

// Error is a failure the harness reported in its stream.
func (c *Conversation) Error(text string) {
	if strings.TrimSpace(text) != "" {
		c.record(ConversationError, text, ConversationTextLimit)
	}
}

// ToolCall is a tool the model called, with its input (JSON or plain text). id pairs it with its result when the harness gives one.
func (c *Conversation) ToolCall(id, name, input string) {
	if c == nil {
		return
	}
	name = toolName(name)
	if id != "" {
		c.mu.Lock()
		if len(c.names) >= conversationTools {
			clear(c.names) // results that never came; a later one is written without its name
		}
		c.names[id] = name
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.last = ""
	c.mu.Unlock()
	text := name
	if input = strings.TrimSpace(input); input != "" && input != "null" && input != "{}" {
		text += " " + input
	}
	c.record(ConversationToolCall, text, ConversationToolLimit)
}

// ToolResult is what a tool returned. name may be empty when id names a call seen before.
func (c *Conversation) ToolResult(id, name, output string, failed bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if known, ok := c.names[id]; ok && id != "" {
		if name == "" {
			name = known
		}
		delete(c.names, id)
	}
	c.last = ""
	c.mu.Unlock()
	head := toolName(name)
	if failed {
		head += " (failed)"
	}
	// The cap is the output's own: the head line is never what is cut.
	c.sink.Record(ConversationToolResult, head+"\n"+CleanConversationText(output, ConversationToolLimit))
}

// Called says whether a tool call with this id was recorded and its result was not: an adapter whose harness reports some tools only when
// they have finished uses it to write the call it never saw start.
func (c *Conversation) Called(id string) bool {
	if c == nil || id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.names[id]
	return ok
}

func (c *Conversation) record(kind ConversationKind, text string, limit int) {
	if c == nil {
		return
	}
	c.sink.Record(kind, CleanConversationText(text, limit))
}

func toolName(name string) string {
	name = strings.Join(strings.Fields(name), "_")
	if name == "" {
		return "tool"
	}
	if len(name) > 128 {
		name = strings.ToValidUTF8(name[:128], "")
	}
	return name
}

// CleanConversationText is what may be written of a text: known secret shapes removed (RedactSecrets) and no more than limit bytes kept,
// cut at a character with a marker saying how much is gone. It is the minimum hygiene of a log only an owner or admin reads: a tool's
// output can hold anything, and a secret with an unknown shape is not recognised.
func CleanConversationText(text string, limit int) string {
	cut := 0
	if len(text) > limit+conversationSlack {
		// What lies far past the limit is dropped before the search: a tool result can be megabytes.
		kept := wholeCharacters(text[:limit+conversationSlack])
		cut, text = len(text)-len(kept), kept
	}
	text = RedactSecrets(text)
	if len(text) > limit {
		kept := wholeCharacters(text[:limit])
		cut, text = cut+len(text)-len(kept), kept
	}
	if cut == 0 {
		return text
	}
	return text + fmt.Sprintf("\n... %d bytes cut", cut)
}

// wholeCharacters drops the bytes of a character a cut went through from the end of text.
func wholeCharacters(text string) string {
	for len(text) > 0 {
		if r, size := utf8.DecodeLastRuneInString(text); r == utf8.RuneError && size <= 1 {
			text = text[:len(text)-1]
			continue
		}
		break
	}
	return text
}

// ConversationJSON renders a value of a harness's event (a tool's input, a structured result) as text for a record: a JSON string as its
// own text, anything else as compact JSON.
func ConversationJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}
