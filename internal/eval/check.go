package eval

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

// Ops lists the check predicates. Each reads the parsed answer (and, for the tool ops, the names of the tools the model called).
var checkOps = map[string]bool{
	"messages_count": true, "no_messages": true, "room_is": true, "reply_to_is": true, "handoff_outcome": true, "no_handoff": true,
	"ask_kind": true, "ask_options": true, "no_attention_controls": true, "proposed_answers": true, "open_questions": true,
	"body_contains": true, "body_not_contains": true, "mentions_id": true, "tool_called": true, "tool_not_called": true,
	"needs_attention": true, "react_if_silent": true,
}

func (c Check) validate() error {
	if !checkOps[c.Op] {
		return fmt.Errorf("unknown check op %q", c.Op)
	}
	need := map[string]string{"room_is": "value", "reply_to_is": "values", "handoff_outcome": "value", "ask_kind": "value", "body_contains": "value",
		"body_not_contains": "value", "mentions_id": "value", "tool_called": "value", "tool_not_called": "value"}[c.Op]
	if need == "value" && c.Value == "" || need == "values" && len(c.Values) == 0 {
		return fmt.Errorf("check %s needs %s", c.Op, need)
	}
	if c.Op == "handoff_outcome" && !answer.ValidOutcome(c.Value) {
		return fmt.Errorf("check handoff_outcome: %q is not an outcome", c.Value)
	}
	if c.Op == "ask_kind" && c.Value != "question" && c.Value != "approval" {
		return fmt.Errorf("check ask_kind: %q is not a kind", c.Value)
	}
	switch c.Op {
	case "messages_count", "ask_options", "proposed_answers", "open_questions":
		if c.Max < c.Min || c.Max < 1 && c.Op != "messages_count" {
			return fmt.Errorf("check %s: bad range %d..%d", c.Op, c.Min, c.Max)
		}
	}
	return nil
}

var codeSpan = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")

// run says what is wrong, or "".
func (c Check) run(candidate answer.Candidate, tools []string) string {
	value := Expand(c.Value)
	count := len(candidate.Messages)
	switch c.Op {
	case "messages_count":
		if count < c.Min || count > c.Max {
			return fmt.Sprintf("messages_count: %d messages, want %d..%d", count, c.Min, c.Max)
		}
	case "no_messages":
		if count != 0 {
			return fmt.Sprintf("no_messages: answered with %d message(s), silence was right", count)
		}
	case "room_is":
		for i, m := range candidate.Messages {
			if m.Room != value {
				return fmt.Sprintf("room_is: message %d is in %s, want %s", i, m.Room, value)
			}
		}
	case "reply_to_is":
		if count == 0 {
			return "reply_to_is: no message"
		}
		for i, m := range candidate.Messages {
			ok := false
			for _, want := range c.Values {
				ok = ok || m.ReplyTo == Expand(want)
			}
			if !ok {
				return fmt.Sprintf("reply_to_is: message %d replies to %q, want one of %v", i, m.ReplyTo, c.Values)
			}
		}
	case "handoff_outcome":
		for _, m := range candidate.Messages {
			if m.Handoff && m.Outcome == value {
				return ""
			}
		}
		return fmt.Sprintf("handoff_outcome: no handoff with outcome %s", value)
	case "no_handoff":
		for i, m := range candidate.Messages {
			if m.Handoff {
				return fmt.Sprintf("no_handoff: message %d is a handoff", i)
			}
		}
	case "ask_kind":
		for _, m := range candidate.Messages {
			if m.Ask != nil && m.Ask.Kind == value {
				return ""
			}
		}
		return "ask_kind: no ask of kind " + value
	case "ask_options":
		for _, m := range candidate.Messages {
			if m.Ask != nil {
				if n := len(m.Ask.Options); n < c.Min || n > c.Max {
					return fmt.Sprintf("ask_options: %d options, want %d..%d", n, c.Min, c.Max)
				}
				return ""
			}
		}
		return "ask_options: no ask"
	case "no_attention_controls":
		// The parser drops these next to an ask with a warning, so by now they are gone; the strict check reads the warning.
		for _, w := range candidate.Warnings {
			if strings.Contains(w, "ask implies attention") {
				return "no_attention_controls: " + w
			}
		}
	case "proposed_answers":
		for _, m := range candidate.Messages {
			if n := len(m.ProposedAnswers); n >= c.Min && n <= c.Max && n > 0 && len(m.OpenQuestions) == 0 {
				return ""
			}
		}
		return fmt.Sprintf("proposed_answers: no message with %d..%d proposed answers", c.Min, c.Max)
	case "open_questions":
		for _, m := range candidate.Messages {
			if n := len(m.OpenQuestions); n >= c.Min && n <= c.Max && n > 0 && len(m.ProposedAnswers) == 0 {
				return ""
			}
		}
		return fmt.Sprintf("open_questions: no message with %d..%d open questions", c.Min, c.Max)
	case "needs_attention":
		for _, m := range candidate.Messages {
			if m.NeedsAttention {
				return ""
			}
		}
		return "needs_attention: no message asks for a human response"
	case "react_if_silent":
		if count == 0 && !contains(tools, "react") {
			return "react_if_silent: no message and no reaction, the person got no sign"
		}
	case "body_contains":
		for _, m := range candidate.Messages {
			if strings.Contains(m.Body, value) {
				return ""
			}
		}
		return fmt.Sprintf("body_contains: no body holds %q", value)
	case "body_not_contains":
		for i, m := range candidate.Messages {
			if strings.Contains(m.Body, value) {
				return fmt.Sprintf("body_not_contains: message %d holds %q", i, value)
			}
		}
	case "mentions_id":
		// A mention in code formatting does not wake anyone, so only text outside code counts.
		for _, m := range candidate.Messages {
			if strings.Contains(codeSpan.ReplaceAllString(m.Body, ""), "@"+value) {
				return ""
			}
		}
		return fmt.Sprintf("mentions_id: no live mention of %s", value)
	case "tool_called":
		if contains(tools, value) {
			return ""
		}
		return "tool_called: " + value + " was not called"
	case "tool_not_called":
		if contains(tools, value) {
			return "tool_not_called: " + value + " was called"
		}
	}
	return ""
}

