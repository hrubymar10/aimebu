package pi

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
			add(map[string]any{"type": "session", "id": testSession})
			for i := 0; i < tc.calls; i++ {
				body := "first"
				if i == tc.calls-1 {
					body = "second"
				}
				id := body
				var args any = map[string]any{"messages": []any{map[string]any{"room": "room_1", "body": body}}}
				if tc.bad {
					args = map[string]any{"messages": "not a list"}
				}
				add(map[string]any{"type": "tool_execution_start", "toolCallId": id, "toolName": "mcp__aimebu__submit_answer", "args": args})
				add(map[string]any{"type": "tool_execution_end", "toolCallId": id, "toolName": "mcp__aimebu__submit_answer", "isError": false})
			}
			content := []any{}
			if tc.calls == 0 || tc.bad {
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

func TestAnswerToolOfferedOnlyAtSupportedVersion(t *testing.T) {
	for version, want := range map[string]bool{"": false, "1.6.0": false, "1.6.1": true} {
		root := t.TempDir()
		path := filepath.Join(root, "pi")
		if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ncat >/dev/null\n"), 0700); err != nil {
			t.Fatal(err)
		}
		turn, err := (&Adapter{Command: path, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), adapter.TurnSpec{TurnID: "offer", Agent: adapter.AgentSpec{ID: "agt_test", Model: "fake-model"}, CheckoutPath: root, NativeSessionHandle: testSession, Mode: adapter.ModeNormal, HarnessVersion: version, AgentTools: testToolsConfig(t, "agt_test")})
		if err != nil {
			t.Fatal(err)
		}
		for range turn.Events() {
		}
		_, _ = turn.Wait()
		var found bool
		_ = filepath.WalkDir(filepath.Join(root, "runtime"), func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Name() == "mcp.json" {
				data, _ := os.ReadFile(p)
				found = true
				if got := strings.Contains(string(data), "answer_tool=1"); got != want {
					t.Errorf("version %q: answer_tool offered=%t want %t\n%s", version, got, want, data)
				}
			}
			return nil
		})
		if !found {
			t.Fatalf("version %q: no mcp.json written", version)
		}
	}
}
