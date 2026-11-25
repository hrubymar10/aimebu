package answer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

var ErrInvalid = errors.New("invalid_answer")

const MaxAnswerBytes = 2 << 20

type Candidate struct {
	Messages       []Message       `json:"messages"`
	Reconciliation *Reconciliation `json:"reconciliation,omitempty"`
	Warnings       []string        `json:"-"`
}

type Message struct {
	Room            string            `json:"room"`
	Body            string            `json:"body"`
	ReplyTo         string            `json:"reply_to,omitempty"`
	Attachments     []string          `json:"attachments,omitempty"`
	Ask             *Ask              `json:"ask,omitempty"`
	NeedsAttention  bool              `json:"needs_attention,omitempty"`
	ProposedAnswers []string          `json:"proposed_answers,omitempty"`
	OpenQuestions   []OpenQuestion    `json:"open_questions,omitempty"`
	Handoff         bool              `json:"handoff,omitempty"`
	Outcome         string            `json:"outcome,omitempty"`
	Force           bool              `json:"force,omitempty"`
	VisualPlan      []json.RawMessage `json:"visual_plan,omitempty"`
	AppendixPages   []json.RawMessage `json:"appendix_pages,omitempty"`
}

// Outcomes a task-result handoff reports: done (the task is complete and verified), partial (some of it is done; the body names the
// rest) and failed (the task could not be done; the body says why).
const (
	OutcomeDone    = "done"
	OutcomePartial = "partial"
	OutcomeFailed  = "failed"
)

// OutcomeChoices names the allowed outcomes in an error an agent reads, without quotes (a held-reply note keeps only unquoted texts).
const OutcomeChoices = "done, partial or failed"

// ValidOutcome says whether value is one of the handoff outcomes.
func ValidOutcome(value string) bool {
	return value == OutcomeDone || value == OutcomePartial || value == OutcomeFailed
}

type OpenQuestion struct {
	Question    string   `json:"question"`
	Description string   `json:"description,omitempty"`
	Options     []string `json:"options"`
}

type Ask struct {
	Kind     string   `json:"kind"`
	Text     string   `json:"text"`
	Options  []string `json:"options,omitempty"`
	FreeText bool     `json:"free_text,omitempty"`
}

type Reconciliation struct {
	Status   string   `json:"status"`
	Findings []string `json:"findings"`
}