var agentMention = regexp.MustCompile(`@(agt_[A-Za-z0-9]+)`)

// universal are the checks every answer to a scenario with an input must pass, the cheap version of what the server does at commit: every
// room is one of the context's, every reply_to names a delivered note, every live @agt_ mention names a roster agent.
func universal(in *Input, candidate answer.Candidate) []string {
	if in == nil {
		return nil
	}
	notes, context, err := in.Build()
	if err != nil {
		return []string{"universal: " + err.Error()}
	}
	var ctx struct {
		Rooms []struct {
			RoomID string `json:"room_id"`
		} `json:"rooms"`
		Roster []struct {
			ID string `json:"id"`
		} `json:"team_roster"`
	}
	json.Unmarshal(context, &ctx)
	rooms, agents, refs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range ctx.Rooms {
		rooms[r.RoomID] = true
	}
	for _, a := range ctx.Roster {
		agents[a.ID] = true
	}
	var list []struct {
		No        json.Number `json:"no"`
		MessageID string      `json:"message_id"`
	}
	json.Unmarshal(notes, &list)
	for _, n := range list {
		refs[n.No.String()], refs["#"+n.No.String()], refs[n.MessageID] = true, true, true
	}
	var problems []string
	for i, m := range candidate.Messages {
		if !rooms[m.Room] {
			problems = append(problems, fmt.Sprintf("universal: message %d room %q is not one of the context's rooms", i, m.Room))
		}
		if m.ReplyTo != "" && !refs[m.ReplyTo] {
			problems = append(problems, fmt.Sprintf("universal: message %d reply_to %q names no delivered note", i, m.ReplyTo))
		}
		for _, found := range agentMention.FindAllStringSubmatch(codeSpan.ReplaceAllString(m.Body, ""), -1) {
			if !agents[found[1]] {
				problems = append(problems, fmt.Sprintf("universal: message %d mentions %s, who is not in the roster", i, found[1]))
			}
		}
	}
	return problems
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
