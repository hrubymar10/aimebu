package answer

import (
	"regexp"
	"strings"
)

var parseField = regexp.MustCompile(`(?:unknown field "([a-z_]+)"|\.([a-z_]+) of type)`)

var envelopeFields = map[string]bool{
	"messages": true, "room": true, "body": true, "reply_to": true,
	"attachments": true, "ask": true, "kind": true, "text": true,
	"options": true, "free_text": true, "handoff": true, "outcome": true,
	"force": true, "visual_plan": true, "appendix_pages": true,
	"needs_attention": true, "proposed_answers": true, "open_questions": true,
	"question": true, "description": true, "title": true, "data": true,
	"order": true, "id": true,
}

// SafeError describes a parse error without repeating any answer content.
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimPrefix(err.Error(), "invalid_answer: ")
	if match := parseField.FindStringSubmatch(message); match != nil {
		field := match[1]
		if field == "" {
			field = match[2]
		}
		if envelopeFields[field] {
			return "invalid field " + field
		}
		return "unknown field in answer"
	}
	if strings.ContainsAny(message, `"'`) || strings.Contains(message, "json:") {
		return "answer is not one valid JSON object with a messages array"
	}
	return message
}