// Parse preserves the wire candidate elsewhere; this is only its strict,
// bounded view for server-side validation and commit.
func Parse(raw []byte, diagnostic bool) (Candidate, error) {
	var candidate Candidate
	if len(raw) > MaxAnswerBytes {
		return candidate, fmt.Errorf("%w: answer too large (max 2 MiB)", ErrInvalid)
	}
	if !utf8.Valid(raw) {
		return candidate, fmt.Errorf("%w: invalid UTF-8", ErrInvalid)
	}
	raw, reasoning := stripLeadingReasoning(raw)
	raw, unwrapped := unwrapJSONFence(raw)
	raw, shapeWarnings := tolerateAnswerShapes(raw)
	normalized, warnings := tolerateMessageMetadata(raw)
	warnings = append(shapeWarnings, warnings...)
	normalized = numbersAsMessageRefs(normalized)
	if unwrapped {
		warnings = append(warnings, "surrounding Markdown JSON fence removed")
	}
	if reasoning > 0 {
		warnings = append(warnings, fmt.Sprintf("%d leading reasoning block(s) removed", reasoning))
	}
	if problem, misshaped := answerShapeProblem(normalized, diagnostic); misshaped {
		return candidate, fmt.Errorf("%w: %s", ErrInvalid, problem)
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return candidate, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return candidate, fmt.Errorf("%w: the reply has text after the JSON object; send exactly one JSON object with a messages array", ErrInvalid)
	}
	candidate.Warnings = warnings
	if candidate.Messages == nil || len(candidate.Messages) > 10 {
		return candidate, fmt.Errorf("%w: messages must be an array of at most 10", ErrInvalid)
	}
	if diagnostic {
		if len(candidate.Messages) != 0 || candidate.Reconciliation == nil {
			return candidate, fmt.Errorf("%w: diagnostic output requires only reconciliation", ErrInvalid)
		}
		if candidate.Reconciliation.Status != "clean" && candidate.Reconciliation.Status != "uncertain" {
			return candidate, fmt.Errorf("%w: invalid reconciliation status", ErrInvalid)
		}
	} else if candidate.Reconciliation != nil {
		return candidate, fmt.Errorf("%w: reconciliation requires diagnostic mode", ErrInvalid)
	}
	for i, message := range candidate.Messages {
		room, body := strings.TrimSpace(message.Room), strings.TrimSpace(message.Body)
		switch {
		case room == "" && body == "":
			return candidate, fmt.Errorf("%w: message %d needs a room and a body", ErrInvalid, i)
		case room == "":
			return candidate, fmt.Errorf("%w: message %d needs a room", ErrInvalid, i)
		case body == "":
			return candidate, fmt.Errorf("%w: message %d needs a body", ErrInvalid, i)
		case len(message.Body) > 32<<10:
			return candidate, fmt.Errorf("%w: message %d: the body is longer than 32 KiB", ErrInvalid, i)
		}
		if message.Outcome != "" && !ValidOutcome(message.Outcome) {
			return candidate, fmt.Errorf("%w: message %d has an invalid outcome; use %s", ErrInvalid, i, OutcomeChoices)
		}
		if message.Outcome != "" && !message.Handoff {
			// Only a handoff reports an outcome: on any other message it means nothing, so it is dropped rather than costing the reply.
			candidate.Warnings = append(candidate.Warnings, fmt.Sprintf("message %d: outcome applies only to a handoff and was dropped", i))
			candidate.Messages[i].Outcome = ""
		}
		for _, attachment := range message.Attachments {
			if attachment == "" {
				return candidate, fmt.Errorf("%w: empty attachment ID", ErrInvalid)
			}
		}
		if message.Ask != nil {
			if message.NeedsAttention || len(message.ProposedAnswers) > 0 || len(message.OpenQuestions) > 0 {
				// An ask already puts the message in the person's inbox, so
				// the attention controls are redundant: drop them and warn
				// instead of rejecting the whole reply.
				candidate.Warnings = append(candidate.Warnings, fmt.Sprintf("message %d: ask implies attention; needs_attention, proposed_answers and open_questions were dropped", i))
				candidate.Messages[i].NeedsAttention = false
				candidate.Messages[i].ProposedAnswers = nil
				candidate.Messages[i].OpenQuestions = nil
				message = candidate.Messages[i]
			}
			if message.Ask.Kind != "question" && message.Ask.Kind != "approval" {
				return candidate, fmt.Errorf("%w: invalid ask kind", ErrInvalid)
			}
			if strings.TrimSpace(message.Ask.Text) == "" {
				return candidate, fmt.Errorf("%w: ask text required", ErrInvalid)
			}
		}
		if len(message.ProposedAnswers) > 4 || len(message.OpenQuestions) > 10 {
			return candidate, fmt.Errorf("%w: message %d has too many answer controls", ErrInvalid, i)
		}
		if len(message.ProposedAnswers) > 0 && len(message.OpenQuestions) > 0 {
			return candidate, fmt.Errorf("%w: message %d cannot combine proposed answers with open questions", ErrInvalid, i)
		}
		for _, option := range message.ProposedAnswers {
			if strings.TrimSpace(option) == "" || utf8.RuneCountInString(option) > 500 {
				return candidate, fmt.Errorf("%w: message %d has an invalid proposed answer", ErrInvalid, i)
			}
		}
		if len(message.ProposedAnswers) == 1 {
			return candidate, fmt.Errorf("%w: message %d needs at least two proposed answers", ErrInvalid, i)
		}
		for _, question := range message.OpenQuestions {
			if strings.TrimSpace(question.Question) == "" || utf8.RuneCountInString(question.Question) > 500 || utf8.RuneCountInString(question.Description) > 1000 || len(question.Options) < 2 || len(question.Options) > 8 {
				return candidate, fmt.Errorf("%w: message %d has an invalid open question", ErrInvalid, i)
			}
			for _, option := range question.Options {
				if strings.TrimSpace(option) == "" || utf8.RuneCountInString(option) > 500 {
					return candidate, fmt.Errorf("%w: message %d has an invalid question option", ErrInvalid, i)
				}
			}
		}
		if len(message.VisualPlan) > 80 || len(message.AppendixPages) > 10 {
			return candidate, fmt.Errorf("%w: message %d exceeds visual plan or appendix count", ErrInvalid, i)
		}
		for j, block := range message.VisualPlan {
			if len(block) > 64<<10 {
				return candidate, fmt.Errorf("%w: message %d visual_plan block %d exceeds 64 KiB", ErrInvalid, i, j)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(block, &fields) != nil || fields == nil {
				candidate.Warnings = append(candidate.Warnings, fmt.Sprintf("message %d visual_plan block %d has an unknown shape; raw content preserved", i, j))
				continue
			}
			var kind string
			_ = json.Unmarshal(fields["type"], &kind)
			if len(fields["data"]) > 64000 {
				return candidate, fmt.Errorf("%w: message %d visual_plan block %d data exceeds 64000 bytes", ErrInvalid, i, j)
			}
			var title, id string
			_ = json.Unmarshal(fields["title"], &title)
			_ = json.Unmarshal(fields["id"], &id)
			if utf8.RuneCountInString(title) > 160 {
				return candidate, fmt.Errorf("%w: message %d visual_plan block %d title exceeds 160 runes", ErrInvalid, i, j)
			}
			if id == "" {
				fields["id"], _ = json.Marshal(fmt.Sprintf("block-%d", j))
			}
			fields["order"], _ = json.Marshal(j)
			candidate.Messages[i].VisualPlan[j], _ = json.Marshal(fields)
			if !knownVisualPlanType(kind) || !validVisualPlanData(kind, fields["data"]) {
				candidate.Warnings = append(candidate.Warnings, fmt.Sprintf("message %d visual_plan block %d has an unknown shape; raw content preserved", i, j))
			}
		}
		total := 0
		for j, page := range message.AppendixPages {
			if len(page) > 128<<10 {
				return candidate, fmt.Errorf("%w: message %d appendix page %d JSON exceeds 128 KiB", ErrInvalid, i, j)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(page, &fields) != nil || fields == nil {
				candidate.Warnings = append(candidate.Warnings, fmt.Sprintf("message %d appendix page %d has an unknown shape; raw content preserved", i, j))
				continue
			}
			var body string
			var title string
			_ = json.Unmarshal(fields["title"], &title)
			if utf8.RuneCountInString(title) > 160 {
				return candidate, fmt.Errorf("%w: message %d appendix page %d title exceeds 160 runes", ErrInvalid, i, j)
			}
			if json.Unmarshal(fields["body"], &body) != nil || len(body) > 32000 {
				return candidate, fmt.Errorf("%w: message %d appendix page %d needs a body of at most 32000 bytes", ErrInvalid, i, j)
			}
			total += len(body)
		}
		if total > 128000 {
			return candidate, fmt.Errorf("%w: message %d appendix body exceeds 128000 bytes", ErrInvalid, i)
		}
	}
	return candidate, nil
}

// A model that reasons in its reply text (GLM and Qwen over an OpenAI-compatible provider, for one) puts the reasoning ahead of the
// answer as an element such as <think>…</think>. The answer is a JSON object, so it never starts with "<": whatever precedes it that is a
// complete <name>…</name> element is not part of it, whatever the name. Only leading elements go; text elsewhere, an element that never
// closes, and an element with attributes stay invalid, so the rest of the answer is still strictly decoded.
func stripLeadingReasoning(raw []byte) ([]byte, int) {
	removed := 0
	for {
		// Only the left side is trimmed: trimming the whole remainder each time would rescan a long trailing run of spaces once per block.
		trimmed := bytes.TrimLeftFunc(raw, unicode.IsSpace)
		if len(trimmed) < 3 || trimmed[0] != '<' {
			return raw, removed
		}
		end := bytes.IndexByte(trimmed, '>')
		if end < 2 {
			return raw, removed
		}
		name := trimmed[1:end]
		if !reasoningTag.Match(name) {
			return raw, removed
		}
		closing := []byte("</" + string(name) + ">")
		at := bytes.Index(trimmed[end+1:], closing)
		if at < 0 {
			return raw, removed
		}
		raw = trimmed[end+1+at+len(closing):]
		removed++
	}
}

var reasoningTag = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Accept one complete Markdown fence around the JSON document. Prose outside
// the fence, another fence, and arbitrary language tags remain invalid JSON.
func unwrapJSONFence(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	lineEnd := bytes.IndexByte(trimmed, '\n')
	if lineEnd < 0 {
		return raw, false
	}
	opening := bytes.TrimSpace(trimmed[:lineEnd])
	if !bytes.Equal(opening, []byte("```")) && !bytes.EqualFold(opening, []byte("```json")) {
		return raw, false
	}
	content := trimmed[lineEnd+1:]
	lastLine := bytes.LastIndexByte(content, '\n')
	if lastLine < 0 || !bytes.Equal(bytes.TrimSpace(content[lastLine+1:]), []byte("```")) {
		return raw, false
	}
	return bytes.TrimSpace(content[:lastLine]), true
}

// messageFields are the fields a message object may carry, including thread_root, which tolerateMessageMetadata drops later with its own
// warning. An object without any of them is not a message, so it keeps the envelope's own shape error.
var messageFields = map[string]bool{"room": true, "body": true, "reply_to": true, "attachments": true, "ask": true, "needs_attention": true,
	"proposed_answers": true, "open_questions": true, "handoff": true, "outcome": true, "force": true, "visual_plan": true,
	"appendix_pages": true, "thread_root": true}

// tolerateAnswerShapes reads three near-miss reply shapes as the envelope they mean, before the strict validation: a lone message object and a
// bare array of message objects become {"messages":[...]}, and prose around an otherwise empty {"messages":[]} counts as the empty answer
// itself, so it neither holds the reply nor raises a recovery. Everything else keeps the exact shape fault: text around a non-empty answer
// still holds, and so does an array holding something that is not a message object.
func tolerateAnswerShapes(raw []byte) ([]byte, []string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, nil
	}
	switch trimmed[0] {
	case '{':
		var fields map[string]json.RawMessage
		if json.Unmarshal(trimmed, &fields) != nil || fields == nil || len(fields["messages"]) != 0 || len(fields["reconciliation"]) != 0 {
			return raw, nil
		}
		if !looksLikeAMessageObject(fields) {
			return raw, nil
		}
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil {
			return raw, nil
		}
		for _, item := range items {
			var fields map[string]json.RawMessage
			if json.Unmarshal(item, &fields) != nil || fields == nil || !looksLikeAMessageObject(fields) {
				return raw, nil
			}
		}
	default:
		if !embeddedEmptyAnswer(trimmed) {
			return raw, nil
		}
		return []byte(`{"messages":[]}`), []string{"surrounding prose around an empty answer removed"}
	}
	// A lone object becomes a one-element array; a bare array is already the messages value.
	messagesRaw := trimmed
	if trimmed[0] == '{' {
		messagesRaw = append(append([]byte("["), trimmed...), ']')
	}
	envelope := map[string]json.RawMessage{"messages": messagesRaw}
	wrapped, err := json.Marshal(envelope)
	if err != nil {
		return raw, nil
	}
	return wrapped, nil
}

// looksLikeAMessageObject says whether an object carries at least one field a message may have.
func looksLikeAMessageObject(fields map[string]json.RawMessage) bool {
	for key := range fields {
		if messageFields[key] {
			return true
		}
	}
	return false
}

// embeddedEmptyAnswer looks for the first JSON object in the reply text and says whether it is an otherwise empty {"messages":[]}; the first
// well-formed object decides, so prose around a non-empty answer still holds. Each attempt decodes a bounded window from the object: an empty
// messages object is short, and an object that reaches past the window cannot be one.
func embeddedEmptyAnswer(raw []byte) bool {
	const window = 512
	for at, attempts := 0, 0; at < len(raw) && attempts < 256; attempts++ {
		next := bytes.IndexByte(raw[at:], '{')
		if next < 0 {
			return false
		}
		at += next
		end := at + window
		if end > len(raw) {
			end = len(raw)
		}
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(bytes.NewReader(raw[at:end])).Decode(&fields); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				// The object reaches past the window, so it cannot be the short empty envelope.
				return false
			}
			at++
			continue
		}
		return len(fields) == 1 && bytes.Equal(bytes.TrimSpace(fields["messages"]), []byte("[]"))
	}
	return false
}

