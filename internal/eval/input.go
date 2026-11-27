package eval

import (
	"fmt"
	"path"
	"sort"
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

// HistoryMessage is one message of a room as the stand-in's room_history and message_get return it. Placeholders such as {dispatcher} and
// {task_room} are expanded.
type HistoryMessage struct {
	No   int64  `json:"no"`
	Room string `json:"room_id"`
	From string `json:"from"`
	Body string `json:"body"`
}

// Messages is every message the stand-in's tools know in this scenario: each input note (the supervisor hands an agent the notes of messages that
// are in its rooms) and the scenario's own History, in order of number. Two messages never share a number.
func (in *Input) Messages() ([]HistoryMessage, error) {
	var notes []struct {
		No   int64  `json:"no"`
		Room string `json:"room_id"`
		From string `json:"from"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(Expand(string(in.Notes))), &notes); err != nil {
		return nil, fmt.Errorf("input.notes: %w", err)
	}
	var out []HistoryMessage
	for _, n := range notes {
		if n.No > 0 {
			out = append(out, HistoryMessage{No: n.No, Room: n.Room, From: n.From, Body: n.Body})
		}
	}
	for _, m := range in.History {
		if m.No < 1 || m.Room == "" || m.From == "" || m.Body == "" {
			return nil, fmt.Errorf("input.history: every message needs no, room_id, from and body")
		}
		out = append(out, HistoryMessage{No: m.No, Room: Expand(m.Room), From: Expand(m.From), Body: Expand(m.Body)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].No < out[j].No })
	for i := 1; i < len(out); i++ {
		if out[i].No == out[i-1].No {
			return nil, fmt.Errorf("input: message number %d is used twice (a history message repeats a note)", out[i].No)
		}
	}
	return out, nil
}

// Checkout is the work a case's checkout holds: Files are written and committed with Commit as the message, on top of the run's base project (a
// small Go module that builds and passes its test). A scenario about finished work needs the work to be there: an agent that looks and finds an
// empty directory asks instead of reporting.
type Checkout struct {
	Commit string            `json:"commit"`
	Files  map[string]string `json:"files"`
}

func (c *Checkout) validate() error {
	if c == nil {
		return nil
	}
	if c.Commit == "" || len(c.Files) == 0 {
		return fmt.Errorf("input.checkout needs a commit message and files")
	}
	for name := range c.Files {
		clean := path.Clean(name)
		if clean != name || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") ||
			clean == ".git" || strings.HasPrefix(clean, ".git/") || clean == ".aimebu" || strings.HasPrefix(clean, ".aimebu/") {
			return fmt.Errorf("input.checkout: %q is not a plain relative path outside .git and .aimebu", name)
		}
	}
	return nil
}
