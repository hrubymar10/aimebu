package agenttools

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAttachmentGetBoundsInlineContent(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		data              []byte
		wantImage         bool
		wantText          string
	}{
		{"png", "image/png", []byte("png"), true, ""},
		{"bmp", "image/bmp", []byte("bmp"), false, "cannot be saved to your workspace here"},
		{"svg is read as source text", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), false, "svg xmlns"},
		{"large svg", "image/svg+xml", bytes.Repeat([]byte("x"), 65<<10), false, "cannot be saved to your workspace here"},
		{"text", "text/plain", []byte("small note"), false, "small note"},
		{"large text", "text/plain", bytes.Repeat([]byte("x"), 65<<10), false, "cannot be saved to your workspace here"},
		{"binary", "application/octet-stream", bytes.Repeat([]byte{0xff}, 1<<20), false, "cannot be saved to your workspace here"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write(tc.data)
			}))
			defer httpServer.Close()
			response, _ := (Server{URL: httpServer.URL, Key: "aak_test"}).Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attachment_get","arguments":{"attachment_id":"att_test"}}}`))
			if got := bytes.Contains(response, []byte(`"type":"image"`)); got != tc.wantImage {
				t.Fatalf("image=%v response=%s", got, response)
			}
			if tc.wantText != "" && !bytes.Contains(response, []byte(tc.wantText)) {
				t.Fatalf("response=%s", response)
			}
			if len(response) > 2<<20 {
				t.Fatalf("unbounded response: %d bytes", len(response))
			}
		})
	}
}

func TestBridgeRetriesOnlyReplaySafeCalls(t *testing.T) {
	for _, tc := range []struct {
		label, name, arguments, retryAfter string
		status                             int
		wantCalls                          int
	}{
		{"deliberate unavailable read", "list_rooms", `{}`, "", http.StatusServiceUnavailable, 1},
		{"deliberate bad gateway write", "run_command", `{"cmd":"true","request_key":"stable"}`, "", http.StatusBadGateway, 1},
		{"maintenance read", "list_rooms", `{}`, "0", http.StatusServiceUnavailable, 2},
		{"maintenance keyed write", "run_command", `{"cmd":"true","request_key":"stable"}`, "0", http.StatusServiceUnavailable, 2},
		{"unkeyed write", "memory_add", `{"scope":"project","body":"note"}`, "0", http.StatusServiceUnavailable, 1},
		{"revisioned write", "handbook_update", `{"section":"current_state","content":"note","rev":1}`, "0", http.StatusServiceUnavailable, 1},
	} {
		t.Run(tc.label, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					if tc.retryAfter != "" {
						w.Header().Set("Retry-After", tc.retryAfter)
					}
					w.WriteHeader(tc.status)
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			(Server{URL: server.URL, Key: "aak_test"}).call(t.Context(), tc.name, json.RawMessage(tc.arguments))
			if calls != tc.wantCalls {
				t.Fatalf("requests=%d want=%d", calls, tc.wantCalls)
			}
		})
	}
}

func TestBridgeReturnsMaintenanceResponseAfterRetryWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"maintenance"}`))
	}))
	defer server.Close()
	result := (Server{URL: server.URL, Key: "aak_test", retryWindow: 30 * time.Millisecond}).call(t.Context(), "list_rooms", nil).(map[string]any)
	content := result["content"].([]map[string]string)
	if result["isError"] != true || !strings.Contains(content[0]["text"], `HTTP 503: {"error":"maintenance"}`) {
		t.Fatalf("maintenance response lost: %v", result)
	}
}

func TestBridgeDoesNotLimitHealthyRequestToRetryWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte(`{"rooms":[]}`))
	}))
	defer server.Close()
	result := (Server{URL: server.URL, Key: "aak_test", retryWindow: 10 * time.Millisecond}).call(t.Context(), "list_rooms", nil).(map[string]any)
	if result["isError"] != false {
		t.Fatalf("healthy request was cut short: %v", result)
	}
}

func TestBridgeRetriesSafeConnectionFailures(t *testing.T) {
	for _, firstError := range []error{syscall.ECONNREFUSED, io.EOF, io.ErrUnexpectedEOF} {
		t.Run(firstError.Error(), func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, firstError
				}
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})}
			result := (Server{URL: "http://127.0.0.1:1", Key: "aak_test", Client: client}).call(t.Context(), "list_rooms", nil).(map[string]any)
			if calls != 2 || result["isError"] != false {
				t.Fatalf("calls=%d result=%v", calls, result)
			}
		})
	}
}

func TestMCPRejectsOversizedToolResponse(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1<<20+100))
	}))
	defer httpServer.Close()
	response, _ := (Server{URL: httpServer.URL, Key: "aak_test"}).Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_rooms","arguments":{}}}`))
	if !bytes.Contains(response, []byte(`"isError":true`)) || !bytes.Contains(response, []byte("tool response exceeds 1 MB")) {
		t.Fatalf("oversized response was accepted: %s", response)
	}
}

