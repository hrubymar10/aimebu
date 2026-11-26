package opencode

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

func TestSubmitAnswerWinsOverTrailingText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls int
		want  string
	}{
		{"tool only", 1, `{"messages":[{"room":"room_1","body":"second"}]}`},
		{"tool and trailing text", 2, `{"messages":[{"room":"room_1","body":"second"}]}`},
		{"free text fallback", 0, `{"messages":[{"room":"room_1","body":"fallback"}]}`},
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
				add(map[string]any{"type": "tool_use", "sessionID": testSession, "part": map[string]any{"tool": "aimebu_submit_answer", "callID": body, "state": map[string]any{"status": "completed", "input": map[string]any{"messages": []any{map[string]any{"room": "room_1", "body": body}}}}}})
			}
			add(map[string]any{"type": "step_start", "sessionID": testSession})
			if tc.calls == 0 {
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
