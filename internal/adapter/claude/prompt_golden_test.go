package claude

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the prompt golden files")

// The goldens were captured from the hand-written prompt before the text moved
// into the shared prompt package. They pin the exact bytes handed to the CLI.
func TestSystemPromptGolden(t *testing.T) {
	cases := map[string]json.RawMessage{
		"no_context":      nil,
		"invalid_context": json.RawMessage(`{`),
		"empty_role":      json.RawMessage(`{"role_body":""}`),
		"role_body":       json.RawMessage(`{"role_body":"You are the worker.\nStay in your checkout."}`),
		"role_unicode":    json.RawMessage(`{"role_body":"Role: 界 — \"quoted\" <tag> & more","rooms":[{"room_id":"room_1"}]}`),
	}
	for name, context := range cases {
		got := systemPromptFor(adapter.TurnSpec{ContextJSON: context})
		path := filepath.Join("testdata", "system_prompt_"+name+".golden")
		if *updateGolden {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s: system prompt changed (%d bytes, want %d)", name, len(got), len(want))
		}
	}
}

func TestSystemPromptUsesContextOverrides(t *testing.T) {
	t.Parallel()
	context := json.RawMessage(`{"role_body":"You are the worker.","prompt_overrides":{"tail":"MY CLOSING GUIDANCE"}}`)
	got := systemPromptFor(adapter.TurnSpec{ContextJSON: context})
	if !strings.Contains(got, "MY CLOSING GUIDANCE") || strings.Contains(got, "A lead with a completed review") || !strings.HasSuffix(got, "Role instructions:\nYou are the worker.") {
		t.Fatalf("override and role body: %q", got)
	}
}

func TestForwardedContextDoesNotRepeatTheOverrides(t *testing.T) {
	t.Parallel()
	input, err := turnInput(adapter.TurnSpec{TurnID: "turn_o", NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{"role_body":"Be.","prompt_overrides":{"tail":"MY CLOSING GUIDANCE"}}`)})
	if err != nil || strings.Contains(string(input), "prompt_overrides") || strings.Contains(string(input), "MY CLOSING GUIDANCE") || !strings.Contains(string(input), `"role_body":"Be."`) {
		t.Fatalf("stdin input: %q %v", input, err)
	}
}
