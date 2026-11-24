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
	inboxAsk        = `Use either proposed_answers or open_questions, never both. For an Inbox ask, set needs_attention:false, proposed_answers:null, and open_questions:null in that same message; for example: {"room":"room_id","body":"Choose a team","reply_to":null,"attachments":null,"ask":{"kind":"question","text":"Which team?","options":["Team A"],"free_text":false},"handoff":false,"outcome":null,"visual_plan":null,"appendix_pages":null,"needs_attention":false,"proposed_answers":null,"open_questions":null}. Never combine ask with human attention controls.`
	mentions        = `After spawning an agent, address its assignment with a live @slug or @agent_id mention in ordinary prose; a name inside backticks does not wake it. A mention only reaches members of the message's room; for a worker use their task room or dm_agent. A reply_to another agent's message wakes that agent, but only a few times in a row: after four automatic replies with no human message between them, replies stop waking anyone, so use a live @mention or a handoff for anything that still needs an answer. Never reply just to acknowledge; use a reaction. Replies to humans still target them. Always @mention whoever should act next: a message with no @mention and no reply_to is a broadcast that wakes every other agent in the room, so it should be rare. A note with fyi true was not addressed to you (another agent broadcast it): answer only if you have to act on it, otherwise end the turn with an empty messages array and write nothing. Mention agents and people by id: @agt_<agent id> and @usr_<user id>, the ids the notes and messages you receive carry (their entities table names who is who); a @slug also works while it is unambiguous, and the names you read are display only. Refer to rooms by id the same way: #room_<room id>, the id the room ids you are given carry (the entities table names rooms too, under the same may-see rule).`
	humanReply      = `Toward people: don't narrate. Message a person only for a decision, an answer, an approval, or a result they asked for; batch several questions into one numbered message; acknowledge with a reaction (👍 seen, ✅ done) instead of a text ack. When a person addresses you directly (mention, a reply to your message, a DM), always respond: answer if there is anything to say, otherwise react. Never leave them without any sign you saw it.`
	// owedWork keeps a turn from ending silently while work is still running. It is not an override slot: a harness job that outlives the
	// turn is lost work, whatever an owner's wording.
	owedWork = `Harness background jobs (a background shell command, a Monitor or another watcher) do not wake you when they finish and die with your process when the turn ends: run a long command with the aimebu run_command tool, which wakes you with its exit code. Never end a turn with an empty messages array while you still owe work or a report; a turn that was addressed to you and ends without a message is announced in the room and wakes whoever addressed you.`
	tail     = `A lead with a completed review should publish the home-room summary before any further task-room status. Other free text is always available.`
)

// Sentences shared by some adapters.
const (
	// AttachmentGetTool tells an agent how to open an attachment by ID through
	// the aimebu toolbox.
	AttachmentGetTool = `call the aimebu attachment_get tool with an attachment ID when you need to inspect one.`
	// AckByReaction asks an agent to acknowledge with a reaction, not a message.
	AckByReaction = `Do not send acknowledgement-only or status-only messages in response to agent updates; use a reaction when available.`
	// ReplyLinks says how an answer is linked to what it answers, and what a note carries about that. Every adapter's output contract has it.
	ReplyLinks = `When your message answers a delivered message, set reply_to to its no; a reply joins that message's conversation. A note's why says why it reached you; in_reply_to shows the message it replies to and conversation the message its conversation started with (yours: true when you wrote it).`
	// UnchangedContext explains the marker that stands in for the role body and knowledge on a wake whose harness session
	// already holds them: only the first turn of a session, a context reset or compaction, or a changed revision delivers
	// the full text again. Only the Claude prompt carries it — the marker is Claude-only, and the other adapters never
	// receive one.
	UnchangedContext = `When a context.role_body or knowledge value is {"unchanged":true,...}, that block is unchanged from an earlier turn of this harness session: its full text from that turn still applies.`
	// ReturnOneObject opens the output contract for adapters that take the
	// reply as one JSON object in their final message.
	ReturnOneObject = `Return only one JSON object with a messages array. Each message needs room and body. ` + ReplyLinks + ` Use attachments, ask, needs_attention, proposed_answers, open_questions, handoff, visual_plan, and appendix_pages only when needed. Set force:true on one message only when you intend to publish it despite a live mention of an agent outside that room. A delivered message carries its number as no; people write it as #no, and reply_to accepts that number or the message_id. A #no written in a message you are given is a message number: the ones that name a message of your project are in your context as referenced_messages, and the message_get tool reads any other message of your project by its number.`
	// OutsideRoomContext closes the prompt for adapters that read it from
	// standard input ahead of the turn JSON, so it ends with a newline.
	OutsideRoomContext = "Do not send messages outside the given room context.\n"
	humanResponse      = `For a human response, set needs_attention with proposed_answers (2-4 %s), open_questions (up to 10 questions with 2-8 choices each), or neither for free text.`
)

// HandoffOutcome is the rule for a handoff's outcome, in every adapter's prompt right after its output contract. It belongs to that contract, so it
// is not an overridable slot: a supervisor that reports supports_handoff_outcome makes the server reject a handoff without one.
const HandoffOutcome = `Every message with handoff true needs outcome: done when the task is complete and verified, partial when some of it is done and the body names what remains, failed when the task could not be done and the body says why. A handoff without an outcome is rejected; leave outcome null on any other message.`

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
		{"mentions", "Mentions", "How to address another agent so it wakes, and what a broadcast (a message with no mention) is.", mentions},
		{"human_reply", "Toward people", "When to message a person, and that someone who addresses the agent directly always gets an answer or a reaction.", humanReply},
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
	parts = append(parts, attachmentsLead+" "+d.Attachments, sentence("retry_notice", retryNotice), d.Output, HandoffOutcome, sentence("inbox_ask", inboxAsk), sentence("mentions", mentions), d.Ack, owedWork, sentence("human_reply", humanReply), sentence("tail", tail), d.Closing)
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