// Unknown fields on a message are ignored only after the rest of the answer
// passes strict decoding and validation. Nested ask fields and top-level
// fields remain strict so a malformed inbox request cannot be silently lost.
func tolerateMessageMetadata(raw []byte) ([]byte, []string) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return raw, nil
	}
	var messages []json.RawMessage
	if json.Unmarshal(envelope["messages"], &messages) != nil || len(messages) > 10 {
		return raw, nil
	}
	known := map[string]bool{"room": true, "body": true, "reply_to": true, "attachments": true, "ask": true, "needs_attention": true, "proposed_answers": true, "open_questions": true, "handoff": true, "outcome": true, "force": true, "visual_plan": true, "appendix_pages": true}
	warnings := []string{}
	changed := false
	for i, encoded := range messages {
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &fields) != nil || fields == nil {
			return raw, nil
		}
		for _, key := range sortedFieldKeys(fields) {
			if known[key] {
				continue
			}
			// The conversation a message belongs to is derived from reply_to, so a thread_root (the old way to say it, still in a stale prompt or a turn
			// already under way) is dropped: silently when it is empty or null, with a note otherwise.
			if key == "thread_root" {
				value := strings.TrimSpace(string(fields[key]))
				delete(fields, key)
				changed = true
				if value != "" && value != "null" && value != `""` {
					warnings = append(warnings, fmt.Sprintf("message %d ignored thread_root: the conversation follows reply_to", i))
				}
				continue
			}
			// A model may label a structured question redundantly. If the
			// label is its only request for an inbox item, keep it invalid
			// rather than silently committing the body as ordinary text.
			if key == "type" && (string(fields[key]) == `"question"` || string(fields[key]) == `"approval"`) &&
				(len(fields["ask"]) == 0 || string(fields["ask"]) == "null") {
				continue
			}
			delete(fields, key)
			warnings = append(warnings, fmt.Sprintf("message %d ignored unknown field %q", i, key))
		}
		messages[i], _ = json.Marshal(fields)
	}
	if len(warnings) == 0 && !changed {
		return raw, nil
	}
	envelope["messages"], _ = json.Marshal(messages)
	normalized, err := json.Marshal(envelope)
	if err != nil {
		return raw, nil
	}
	return normalized, warnings
}

