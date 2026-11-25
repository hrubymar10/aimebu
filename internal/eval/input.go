package eval

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"
)

// Presets are the built-in contexts a scenario can start from. "task_room" is a worker in a task room with a dispatcher; "no_tools" is the same
// agent with nothing but its reply to give.
func presetContext(name string) (map[string]any, error) {
	switch name {
	case "", "task_room":
		return map[string]any{
			"you_are": map[string]any{"id": WorkerID, "name": "eval-worker", "role": "worker", "harness": "eval", "model": "eval-model"},
			"team_roster": []map[string]any{
				{"id": WorkerID, "name": "eval-worker", "role": "worker", "harness": "eval", "model": "eval-model"},
				{"id": DispatcherID, "name": "eval-dispatch", "role": "dispatcher", "harness": "eval", "model": "eval-model"},
				{"id": OtherAgentID, "name": "eval-other", "role": "worker", "harness": "eval", "model": "eval-model"},
			},
			"rooms":     []map[string]any{{"kind": "task", "room_id": TaskRoom, "title": "Eval task room"}},
			"role_body": "You are an implementation worker. Work in your assigned checkout and report to the agent who briefed you.",
		}, nil
	default:
		return nil, fmt.Errorf("unknown input preset %q", name)
	}
}

// Expand replaces the {name} placeholders of a check value.
func Expand(value string) string {
	return strings.NewReplacer("{worker}", WorkerID, "{dispatcher}", DispatcherID, "{other_agent}", OtherAgentID, "{person}", PersonID,
		"{task_room}", TaskRoom, "{home_room}", HomeRoom).Replace(value)
}

// Build returns the notes and the context JSON a turn is dispatched with: the preset, the scenario's keys over it, then the filler knowledge.
// Placeholders such as {task_room} in the scenario's notes and context are expanded.
func (in *Input) Build() (notes, context json.RawMessage, err error) {
	base, err := presetContext(in.Preset)
	if err != nil {
		return nil, nil, err
	}
	for key, value := range in.Context {
		base[key] = json.RawMessage(Expand(string(value)))
	}
	if in.FillerTokens > 0 {
		base["knowledge"] = map[string]any{"project_memory": []map[string]any{{"id": "knw_filler", "body": filler(in.FillerTokens, in.FillerMarker)}}}
	}
	context, err = json.Marshal(base)
	if err != nil {
		return nil, nil, err
	}
	if !json.Valid(in.Notes) {
		return nil, nil, fmt.Errorf("input.notes is not valid JSON")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(in.Notes, &list); err != nil {
		return nil, nil, fmt.Errorf("input.notes must be an array: %w", err)
	}
	return json.RawMessage(Expand(string(in.Notes))), context, nil
}

// filler is deterministic text of about n tokens (4 characters each) whose last line is marker.
func filler(tokens int, marker string) string {
	const line = "Project note: the build uses the module cache and the release branch follows the weekly cut. "
	var b strings.Builder
	for b.Len() < tokens*4 {
		fmt.Fprintf(&b, "%s(%d) ", line, b.Len())
	}
	b.WriteString("\n")
	b.WriteString(marker)
	return b.String()
}
