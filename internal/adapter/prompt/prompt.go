// Package prompt holds the turn instructions the Claude, Codex, and Pi
// adapters hand to their CLIs. The three prompts share most of their text, so
// the shared sentences live here once and each adapter supplies only what is
// specific to it through Deltas. Changing a shared sentence changes every
// adapter's prompt; the adapters' golden tests pin the exact bytes.
package prompt

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"
)

// Shared sentences, identical in every adapter's prompt.
const (
	identity        = `Use context.you_are for your identity and role, and context.team_roster for current room agent names, roles, harnesses, and models. If context.team_roster_more is present, more room agents were omitted from that list.`
	attachmentsLead = `Input note attachments are files;`
	retryNotice     = `A note retry_notice means the previous attempt was interrupted before it finished (for example by a provider limit or a lost session); check the state before repeating actions.`
	inboxAsk        = `Use either proposed_answers or open_questions, never both. For an Inbox ask, set needs_attention:false, proposed_answers:null, and open_questions:null in that same message; for example: {"room":"room_id","body":"Choose a team","reply_to":null,"thread_root":null,"attachments":null,"ask":{"kind":"question","text":"Which team?","options":["Team A"],"free_text":false},"handoff":false,"visual_plan":null,"appendix_pages":null,"needs_attention":false,"proposed_answers":null,"open_questions":null}. Never combine ask with human attention controls.`
	mentions        = `After spawning an agent, address its assignment with a live @slug or @agent_id mention in ordinary prose; a name inside backticks does not wake it. A reply_to another agent's message wakes that agent, but only a few times in a row: after four automatic replies with no human message between them, replies stop waking anyone, so use a live @mention or a handoff for anything that still needs an answer. Never reply just to acknowledge; use a reaction. Replies to humans still target them.`
	humanReply      = `A message from a human that is addressed to you, or posted room-wide in your room, always gets a reply, even a one-line one. Silence and reaction-only rules apply to agent-to-agent traffic and to your own unprompted progress reports.`
	tail            = `A lead with a completed review should publish the home-room summary before any further task-room status. Other free text is always available.`
)

// Sentences shared by some adapters.
const (
	// AttachmentGetTool tells an agent how to open an attachment by ID through
	// the aimebu toolbox.
	AttachmentGetTool = `call the aimebu attachment_get tool with an attachment ID when you need to inspect one.`
	// AckByReaction asks an agent to acknowledge with a reaction, not a message.
	AckByReaction = `Do not send acknowledgement-only or status-only messages in response to agent updates; use a reaction when available.`
	// ReturnOneObject opens the output contract for adapters that take the
	// reply as one JSON object in their final message.
	ReturnOneObject = `Return only one JSON object with a messages array. Each message needs room and body. Use reply_to, thread_root, attachments, ask, needs_attention, proposed_answers, open_questions, handoff, visual_plan, and appendix_pages only when needed. A delivered message carries its number as no; people write it as #no, and reply_to and thread_root accept that number or the message_id.`
	// OutsideRoomContext closes the prompt for adapters that read it from
	// standard input ahead of the turn JSON, so it ends with a newline.
	OutsideRoomContext = "Do not send messages outside the given room context.\n"
	humanResponse      = `For a human response, set needs_attention with proposed_answers (2-4 %s), open_questions (up to 10 questions with 2-8 choices each), or neither for free text.`
)

// HumanResponse is the sentence on answering a human. choices names the
// proposed-answer count, such as "choices" or "short choices".
func HumanResponse(choices string) string {
	return fmt.Sprintf(humanResponse, choices)
}

// Deltas are the parts of a prompt that differ between adapters. An empty
// Role leaves the role sentence out.
type Deltas struct {
	// Opening is the first sentence, which says where the turn JSON comes from.
	Opening string
	// Role tells the agent to obey context.role_body.
	Role string
	// Attachments follows "Input note attachments are files;".
	Attachments string
	// Output is the response contract, ending with the human-response sentence.
	Output string
	// Ack is the acknowledgement rule.
	Ack string
	// Closing is the final instruction.
	Closing string
}

