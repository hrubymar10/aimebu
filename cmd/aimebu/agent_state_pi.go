package main

import "encoding/json"

type piStateDetector struct {
	current      string
	preToolState string
}

func (*piStateDetector) Name() string {
	return "pi"
}

func (d *piStateDetector) Detect(line []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &event); err != nil || event.Type == "" {
		return ""
	}
	switch event.Type {
	case "thinking_start":
		d.current = "thinking"
		return d.current
	case "tool_execution_start":
		d.preToolState = d.current
		if d.preToolState == "" {
			d.preToolState = "idle"
		}
		d.current = "tool_call"
		return d.current
	case "tool_execution_end":
		if d.preToolState == "" {
			d.preToolState = "idle"
		}
		d.current = d.preToolState
		d.preToolState = ""
		return d.current
	case "turn_end":
		d.current = "idle"
		d.preToolState = ""
		return d.current
	}
	return ""
}
