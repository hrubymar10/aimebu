package adapter

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const MinOpenCodeAnswerTool = "1.19.6"
const MinPiAnswerTool = "1.6.1"

// AnswerToolMinimum is the oldest harness version whose toolbox path this
// adapter supports. Older and unrecognized versions keep the text fallback.
func AnswerToolMinimum(tool string) string {
	switch tool {
	case "opencode":
		return MinOpenCodeAnswerTool
	case "pi":
		return MinPiAnswerTool
	}
	return ""
}

// SupportsAnswerTool reports whether the probed CLI version is at least the
// supported toolbox release. A suffix after the patch version is allowed.
func SupportsAnswerTool(tool, version string) bool {
	minimum := AnswerToolMinimum(tool)
	if minimum == "" {
		return false
	}
	read := func(value string) ([3]int, bool) {
		var parts [3]int
		value = strings.TrimSpace(value)
		value = strings.TrimPrefix(value, tool+" ")
		segments := strings.SplitN(value, ".", 3)
		if len(segments) != 3 {
			return parts, false
		}
		for i, part := range segments {
			if i == 2 {
				words := strings.Fields(part)
				if len(words) == 0 {
					return parts, false
				}
				part = words[0]
				part = strings.SplitN(part, "-", 2)[0]
			}
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 {
				return parts, false
			}
			parts[i] = n
		}
		return parts, true
	}
	got, ok := read(version)
	if !ok {
		return false
	}
	want, _ := read(minimum)
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

// AnswerToolSupport is the capability probe's human-readable result. Empty
// versions cannot establish support and keep the text path without a warning.
func AnswerToolSupport(tool, version string) (bool, string) {
	minimum := AnswerToolMinimum(tool)
	if minimum == "" || version == "" {
		return false, ""
	}
	if SupportsAnswerTool(tool, version) {
		return true, ""
	}
	return false, fmt.Sprintf("update %s to %s or newer for submit_answer", tool, minimum)
}

// AnswerToolEndpoint marks an MCP endpoint as belonging to a turn that reads submit_answer, so the toolbox lists the tool for it and for no other harness.
// Callers use it only when SupportsAnswerTool holds for the turn.
func AnswerToolEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	q := u.Query()
	q.Set("answer_tool", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

// OffersAnswerTool reports whether a turn with this spec is offered the submit_answer tool. It is the one test the OpenCode and Pi
// adapters apply before they list the tool for the turn and mark the turn's bus endpoint with AnswerToolEndpoint, each naming its own
// tool: a turn whose probed harness version supports it and whose mode is the normal one (an empty mode is not), and no other. The supervisor answers
// the turn's answer frame with the same result, so the server's held-reply repair note follows what the turn actually had — a launcher
// turn included — and the report can never drift from what the adapter did.
func OffersAnswerTool(tool string, spec TurnSpec) bool {
	return spec.Mode == ModeNormal && SupportsAnswerTool(tool, spec.HarnessVersion)
}
