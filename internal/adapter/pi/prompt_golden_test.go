package pi

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the prompt golden files")

func TestPromptUsesToolParameterSchema(t *testing.T) {
	if !strings.Contains(turnPrompt, "Pass every field required by its schema") ||
		!strings.Contains(turnPrompt, "<json-schema>"+string(answer.ResponseSchema)+"</json-schema>") ||
		!strings.Contains(turnPrompt, "calling the aimebu submit_answer tool") {
		t.Fatal("prompt and tool schema disagree")
	}
}

func TestAnswerToolPromptVersionGate(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{{"1.6.0", false}, {adapter.MinPiAnswerTool, true}, {"1.6.2", true}} {
		got := promptFor(adapter.TurnSpec{Mode: adapter.ModeNormal, HarnessVersion: tc.version})
		if strings.Contains(got, "calling the aimebu submit_answer tool") != tc.want {
			t.Fatalf("version %s prompt gate=%t", tc.version, tc.want)
		}
	}
}

// The goldens were captured from the hand-written prompt before the text moved
// into the shared prompt package. They pin the exact bytes sent on stdin.
func TestTurnInputGolden(t *testing.T) {
	cases := map[string]adapter.TurnSpec{
		"minimal":   {TurnID: "turn_1", Agent: adapter.AgentSpec{ID: "agt_1"}, NotesJSON: json.RawMessage(`[]`)},
		"nil_parts": {TurnID: "turn_nil", Agent: adapter.AgentSpec{ID: "agt_nil"}},
		"full": {TurnID: "turn_2", Agent: adapter.AgentSpec{ID: "agt_2", Tool: "pi"},
			NotesJSON:    json.RawMessage(`[{"id":"note_1","from":"usr_1","body":"hi @worker","retry_notice":"check"}]`),
			ContextJSON:  json.RawMessage(`{"role_body":"Be the worker.","rooms":[{"room_id":"room_1"}],"team_roster":[{"id":"agt_2","slug":"ada"}],"you_are":{"id":"agt_2"}}`),
			AllowedTools: []string{"post_message", "create_room"}},
		"docker_attachments": {TurnID: "turn_4", Agent: adapter.AgentSpec{ID: "agt_4", Variant: "docker"}, NotesJSON: json.RawMessage(`[{"attachments":[{"id":"att_1"}]}]`),
			AttachmentBaseURL: "http://host.docker.internal:9997/api/agent/attachments", AttachmentBearer: "aab_secret"},
		"bearer_without_url": {TurnID: "turn_5", Agent: adapter.AgentSpec{ID: "agt_5"}, AttachmentBearer: "aab_secret"},
		"unicode":            {TurnID: "turn_3", Agent: adapter.AgentSpec{ID: "agt_3"}, NotesJSON: json.RawMessage(`[{"body":"界 <b> & \"q\""}]`), AllowedTools: []string{}},
	}
	for name, spec := range cases {
		got, err := turnInput(spec)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "turn_input_"+name+".golden")
		if *updateGolden {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: turn input changed (%d bytes, want %d)", name, len(got), len(want))
		}
	}
}

func TestTurnInputUsesContextOverrides(t *testing.T) {
	t.Parallel()
	spec := adapter.TurnSpec{TurnID: "turn_o", Agent: adapter.AgentSpec{ID: "agt_o"}, NotesJSON: json.RawMessage(`[]`),
		ContextJSON: json.RawMessage(`{"role_body":"Be the worker.","prompt_overrides":{"tail":"MY CLOSING GUIDANCE"}}`)}
	got, err := turnInput(spec)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "MY CLOSING GUIDANCE") || strings.Contains(text, "A lead with a completed review") || !strings.Contains(text, `"role_body":"Be the worker."`) {
		t.Fatalf("override, and the context still goes after it: %q", text)
	}
	if strings.Count(text, "MY CLOSING GUIDANCE") != 1 || strings.Contains(text, "prompt_overrides") {
		t.Fatalf("the override is in the instructions once, not repeated in the forwarded context: %q", text)
	}
	plain := spec
	plain.ContextJSON = json.RawMessage(`{"role_body":"Be the worker."}`)
	if got, err := turnInput(plain); err != nil || !strings.Contains(string(got), "A lead with a completed review") {
		t.Fatalf("without overrides the shipped text stays: %v", err)
	}
}
