package opencode

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestSubmitAnswerWinsOverTrailingText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls int
		want  string
		bad   bool
	}{
		{"tool only", 1, `{"messages":[{"room":"room_1","body":"second"}]}`, false},
		{"tool and trailing text", 2, `{"messages":[{"room":"room_1","body":"second"}]}`, false},
		{"free text fallback", 0, `{"messages":[{"room":"room_1","body":"fallback"}]}`, false},
		{"invalid tool input falls back to text", 1, `{"messages":[{"room":"room_1","body":"fallback"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var lines []byte
			add := func(event any) {
				raw, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				lines = append(append(lines, raw...), '\n')
			}
			add(map[string]any{"type": "step_start", "sessionID": testSession})
			for i := 0; i < tc.calls; i++ {
				body := "first"
				if i == tc.calls-1 {
					body = "second"
				}
				var input any = map[string]any{"messages": []any{map[string]any{"room": "room_1", "body": body}}}
				if tc.bad {
					input = map[string]any{"messages": "not a list"}
				}
				add(map[string]any{"type": "tool_use", "sessionID": testSession, "part": map[string]any{"tool": "aimebu_submit_answer", "callID": body, "state": map[string]any{"status": "completed", "input": input}}})
			}
			add(map[string]any{"type": "step_start", "sessionID": testSession})
			if tc.calls == 0 || tc.bad {
				add(map[string]any{"type": "text", "sessionID": testSession, "part": map[string]any{"text": tc.want}})
			} else if tc.calls == 2 {
				add(map[string]any{"type": "text", "sessionID": testSession, "part": map[string]any{"text": "trailing narration"}})
			}
			add(map[string]any{"type": "step_finish", "sessionID": testSession, "part": map[string]any{"reason": "stop"}})
			fixture := filepath.Join(root, "events.jsonl")
			if err := os.WriteFile(fixture, lines, 0600); err != nil {
				t.Fatal(err)
			}
			path, _ := script(t, root, "cat "+fixture+"\n")
			turn, err := (&Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), spec(t, root))
			if err != nil {
				t.Fatal(err)
			}
			events, exit, err := drain(t, turn)
			if err != nil || !exit.TerminalEvent {
				t.Fatalf("exit=%+v err=%v", exit, err)
			}
			var got string
			for _, event := range events {
				if event.Kind == adapter.EventFinalAnswer {
					got = string(event.Answer)
				}
			}
			var gotObject, wantObject any
			_ = json.Unmarshal([]byte(got), &gotObject)
			_ = json.Unmarshal([]byte(tc.want), &wantObject)
			if !reflect.DeepEqual(gotObject, wantObject) {
				t.Fatalf("answer=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestAnswerToolOfferedOnlyAtSupportedVersion(t *testing.T) {
	for version, want := range map[string]bool{"": false, "1.19.5": false, "1.19.6": true} {
		root := t.TempDir()
		capture := filepath.Join(root, "config.json")
		path := filepath.Join(root, "opencode")
		body := "#!/bin/sh\nprintf '%s' \"$OPENCODE_CONFIG_CONTENT\" > " + capture + "\ncat >/dev/null\ncat " + fixture(t, "ok.jsonl") + "\n"
		if err := testenv.WriteExecutable(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		turnSpec := spec(t, root)
		turnSpec.HarnessVersion = version
		turn, err := (&Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), turnSpec)
		if err != nil {
			t.Fatal(err)
		}
		if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
			t.Fatalf("exit=%+v err=%v", exit, err)
		}
		data, _ := os.ReadFile(capture)
		if got := strings.Contains(string(data), "answer_tool=1"); got != want {
			t.Fatalf("version %q: answer_tool offered=%t want %t\n%s", version, got, want, data)
		}
	}
}