// Slot is one prose sentence an owner may override. The output contract, the response schema and every adapter-specific part (the
// opening, the role pointer, the attachment tool, the acknowledgement rule, the closing) are not slots: they stay as shipped, because
// overriding them can break how a turn's answer is parsed.
type Slot struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Desc    string `json:"desc"`
	Default string `json:"default"`
}

// MaxOverrideLen caps one override, in characters.
const MaxOverrideLen = 2000

// Slots lists the overridable sentences in the order they appear in a prompt.
func Slots() []Slot {
	return []Slot{
		{"identity", "Identity and roster", "Tells the agent where its identity, role and team roster are in the turn context.", identity},
		{"retry_notice", "Retry notice", "What an interrupted-attempt note means.", retryNotice},
		{"inbox_ask", "Asking a human", "How to ask the human for an answer through the Inbox.", inboxAsk},
		{"mentions", "Mentions", "How to address another agent so it wakes.", mentions},
		{"human_reply", "Replying to people", "That a human's message always gets a reply.", humanReply},
		{"tail", "Closing guidance", "The last shared sentence before the adapter's own closing.", tail},
	}
}

// ValidSlot says whether key names an overridable sentence.
func ValidSlot(key string) bool {
	for _, slot := range Slots() {
		if slot.Key == key {
			return true
		}
	}
	return false
}

// Build joins the shared sentences and the adapter's deltas in the fixed
// order every adapter's prompt has always used.
func Build(d Deltas) string { return BuildWith(d, nil) }

// BuildWith is Build with some shared sentences replaced. An empty or unknown override is ignored, so a bad value can only fall back
// to the shipped text, never remove a sentence.
func BuildWith(d Deltas, overrides map[string]string) string {
	sentence := func(key, shipped string) string {
		if value := strings.TrimSpace(overrides[key]); value != "" {
			return value
		}
		return shipped
	}
	parts := []string{d.Opening, sentence("identity", identity)}
	if d.Role != "" {
		parts = append(parts, d.Role)
	}
	parts = append(parts, attachmentsLead+" "+d.Attachments, sentence("retry_notice", retryNotice), d.Output, sentence("inbox_ask", inboxAsk), sentence("mentions", mentions), d.Ack, sentence("human_reply", humanReply), sentence("tail", tail), d.Closing)
	return strings.Join(parts, " ")
}

// OverridesFromContext reads context.prompt_overrides from a turn's context JSON; anything unreadable gives none.
func OverridesFromContext(contextJSON []byte) map[string]string {
	var context struct {
		Overrides map[string]string `json:"prompt_overrides"`
	}
	if len(contextJSON) == 0 || json.Unmarshal(contextJSON, &context) != nil {
		return nil
	}
	return context.Overrides
}

// ForContext is the prompt for one turn: shipped when the context carries no overrides, otherwise built with them. shipped must be
// Build(d), computed once by the adapter, so the common case costs nothing and stays byte-identical.
func ForContext(d Deltas, shipped string, contextJSON []byte) string {
	if overrides := OverridesFromContext(contextJSON); len(overrides) > 0 {
		return BuildWith(d, overrides)
	}
	return shipped
}

// WithoutOverrides is the context JSON to forward to the agent: the overrides are already in the instruction text, so the copy under
// context.prompt_overrides would only repeat them. A context without the key (or unreadable) is returned untouched, byte for byte.
func WithoutOverrides(contextJSON []byte) []byte {
	var fields map[string]json.RawMessage
	if len(contextJSON) == 0 || json.Unmarshal(contextJSON, &fields) != nil {
		return contextJSON
	}
	if _, ok := fields["prompt_overrides"]; !ok {
		return contextJSON
	}
	delete(fields, "prompt_overrides")
	stripped, err := json.Marshal(fields)
	if err != nil {
		return contextJSON
	}
	return stripped
}