// answerShapeProblem classifies the reply-shape faults a strict JSON decoder would only report as raw errors that quote what the agent wrote
// (which a held-answer note may not repeat): text around the object, an unknown top-level field, a field of the wrong type, a reply_to that is
// neither a number, an id nor null, and a missing messages array. It reports false when no such cause is found and the decoder's own error
// stands.
func answerShapeProblem(raw []byte, diagnostic bool) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", false
	}
	switch trimmed[0] {
	case '{':
	case '[':
		return "the reply is a JSON array; send exactly one JSON object with a messages array", true
	default:
		return "the reply has text around the JSON object; send exactly one JSON object with a messages array", true
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		// The bytes may hold one good object followed by text: tell that apart from JSON that is broken outright.
		first := json.NewDecoder(bytes.NewReader(trimmed))
		first.Decode(&envelope)
		if len(envelope) > 0 {
			return "the reply has text after the JSON object; send exactly one JSON object with a messages array", true
		}
		return "the reply is not valid JSON; send exactly one JSON object with a messages array", true
	}
	for _, key := range sortedFieldKeys(envelope) {
		if key == "messages" || key == "reconciliation" {
			continue
		}
		return fmt.Sprintf("the reply object has an unknown field %s; the fields are messages and reconciliation", safeFieldName(key)), true
	}
	messagesRaw, hasMessages := envelope["messages"]
	if !hasMessages {
		// A diagnostic answer carries only a reconciliation, so the messages checks below do not apply to it.
		if diagnostic {
			return "", false
		}
		return "the reply needs a messages array", true
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(messagesRaw, &messages) != nil {
		return "the reply needs a messages array; every message is an object", true
	}
	for i, fields := range messages {
		for _, key := range sortedFieldKeys(fields) {
			value := bytes.TrimSpace(fields[key])
			if len(value) == 0 || bytes.Equal(value, []byte("null")) {
				continue
			}
			switch key {
			case "room", "body", "outcome":
				if value[0] != '"' {
					return fmt.Sprintf("message %d: %s must be text", i, key), true
				}
			case "needs_attention", "handoff", "force":
				if value[0] != 't' && value[0] != 'f' {
					return fmt.Sprintf("message %d: %s must be true or false", i, key), true
				}
			case "reply_to":
				if value[0] == '"' || plainPositiveInteger(value) {
					continue
				}
				return fmt.Sprintf("message %d: reply_to must be a message number, a msg_ id or null", i), true
			case "attachments", "proposed_answers", "open_questions", "visual_plan", "appendix_pages":
				if value[0] != '[' {
					return fmt.Sprintf("message %d: %s must be an array", i, key), true
				}
			case "ask":
				if value[0] != '{' {
					return fmt.Sprintf("message %d: ask must be an object", i), true
				}
				if problem, misshaped := askShapeProblem(i, value); misshaped {
					return problem, true
				}
			}
		}
	}
	return "", false
}

