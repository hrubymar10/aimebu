package answer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
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
	ThreadRoot      string            `json:"thread_root,omitempty"`
	Attachments     []string          `json:"attachments,omitempty"`
	Ask             *Ask              `json:"ask,omitempty"`
	NeedsAttention  bool              `json:"needs_attention,omitempty"`
	ProposedAnswers []string          `json:"proposed_answers,omitempty"`
	OpenQuestions   []OpenQuestion    `json:"open_questions,omitempty"`
	Handoff         bool              `json:"handoff,omitempty"`
	VisualPlan      []json.RawMessage `json:"visual_plan,omitempty"`
	AppendixPages   []json.RawMessage `json:"appendix_pages,omitempty"`
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
	raw, unwrapped := unwrapJSONFence(raw)
	normalized, warnings := tolerateMessageMetadata(raw)
	if unwrapped {
		warnings = append(warnings, "surrounding Markdown JSON fence removed")
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return candidate, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return candidate, fmt.Errorf("%w: trailing data", ErrInvalid)
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
		if strings.TrimSpace(message.Room) == "" || strings.TrimSpace(message.Body) == "" || len(message.Body) > 32<<10 {
			return candidate, fmt.Errorf("%w: message %d needs a room and body of at most 32 KiB", ErrInvalid, i)
		}
		for _, attachment := range message.Attachments {
			if attachment == "" {
				return candidate, fmt.Errorf("%w: empty attachment ID", ErrInvalid)
			}
		}
		if message.Ask != nil {
			if message.NeedsAttention || len(message.ProposedAnswers) > 0 || len(message.OpenQuestions) > 0 {
				return candidate, fmt.Errorf("%w: message %d cannot combine ask with attention controls", ErrInvalid, i)
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
	known := map[string]bool{"room": true, "body": true, "reply_to": true, "thread_root": true, "attachments": true, "ask": true, "needs_attention": true, "proposed_answers": true, "open_questions": true, "handoff": true, "visual_plan": true, "appendix_pages": true}
	warnings := []string{}
	for i, encoded := range messages {
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &fields) != nil || fields == nil {
			return raw, nil
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if known[key] {
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
	if len(warnings) == 0 {
		return raw, nil
	}
	envelope["messages"], _ = json.Marshal(messages)
	normalized, err := json.Marshal(envelope)
	if err != nil {
		return raw, nil
	}
	return normalized, warnings
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
