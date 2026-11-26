package pi

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
			add(map[string]any{"type": "session", "id": testSession})
			for i := 0; i < tc.calls; i++ {
				body := "first"
				if i == tc.calls-1 {
					body = "second"
				}
				id := body
				add(map[string]any{"type": "tool_execution_start", "toolCallId": id, "toolName": "mcp__aimebu__submit_answer", "args": map[string]any{"messages": []any{map[string]any{"room": "room_1", "body": body}}}})
				add(map[string]any{"type": "tool_execution_end", "toolCallId": id, "toolName": "mcp__aimebu__submit_answer", "isError": false})
			}
			content := []any{}
			if tc.calls == 0 {
				content = append(content, map[string]any{"type": "text", "text": tc.want})
			} else if tc.calls == 2 {
				content = append(content, map[string]any{"type": "text", "text": "trailing narration"})
			}
			add(map[string]any{"type": "turn_end", "message": map[string]any{"role": "assistant", "content": content}})
			add(map[string]any{"type": "agent_settled"})
			fixture := filepath.Join(root, "events.jsonl")
			if err := os.WriteFile(fixture, lines, 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "pi")
			if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\ncat "+fixture+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			turn, err := (&Adapter{Command: path, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), adapter.TurnSpec{TurnID: "submit", Agent: adapter.AgentSpec{ID: "agt_test", Model: "fake-model"}, CheckoutPath: root, NativeSessionHandle: testSession, Mode: adapter.ModeNormal, AgentTools: testToolsConfig(t, "agt_test")})
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for event := range turn.Events() {
				if event.Kind == adapter.EventFinalAnswer {
					got = string(event.Answer)
				}
			}
			exit, err := turn.Wait()
			var gotObject, wantObject any
			_ = json.Unmarshal([]byte(got), &gotObject)
			_ = json.Unmarshal([]byte(tc.want), &wantObject)
			if err != nil || !exit.TerminalEvent || !reflect.DeepEqual(gotObject, wantObject) {
				t.Fatalf("answer=%q want=%q exit=%+v err=%v", got, tc.want, exit, err)
			}
		})
	}
}