// askShapeProblem classifies a wrong field type inside an ask, the one nested object an answer may carry.
func askShapeProblem(i int, raw []byte) (string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return fmt.Sprintf("message %d: ask must be an object", i), true
	}
	for _, key := range sortedFieldKeys(fields) {
		value := bytes.TrimSpace(fields[key])
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			continue
		}
		switch key {
		case "kind", "text":
			if value[0] != '"' {
				return fmt.Sprintf("message %d: ask %s must be text", i, key), true
			}
		case "free_text":
			if value[0] != 't' && value[0] != 'f' {
				return fmt.Sprintf("message %d: ask free_text must be true or false", i), true
			}
		case "options":
			if value[0] != '[' {
				return fmt.Sprintf("message %d: ask options must be an array of texts", i), true
			}
		}
	}
	return "", false
}

func plainPositiveInteger(raw []byte) bool {
	if len(raw) == 0 || raw[0] < '1' || raw[0] > '9' {
		return false
	}
	for _, b := range raw {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// safeFieldName makes a JSON key safe to name in a room-readable note: only letters, digits and a few separators survive, so a crafted key
// cannot carry text or control characters into the note.
func safeFieldName(key string) string {
	name := make([]rune, 0, len(key))
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			name = append(name, r)
		default:
			name = append(name, '?')
		}
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return string(name)
}

func sortedFieldKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func knownVisualPlanType(kind string) bool {
	switch kind {
	case "markdown", "file-tree", "data-model", "api-endpoint", "annotated-code", "diff", "checklist", "question-form", "diagram", "canvas", "prototype":
		return true
	}
	return false
}

func validVisualPlanData(kind string, raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	if encoded, ok := value.(string); ok {
		if json.Unmarshal([]byte(encoded), &value) != nil {
			return false
		}
	}
	data, ok := value.(map[string]any)
	if !ok {
		return false
	}
	array := func(key string) ([]any, bool) { items, ok := data[key].([]any); return items, ok }
	switch kind {
	case "markdown":
		return visualTextFallback(data, "markdown", "text")
	case "diff":
		return visualTextFallback(data, "diff", "text")
	case "diagram":
		return visualTextFallback(data, "mermaid", "source", "text")
	case "file-tree":
		if root, present := data["root"]; present {
			return visualTreeEntry(root, 0)
		}
		for _, key := range []string{"files", "children"} {
			if entries, ok := array(key); ok {
				valid := true
				for _, entry := range entries {
					valid = valid && visualTreeEntry(entry, 0)
				}
				if valid {
					return true
				}
			}
		}
		return false
	case "data-model":
		entities := data["entities"]
		if entities == nil {
			entities = data["tables"]
		}
		list, ok := entities.([]any)
		if !ok {
			return false
		}
		for _, entity := range list {
			object, ok := entity.(map[string]any)
			if !ok || !visualTextFields(object, "name", "table") {
				return false
			}
			if fields, present := object["fields"]; present {
				items, ok := fields.([]any)
				if !ok {
					return false
				}
				for _, field := range items {
					if _, ok := field.(string); ok {
						continue
					}
					entry, ok := field.(map[string]any)
					if !ok || !visualTextFields(entry, "name", "type", "notes", "description") {
						return false
					}
				}
			}
		}
		return true
	case "api-endpoint":
		return visualTextFields(data, "method", "path", "url", "notes", "description")
	case "annotated-code":
		if !visualTextFields(data, "code", "source") {
			return false
		}
		if notes, present := data["annotations"]; present {
			items, ok := notes.([]any)
			if !ok {
				return false
			}
			for _, note := range items {
				if _, ok := note.(string); ok {
					continue
				}
				entry, ok := note.(map[string]any)
				if !ok || !visualTextFields(entry, "text", "note") {
					return false
				}
			}
		}
		return true
	case "checklist":
		items, ok := array("items")
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.(string); ok {
				continue
			}
			entry, ok := item.(map[string]any)
			if !ok || !visualTextFields(entry, "text", "label") {
				return false
			}
			if checked, present := entry["checked"]; present {
				if _, ok := checked.(bool); !ok {
					return false
				}
			}
		}
		return true
	case "question-form":
		questions, ok := array("questions")
		if !ok {
			return false
		}
		for _, question := range questions {
			entry, ok := question.(map[string]any)
			if !ok || !visualRequiredText(entry, "question") || !visualTextFields(entry, "description") {
				return false
			}
			if options, present := entry["options"]; present {
				items, ok := options.([]any)
				if !ok {
					return false
				}
				for _, option := range items {
					if _, ok := option.(string); !ok {
						return false
					}
				}
			}
		}
		return true
	case "canvas":
		nodes, ok := array("nodes")
		if !ok {
			return false
		}
		for _, node := range nodes {
			entry, ok := node.(map[string]any)
			if !ok || !visualPosition(entry) || !visualTextFields(entry, "label", "text", "id") {
				return false
			}
		}
		return true
	case "prototype":
		screens, ok := array("screens")
		if !ok {
			return false
		}
		for _, screen := range screens {
			entry, ok := screen.(map[string]any)
			if !ok || !visualTextFields(entry, "id", "title") {
				return false
			}
			if elements, present := entry["elements"]; present {
				items, ok := elements.([]any)
				if !ok {
					return false
				}
				for _, item := range items {
					element, ok := item.(map[string]any)
					if !ok || !visualPosition(element) || !visualTextFields(element, "id", "text", "label", "target", "type") {
						return false
					}
				}
			}
		}
		return true
	}
	return false
}

