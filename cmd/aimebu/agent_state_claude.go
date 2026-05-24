package main

import "bytes"

type claudeCodeStateDetector struct{}

func (claudeCodeStateDetector) Name() string {
	return "claude-code"
}

func (claudeCodeStateDetector) Detect(line []byte) string {
	if claudeLineHasSpinner(line) {
		return "thinking"
	}
	if bytes.Contains(line, []byte(agentPTYCanary)) {
		return "idle"
	}
	// TODO: Emit tool_call once we have stable Claude PTY samples that
	// distinguish tool execution from ordinary TUI redraws.
	return ""
}

func claudeLineHasSpinner(line []byte) bool {
	for _, glyph := range [][]byte{
		[]byte("✻"),
		[]byte("✶"),
		[]byte("✽"),
		[]byte("✢"),
	} {
		if bytes.Contains(line, glyph) {
			return true
		}
	}
	return bytes.Contains(line, []byte("*")) && claudeLineHasANSI(line)
}

func claudeLineHasANSI(line []byte) bool {
	return bytes.Contains(line, []byte("\x1b["))
}
