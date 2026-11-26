package agenttools

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

func TestSubmitAnswerSchemaAndValidation(t *testing.T) {
	var schema any
	if err := json.Unmarshal(answer.ResponseSchema, &schema); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range (Server{AnswerTool: true}).tools() {
		if item.Name != "submit_answer" {
			continue
		}
		found = true
		got, err := json.Marshal(item.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(schema, decoded) {
			t.Fatalf("tool schema differs from response schema")
		}
	}
	if !found {
		t.Fatal("submit_answer missing from catalog")
	}
	for _, tc := range []struct {
		input    string
		failed   bool
		contains string
	}{
		{`{"messages":[]}`, false, "accepted"},
		{`{"messages":[{"room":"r","body":123}]}`, true, "body"},
	} {
		response, _ := (Server{AnswerTool: true}).Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_answer","arguments":`+tc.input+`}}`))
		var decoded struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Result.IsError != tc.failed || len(decoded.Result.Content) != 1 || !strings.Contains(strings.ToLower(decoded.Result.Content[0].Text), tc.contains) || strings.Contains(decoded.Result.Content[0].Text, "123") {
			t.Fatalf("result=%s", response)
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestSubmitAnswerIsOfferedOnlyWhenAdapterReadsIt(t *testing.T) {
	names := func(s Server) map[string]bool {
		response, _ := s.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		var decoded struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response, &decoded); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, item := range decoded.Result.Tools {
			out[item.Name] = true
		}
		return out
	}
	if got := names(Server{}); got["submit_answer"] || !got["list_rooms"] {
		t.Fatalf("default catalog (Claude, Codex, Vibe) must lack submit_answer: %v", got)
	}
	if got := names(Server{AnswerTool: true}); !got["submit_answer"] || !got["list_rooms"] {
		t.Fatalf("opted-in catalog (OpenCode, Pi) must carry submit_answer: %v", got)
	}
	response, _ := (Server{}).Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_answer","arguments":{"messages":[]}}}`))
	if !strings.Contains(string(response), `"isError":true`) || strings.Contains(string(response), "accepted") {
		t.Fatalf("a harness that does not read the tool must never be told it was accepted: %s", response)
	}
}