func visualRequiredText(data map[string]any, key string) bool { _, ok := data[key].(string); return ok }

func visualTextFields(data map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, present := data[key]; present {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	return true
}

func visualTextFallback(data map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value := data[key]; value != nil {
			_, ok := value.(string)
			return ok
		}
	}
	return false
}

func visualTreeEntry(value any, depth int) bool {
	if _, ok := value.(string); ok {
		return true
	}
	entry, ok := value.(map[string]any)
	if !ok || depth > 30 || !visualTextFields(entry, "name", "path", "type", "note", "status", "description") {
		return false
	}
	if children, present := entry["children"]; present {
		items, ok := children.([]any)
		if !ok {
			return false
		}
		for _, child := range items {
			if !visualTreeEntry(child, depth+1) {
				return false
			}
		}
	}
	return true
}

func visualPosition(data map[string]any) bool {
	for _, key := range []string{"x", "y", "w", "h"} {
		value, present := data[key]
		if !present {
			continue
		}
		if !visualFiniteNumber(value) {
			return false
		}
	}
	return true
}

// Browser positioning uses Number(value), including JSON scalars and arrays.
func visualFiniteNumber(value any) bool {
	switch item := value.(type) {
	case nil, bool, float64:
		return true
	case string:
		item = strings.TrimSpace(item)
		if item == "" {
			return true
		}
		if strings.HasPrefix(item, "0x") || strings.HasPrefix(item, "0X") || strings.HasPrefix(item, "0b") || strings.HasPrefix(item, "0B") || strings.HasPrefix(item, "0o") || strings.HasPrefix(item, "0O") {
			_, err := strconv.ParseUint(item, 0, 64)
			return err == nil
		}
		number, err := strconv.ParseFloat(item, 64)
		return err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
	case []any:
		if len(item) == 0 {
			return true
		}
		if len(item) != 1 {
			return false
		}
		switch child := item[0].(type) {
		case nil:
			return true
		case float64:
			return true
		case string:
			return visualFiniteNumber(child)
		case []any:
			return visualFiniteNumber(child)
		default:
			return false
		}
	default:
		return false
	}
}

// numbersAsMessageRefs lets a message name the message it replies to by its number written as a JSON number
// ("reply_to": 36226), as the prompt invites, by reading it as the string "36226"; the number is resolved to an id when the answer is
// committed. Anything that is not a plain positive integer is left for the strict decoder to refuse.
func numbersAsMessageRefs(raw []byte) []byte {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return raw
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(envelope["messages"], &messages) != nil {
		return raw
	}
	changed := false
	for _, fields := range messages {
		for _, key := range []string{"reply_to"} {
			value := bytes.TrimSpace(fields[key])
			if len(value) == 0 || value[0] < '1' || value[0] > '9' {
				continue
			}
			digits := true
			for _, b := range value {
				if b < '0' || b > '9' {
					digits = false
					break
				}
			}
			if digits {
				fields[key], _ = json.Marshal(string(value))
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	envelope["messages"], _ = json.Marshal(messages)
	out, err := json.Marshal(envelope)
	if err != nil {
		return raw
	}
	return out
}
