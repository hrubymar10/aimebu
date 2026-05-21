package main

import (
	"encoding/json"
	"strings"
)

type agentPiEventHeader struct {
	Type string `json:"type"`
}

type agentPiSessionEvent struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	ID      string `json:"id"`
	CWD     string `json:"cwd"`
}

func agentParsePiSessionID(output []byte) (string, int) {
	n := 0
	for line := range strings.SplitSeq(string(output), "\n") {
		if n >= 20 {
			break
		}
		n++
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r agentPiSessionEvent
		if json.Unmarshal([]byte(line), &r) == nil && r.Type == "session" && r.ID != "" {
			return r.ID, n
		}
	}
	return "", -1
}

func agentPiHasAgentEnd(output []byte) bool {
	for line := range strings.SplitSeq(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r agentPiEventHeader
		if json.Unmarshal([]byte(line), &r) == nil && r.Type == "agent_end" {
			return true
		}
	}
	return false
}
