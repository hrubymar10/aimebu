package supervisor

import (
	"fmt"
	"strings"
	"sync"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// turnConversation is the conversation log of one running turn.
type turnConversation struct {
	conv *adapter.Conversation
	once sync.Once // the turn's end is written once
}

func (t *turnConversation) end(text string) { t.once.Do(func() { t.conv.TurnEnd(text) }) }

// openConversation opens the agent's conversation log for a turn, writes the turn's start and what the agent was asked, and gives the
// spec the sink the adapter writes the rest to. It returns what closes the log: with the error when the harness never started, with nil
// once the turn has ended. A log that cannot be opened is reported and the turn runs without one, as with the output log.
func (c *Client) openConversation(delivery protocol.Delivery, spec *adapter.TurnSpec) func(startErr error) {
	if c.AgentLogs == nil {
		return func(error) {}
	}
	sink, err := c.AgentLogs.OpenConversation(delivery.AgentID)
	if err != nil {
		c.report("a turn conversation log could not be written", fmt.Errorf("agent %s turn %s: conversation log: %w", delivery.AgentID, delivery.TurnID, err))
		return func(error) {}
	}
	spec.ConversationSink = sink
	turn := &turnConversation{conv: adapter.NewConversation(sink)}
	c.conversations.Store(delivery.TurnID, turn)
	turn.conv.TurnStart(fmt.Sprintf("turn %s gen %d %s %s/%s %s", oneLine(delivery.TurnID), delivery.Generation, oneLine(string(delivery.Mode)),
		oneLine(spec.Agent.Tool), oneLine(spec.Agent.Variant), oneLine(spec.Agent.Model)))
	// The notes are what the agent was asked this turn. The rest of its input (the context: its role, the roster, the room's history) is
	// the same from turn to turn and is not repeated here.
	if notes := strings.TrimSpace(string(spec.NotesJSON)); notes != "" && notes != "[]" && notes != "null" {
		turn.conv.Prompt(notes)
	}
	return func(startErr error) {
		if startErr != nil {
			turn.conv.Error("the harness did not start: " + startErr.Error())
			turn.end("not started")
		}
		turn.end("ended with no exit recorded")
		c.conversations.Delete(delivery.TurnID)
		_ = sink.Close()
	}
}

// conversationTurnEnd writes how a turn ended to its conversation log, from the exit the supervisor is about to journal.
func (c *Client) conversationTurnEnd(turnID string, exit adapter.ExitInfo) {
	value, ok := c.conversations.Load(turnID)
	if !ok {
		return
	}
	turn := value.(*turnConversation)
	if !exit.TerminalEvent {
		switch {
		case exit.ErrorSummary != "":
			turn.conv.Error(exit.ErrorSummary)
		case exit.ErrorEvent != "":
			turn.conv.Error(exit.ErrorEvent)
		}
	}
	turn.end(turnEndText(exit))
}

// turnEndText says in a few words how a turn ended.
func turnEndText(exit adapter.ExitInfo) string {
	parts := []string{"answered"}
	if !exit.TerminalEvent {
		parts[0] = "no answer"
	}
	switch {
	case exit.LimitStall:
		parts = append(parts, "ended by the turn limit")
	case exit.Interrupted:
		parts = append(parts, "interrupted")
	case exit.CancelIntent:
		parts = append(parts, "cancelled")
	case exit.StartFailure:
		parts = append(parts, "not started")
	}
	if exit.ProcessAlive {
		parts = append(parts, "process kept warm")
	} else {
		parts = append(parts, fmt.Sprintf("exit %d", exit.ExitCode))
	}
	return strings.Join(parts, ", ")
}

func oneLine(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "-"
	}
	return value
}
