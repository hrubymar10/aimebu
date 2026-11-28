package server

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

const handbookAnswerCap = 16 << 10

var handbookOrder = []string{"current_state", "open_backlog", "conventions", "gotchas", "timeline"}
var handbookStamp = regexp.MustCompile(`(?m)\bUpdated\s+\d{4}-\d{2}-\d{2}\b`)

type handbookLine struct {
	Section string `json:"section"`
	Line    int    `json:"line"`
	Text    string `json:"text"`
}

type handbookSectionSummary struct {
	Section   string `json:"section"`
	Name      string `json:"name"`
	LineCount int    `json:"line_count"`
	Updated   string `json:"updated,omitempty"`
}

type handbookView struct {
	Rev        int64                    `json:"rev"`
	Sections   []handbookSectionSummary `json:"sections"`
	Lines      []handbookLine           `json:"lines"`
	TotalLines int                      `json:"total_lines"`
	HasMore    bool                     `json:"has_more"`
	NextOffset *int                     `json:"next_offset"`
	Continue   string                   `json:"continue,omitempty"`
}

func handbookContentLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
}

func handbookReadView(doc store.Handbook, limited bool, query url.Values) (handbookView, string) {
	section := query.Get("section")
	if section != "" && !handbookSectionAllowed(section, limited) {
		return handbookView{}, "invalid section"
	}
	offset, limit := 1, 50
	if raw := query.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return handbookView{}, "offset must be positive"
		}
		offset = n
	}
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return handbookView{}, "limit must be positive"
		}
		limit = n
	}
	grep := query.Get("grep")
	out := handbookView{Rev: doc.Rev, Sections: []handbookSectionSummary{}, Lines: []handbookLine{}}
	var all []handbookLine
	for _, key := range handbookOrder {
		if !handbookSectionAllowed(key, limited) {
			continue
		}
		lines := handbookContentLines(doc.Sections[key])
		stamp := handbookStamp.FindString(doc.Sections[key])
		name := strings.ReplaceAll(key, "_", " ")
		name = strings.ToUpper(name[:1]) + name[1:]
		out.Sections = append(out.Sections, handbookSectionSummary{Section: key, Name: name, LineCount: len(lines), Updated: stamp})
		if section != "" && section != key {
			continue
		}
		for i, line := range lines {
			all = append(all, handbookLine{Section: key, Line: i + 1, Text: line})
		}
	}
	out.TotalLines = len(all)
	if grep != "" {
		matched := make([]bool, len(all))
		for i, line := range all {
			if strings.Contains(strings.ToLower(line.Text), strings.ToLower(grep)) {
				matched[i] = true
				if i > 0 && all[i-1].Section == line.Section {
					matched[i-1] = true
				}
				if i+1 < len(all) && all[i+1].Section == line.Section {
					matched[i+1] = true
				}
			}
		}
		selected := make([]handbookLine, 0)
		for i, line := range all {
			if matched[i] {
				selected = append(selected, line)
			}
		}
		all = selected
		out.TotalLines = len(all)
	}
	// An index previews up to 50 lines in each section. A section or grep
	// request uses one continuous, 1-based result stream.
	index := section == "" && grep == "" && query.Get("limit") == "" && query.Get("offset") == ""
	if index {
		preview := make([]handbookLine, 0)
		for _, line := range all {
			if line.Line <= 50 {
				preview = append(preview, line)
			}
		}
		all = preview
	} else if offset > len(all) {
		return out, ""
	} else {
		all = all[offset-1:]
		if len(all) > limit {
			all = all[:limit]
			out.HasMore = true
		}
	}
	for i, line := range all {
		out.Lines = append(out.Lines, line)
		// Leave room for the continuation fields when the next line is too large.
		probe := out
		probe.HasMore = true
		next := offset + i
		probe.NextOffset = &next
		probe.Continue = strings.Repeat("x", 160)
		encoded, _ := json.Marshal(probe)
		if len(encoded) >= handbookAnswerCap {
			out.Lines = out.Lines[:len(out.Lines)-1]
			out.HasMore = true
			if index {
				next = line.Line
			}
			continuation := "Call handbook_get with the same filters and next_offset to continue."
			if index || (grep == "" && section != "") {
				continuation = "Call handbook_get with section=" + line.Section + " and offset=" + strconv.Itoa(next) + " to continue."
			}
			if len(out.Lines) == 0 {
				next++
				if index || (grep == "" && section != "") {
					continuation = "Call handbook_get with section=" + line.Section + " and offset=" + strconv.Itoa(next) + " to continue."
				}
				out.Continue = "One line exceeds the answer cap; continue after it. " + continuation
			} else {
				out.Continue = continuation
			}
			out.NextOffset = &next
			return out, ""
		}
	}
	if index {
		for _, summary := range out.Sections {
			if summary.LineCount > 50 {
				out.HasMore = true
				break
			}
		}
		if out.HasMore {
			next := 51
			out.NextOffset = &next
			out.Continue = "Call handbook_get with section and offset=51 to continue after a preview."
		}
	} else if out.HasMore {
		next := offset + len(out.Lines)
		out.NextOffset = &next
		out.Continue = "Call handbook_get with the same filters and next_offset to continue."
	}
	return out, ""
}

func handbookSectionAllowed(key string, limited bool) bool {
	for _, name := range handbookOrder {
		if key == name {
			return !limited || key == "current_state" || key == "open_backlog"
		}
	}
	return false
}