func TestMCPListsNarrowToolsAndSendsAgentKey(t *testing.T) {
	requests := 0
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/agent/rooms" || r.Header.Get("Authorization") != "Bearer aak_test" || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected request: %s %s %s", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie"))
		}
		_, _ = w.Write([]byte(`{"rooms":[]}`))
	}))
	defer httpServer.Close()
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"list_rooms\",\"arguments\":{}}}\n")
	var output strings.Builder
	server := Server{URL: httpServer.URL, Key: "aak_test"}
	if err := server.Serve(context.Background(), input, &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || requests != 1 {
		t.Fatalf("lines=%d requests=%d", len(lines), requests)
	}
	var listed struct {
		Result struct {
			Tools []tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Result.Tools) != 32 {
		t.Fatalf("tools=%+v", listed.Result.Tools)
	}
	for i, name := range []string{"list_recoveries", "resolve_recovery", "list_rooms", "list_agents", "room_history", "list_macros", "list_schedules", "create_schedule", "delete_schedule", "usage_left", "run_command", "list_commands", "command_log", "cancel_command", "create_room", "dm_agent", "dm_human", "close_room", "handbook_get", "handbook_update", "memory_add", "memory_list", "memory_update", "memory_remove", "memory_recall", "attachment_get", "spawn_agent", "stop_agent", "agent_output", "set_role", "message_get", "react"} {
		if listed.Result.Tools[i].Name != name {
			t.Fatalf("tool %d = %q", i, listed.Result.Tools[i].Name)
		}
	}
	var called struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &called); err != nil {
		t.Fatal(err)
	}
	if called.Result.IsError || len(called.Result.Content) != 1 || called.Result.Content[0].Text != `{"rooms":[]}` {
		t.Fatalf("result=%+v", called)
	}
}

func TestReactToolRoutesAddAndRemove(t *testing.T) {
	var requests []string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer aak_test" {
			t.Errorf("agent key = %q", got)
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer httpServer.Close()
	server := Server{URL: httpServer.URL, Key: "aak_test"}
	for _, args := range []string{
		`{"message_id":"msg_test","emoji":"👍"}`,
		`{"message_id":"msg_test","emoji":"👍","remove":true}`,
	} {
		result := server.call(t.Context(), "react", json.RawMessage(args)).(map[string]any)
		if result["isError"] != false {
			t.Fatalf("react result=%v", result)
		}
	}
	want := []string{"PUT /api/agent/messages/msg_test/reactions/👍", "DELETE /api/agent/messages/msg_test/reactions/👍"}
	if len(requests) != len(want) || requests[0] != want[0] || requests[1] != want[1] {
		t.Fatalf("requests=%v", requests)
	}
}

func TestSpawnAgentRequiresSingleChoice(t *testing.T) {
	type forwarded struct {
		path, key string
		body      map[string]any
	}
	var requests []forwarded
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		requests = append(requests, forwarded{r.URL.Path, r.Header.Get("Idempotency-Key"), body})
		if r.Header.Get("Authorization") != "Bearer aak_test" {
			t.Errorf("agent key missing")
		}
		_, _ = w.Write([]byte(`{"id":"agt_1"}`))
	}))
	defer httpServer.Close()
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"spawn_agent\",\"arguments\":{\"room_id\":\"room_a\",\"team\":\"implement + review\",\"request_key\":\"team-key\"}}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"spawn_agent\",\"arguments\":{\"room_id\":\"room_a\",\"role_key\":\"worker\",\"tool\":\"codex\",\"variant\":\"docker\",\"model\":\"gpt-6-sol\",\"worktree\":true,\"request_key\":\"single-key\"}}}\n")
	var output strings.Builder
	if err := (Server{URL: httpServer.URL, Key: "aak_test"}).Serve(context.Background(), input, &output); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].path != "/api/agent/agents" || requests[0].key != "single-key" || requests[0].body["model"] != "gpt-6-sol" || requests[0].body["team"] != nil || requests[0].body["worktree"] != true {
		t.Fatalf("single request %+v", requests)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"isError":true`) || !strings.Contains(lines[1], `\"id\":\"agt_1\"`) {
		t.Fatalf("responses %q", output.String())
	}
}

