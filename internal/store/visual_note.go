package store

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

// deliveredNoteBody gives agents the readable substance of display-only blocks.
// The original message body remains intact and the added text has its own cap.
func deliveredNoteBody(body string, no int64, planJSON, pagesJSON string) string {
	if planJSON == "" && pagesJSON == "" {
		return body
	}
	const extraLimit = 16 << 10
	omitted := "\n[Additional visual content omitted]\n"
	if pagesJSON != "" {
		omitted = fmt.Sprintf("\n[Additional visual content omitted; message_get #%d returns all %d appendix page(s) in full]\n", no, appendixPageCount(pagesJSON))
	}
	var extra strings.Builder
	appendText := func(value string) bool {
		remaining := extraLimit - extra.Len()
		if remaining <= len(omitted) {
			return false
		}
		if len(value) <= remaining {
			extra.WriteString(value)
			return true
		}
		cut := remaining - len(omitted)
		if cut > 0 {
			value = value[:cut]
			for !utf8.ValidString(value) {
				value = value[:len(value)-1]
			}
			extra.WriteString(value)
		}
		extra.WriteString(omitted)
		return false
	}
	var blocks []json.RawMessage
	if planJSON != "" {
		if json.Unmarshal([]byte(planJSON), &blocks) != nil {
			appendText("\n[Visual plan]\n" + planJSON + "\n")
		} else {
			for _, raw := range blocks {
				var block struct {
					Type  string          `json:"type"`
					Title string          `json:"title"`
					Data  json.RawMessage `json:"data"`
				}
				if json.Unmarshal(raw, &block) != nil {
					if !appendText("\n[Visual block]\n" + string(raw) + "\n") {
						break
					}
					continue
				}
				heading := "\n[Visual block: " + block.Type
				if block.Title != "" {
					heading += " — " + block.Title
				}
				heading += "]\n"
				content := visualNoteData(block.Type, block.Data)
				if !appendText(heading + content + "\n") {
					break
				}
			}
		}
	}
	if pagesJSON != "" && extra.Len() < extraLimit-len(omitted) {
		var pages []json.RawMessage
		if json.Unmarshal([]byte(pagesJSON), &pages) != nil {
			appendText("\n[Appendix]\n" + pagesJSON + "\n")
		} else {
			for index, raw := range pages {
				var page struct {
					Title string `json:"title"`
					Body  string `json:"body"`
				}
				if json.Unmarshal(raw, &page) != nil {
					if !appendText("\n[Appendix page]\n" + string(raw) + "\n") {
						break
					}
					continue
				}
				if page.Title == "" {
					page.Title = fmt.Sprintf("Page %d", index+1)
				}
				if !appendText("\n[Appendix: " + page.Title + "]\n" + page.Body + "\n") {
					break
				}
			}
		}
	}
	return body + extra.String()
}

func visualNoteData(kind string, raw json.RawMessage) string {
	var data map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil {
		var encoded string
		if json.Unmarshal(raw, &encoded) == nil {
			if json.Unmarshal([]byte(encoded), &data) != nil {
				return encoded
			}
		}
	}
	if data != nil {
		keys := map[string][]string{
			"markdown": {"markdown", "text"},
			"diff":     {"diff", "text"},
			"diagram":  {"mermaid", "source", "text"},
		}[kind]
		for _, key := range keys {
			var value any
			if json.Unmarshal(data[key], &value) == nil {
				if text, ok := value.(string); ok {
					return text
				}
			}
		}
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		return pretty.String()
	}
	return string(raw)
}
