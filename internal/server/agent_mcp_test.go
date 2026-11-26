package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestAgentMCPHTTPUsesAgentIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgent(t, "role_worker_default")
	request := func(credential, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/agent/mcp", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	bad := request("aak_invalid", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid key status=%d", bad.StatusCode)
	}
	good := request(key, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_rooms","arguments":{}}}`)
	defer good.Body.Close()
	if good.StatusCode != http.StatusOK {
		t.Fatalf("valid key status=%d", good.StatusCode)
	}
	data, err := io.ReadAll(good.Body)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != 2 || result.Result.IsError || len(result.Result.Content) != 1 || !bytes.Contains([]byte(result.Result.Content[0].Text), []byte(f.room.ID)) {
		t.Fatalf("MCP result=%s", data)
	}
}

func TestAgentMCPAnswerToolOnlyWhenOptedIn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgent(t, "role_worker_default")
	list := func(query string) string {
		req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/agent/mcp"+query, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return string(data)
	}
	if body := list(""); strings.Contains(body, "submit_answer") || !strings.Contains(body, "list_rooms") {
		t.Fatalf("default endpoint: %s", body)
	}
	if body := list("?answer_tool=1"); !strings.Contains(body, "submit_answer") {
		t.Fatalf("opted-in endpoint: %s", body)
	}
}