func TestMemoryToolsForwardScopeAndEncodedQuery(t *testing.T) {
	var paths []string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer aak_test" {
			t.Errorf("missing agent key")
		}
		if r.Method == "POST" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["scope"] != "task" || body["room_id"] != "room_1" || body["body"] != "remember this" || body["source_message_id"] != "" {
				t.Errorf("memory add body=%+v err=%v", body, err)
			}
		} else if r.Method == "PUT" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["rev"] != float64(2) || body["body"] != "updated" {
				t.Errorf("memory update body=%+v err=%v", body, err)
			}
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer httpServer.Close()
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"memory_add\",\"arguments\":{\"scope\":\"task\",\"room_id\":\"room_1\",\"body\":\"remember this\"}}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"memory_list\",\"arguments\":{\"scope\":\"task\",\"room_id\":\"room_1\"}}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"memory_update\",\"arguments\":{\"scope\":\"project\",\"id\":\"knw_1\",\"rev\":2,\"body\":\"updated\"}}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"memory_remove\",\"arguments\":{\"scope\":\"project\",\"id\":\"knw_1\",\"rev\":3}}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"tools/call\",\"params\":{\"name\":\"memory_recall\",\"arguments\":{\"query\":\"two words\",\"limit\":3}}}\n")
	var output strings.Builder
	if err := (Server{URL: httpServer.URL, Key: "aak_test"}).Serve(context.Background(), input, &output); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 5 || paths[0] != "POST /api/agent/memory" || paths[1] != "GET /api/agent/memory?room_id=room_1&scope=task" || paths[2] != "PUT /api/agent/memory/knw_1?scope=project" || paths[3] != "DELETE /api/agent/memory/knw_1?scope=project&rev=3" || paths[4] != "GET /api/agent/recall?limit=3&query=two+words" {
		t.Fatalf("paths=%+v output=%s", paths, output.String())
	}
}

func TestRoomToolsForwardToNarrowRoutes(t *testing.T) {
	type call struct {
		Method, Path, Key string
		Body              map[string]any
	}
	var calls []call
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		calls = append(calls, call{r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), body})
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer httpServer.Close()
	s := Server{URL: httpServer.URL, Key: "aak_test"}
	for _, input := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_room","arguments":{"title":"Task","parent_id":"room_parent","request_key":"create-key"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"close_room","arguments":{"room_id":"room_child","lifecycle_rev":2,"cascade":true,"request_key":"close-key"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"dm_agent","arguments":{"target_agent_id":"agt_other","body":"a question","request_key":"dm-key"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"dm_human","arguments":{"target_user":" casey ","body":"a decision","request_key":"human-key"}}}`,
	} {
		response, _ := s.Process(context.Background(), []byte(input))
		if strings.Contains(string(response), `"isError":true`) {
			t.Fatalf("tool failed: %s", response)
		}
	}
	if len(calls) != 4 || calls[2].Method != "POST" || calls[2].Path != "/api/agent/dms" || calls[2].Key != "dm-key" || calls[2].Body["target_agent_id"] != "agt_other" || calls[2].Body["body"] != "a question" {
		t.Fatalf("dm_agent call=%+v", calls)
	}
	if calls[3].Method != "POST" || calls[3].Path != "/api/agent/human-dms" || calls[3].Key != "human-key" || calls[3].Body["target_user"] != "casey" || calls[3].Body["body"] != "a decision" {
		t.Fatalf("dm_human call=%+v", calls)
	}
	if len(calls) != 4 ||
		calls[0].Method != "POST" || calls[0].Path != "/api/agent/rooms" || calls[0].Key != "create-key" || calls[0].Body["title"] != "Task" ||
		calls[1].Method != "POST" || calls[1].Path != "/api/agent/rooms/room_child/close" || calls[1].Key != "close-key" || calls[1].Body["lifecycle_rev"] != float64(2) || calls[1].Body["cascade"] != true {
		t.Fatalf("forwarded calls=%+v", calls)
	}
}

func TestCreateScheduleForwardsIdempotentLeadRequest(t *testing.T) {
	calls := 0
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/agent/rooms/room_test/schedules" || r.Header.Get("Idempotency-Key") != "stable" {
			t.Errorf("request=%s %s key=%q", r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["target_lead"] != true || body["interval_seconds"] != float64(300) || body["body"] != "check status" {
			t.Errorf("body=%v", body)
		}
		_, _ = w.Write([]byte(`{"id":"sch_test"}`))
	}))
	defer httpServer.Close()
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_schedule","arguments":{"room_id":"room_test","target_lead":true,"body":"check status","kind":"interval","timezone":"UTC","interval_seconds":300,"request_key":"stable"}}}`)
	result, _ := (Server{URL: httpServer.URL, Key: "aak_test"}).Process(t.Context(), raw)
	if calls != 1 || !bytes.Contains(result, []byte(`sch_test`)) {
		t.Fatalf("calls=%d result=%s", calls, result)
	}
}

// create_schedule forwards a cron schedule's expression untouched, and its schema and text tell the lead about the kind.
func TestCreateScheduleForwardsCron(t *testing.T) {
	var body map[string]any
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"id":"sch_cron"}`))
	}))
	defer httpServer.Close()
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_schedule","arguments":{"room_id":"room_test","target_lead":true,"body":"standup","kind":"cron","cron":"0 8,12,16,20 * * *","timezone":"Europe/Prague","request_key":"k"}}}`)
	result, _ := (Server{URL: httpServer.URL, Key: "aak_test"}).Process(t.Context(), raw)
	if body["kind"] != "cron" || body["cron"] != "0 8,12,16,20 * * *" || body["timezone"] != "Europe/Prague" || !bytes.Contains(result, []byte(`sch_cron`)) {
		t.Fatalf("body=%v result=%s", body, result)
	}
	for _, entry := range catalog {
		if entry.Name != "create_schedule" {
			continue
		}
		properties := entry.InputSchema["properties"].(map[string]any)
		if _, ok := properties["cron"]; !ok || !strings.Contains(entry.Description, "cron") || !strings.Contains(entry.Description, "0 8,12,16,20 * * *") {
			t.Fatalf("create_schedule does not describe cron: %v / %s", properties, entry.Description)
		}
	}
}

// A lead may pick restart (the fresh session of a broken-session recovery) through resolve_recovery.
func TestMCPResolveRecoveryAcceptsRestart(t *testing.T) {
	var gotPath, gotBody string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer httpServer.Close()
	server := Server{URL: httpServer.URL, Key: "aak_test"}
	response, _ := server.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"resolve_recovery","arguments":{"item_id":"inb_x","choice":"restart"}}}`))
	if gotPath != "/api/agent/recoveries/inb_x/decision" || !strings.Contains(gotBody, `"restart"`) || bytes.Contains(response, []byte(`"isError":true`)) {
		t.Fatalf("path=%q body=%q response=%s", gotPath, gotBody, response)
	}
	if !bytes.Contains(listTools(t, server), []byte(`"restart"`)) {
		t.Fatal("restart is not in the resolve_recovery enum")
	}
}

// A lead may pick abandon_stop (Abandon plus stopping the agent) through resolve_recovery; the tool lists it and its description says what it does.
func TestMCPResolveRecoveryAcceptsAbandonStop(t *testing.T) {
	var gotPath, gotBody, gotKey string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotPath, gotBody, gotKey = r.URL.Path, string(data), r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer httpServer.Close()
	server := Server{URL: httpServer.URL, Key: "aak_test"}
	response, _ := server.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"resolve_recovery","arguments":{"item_id":"inb_x","choice":"abandon_stop"}}}`))
	if gotPath != "/api/agent/recoveries/inb_x/decision" || !strings.Contains(gotBody, `"abandon_stop"`) || !strings.Contains(gotKey, "abandon_stop") || bytes.Contains(response, []byte(`"isError":true`)) {
		t.Fatalf("path=%q body=%q key=%q response=%s", gotPath, gotBody, gotKey, response)
	}
	tools := listTools(t, server)
	if !bytes.Contains(tools, []byte(`"abandon_stop"`)) || !bytes.Contains(tools, []byte("stop_agent needs")) {
		t.Fatal("abandon_stop is not in the resolve_recovery enum or its description")
	}
}

func listTools(t *testing.T, server Server) []byte {
	t.Helper()
	response, _ := server.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	return response
}
