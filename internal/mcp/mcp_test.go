package mcp

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
	"github.com/hrubymar10/aimebu/internal/types"
)

func callMCPToolForTest(t *testing.T, c *client.Client, id int, name string, args any) *response {
	t.Helper()
	argJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": json.RawMessage(argJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := handle(c, request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
		Params:  params,
	}, nil)
	if resp == nil {
		t.Fatalf("tools/call %s returned nil", name)
	}
	return resp
}

func requireMCPTextResult(t *testing.T, resp *response) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map[string]any", resp.Result)
	}
	content, ok := result["content"].([]textContent)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want one textContent", result["content"])
	}
	return content[0].Text
}

func TestDetectHarness(t *testing.T) {
	// Clear all env vars detectHarness may consult, then re-set per case.
	clear := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{
			"AIMEBU_HARNESS",
			"CLAUDECODE",
			"CLAUDE_CODE_ENTRYPOINT",
			"CURSOR_TRACE_ID",
			"CURSOR_SESSION_ID",
			"AIDER_VERSION",
		} {
			t.Setenv(k, "")
		}
	}

	tests := []struct {
		name string
		// env to set after clear; key→value. Empty map means "no env vars".
		env  map[string]string
		want string
	}{
		// Tier 2 — owned AIMEBU_HARNESS env var (post-AI fallback).
		{name: "aimebu_harness codex", env: map[string]string{"AIMEBU_HARNESS": "codex"}, want: "codex"},
		{name: "aimebu_harness cline", env: map[string]string{"AIMEBU_HARNESS": "cline"}, want: "cline"},
		{name: "aimebu_harness pi", env: map[string]string{"AIMEBU_HARNESS": "pi"}, want: "pi"},
		// AIMEBU_HARNESS wins over upstream env sniff.
		{name: "aimebu_harness wins over claudecode", env: map[string]string{"AIMEBU_HARNESS": "codex", "CLAUDECODE": "1"}, want: "codex"},

		// Tier 3 — upstream env sniff for harnesses that actually propagate.
		{name: "claudecode", env: map[string]string{"CLAUDECODE": "1"}, want: "claude-code"},
		{name: "claude_code_entrypoint", env: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}, want: "claude-code"},
		{name: "cursor trace id", env: map[string]string{"CURSOR_TRACE_ID": "abc"}, want: "cursor"},
		{name: "cursor session id", env: map[string]string{"CURSOR_SESSION_ID": "abc"}, want: "cursor"},
		{name: "aider", env: map[string]string{"AIDER_VERSION": "0.1"}, want: "aider"},

		// Tier 4 — nothing set.
		{name: "unknown", env: map[string]string{}, want: "unknown"},

		// Codex env vars are deliberately NOT in tier 3 anymore — codex doesn't
		// propagate them to MCP children, so detection by env was misleading.
		// Setting them alone should yield "unknown".
		{name: "codex session_id alone falls through to unknown", env: map[string]string{"CODEX_SESSION_ID": "abc"}, want: "unknown"},
		{name: "codex thread_id alone falls through to unknown", env: map[string]string{"CODEX_THREAD_ID": "abc"}, want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clear(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := detectHarness(); got != tt.want {
				t.Fatalf("detectHarness() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBusEtiquetteCoversRoleAssignmentWakeup(t *testing.T) {
	for _, want := range []string{
		"assigned room role keys",
		"Exact in-room slugs and special group tags take precedence over role keys",
		"System sender (from_kind=system): should_respond=true only when addressed_to_me=true",
		"call `bus_role_get` for that room",
		`targeted "role cleared" message`,
		"do not post an acknowledgement",
		"Use `bus_react` for lightweight acknowledgements",
	} {
		if !strings.Contains(busEtiquette, want) {
			t.Fatalf("bus etiquette missing %q", want)
		}
	}
}

func TestToolsIncludeBusReact(t *testing.T) {
	var found bool
	for _, tool := range tools {
		if tool.Name != "bus_react" {
			continue
		}
		found = true
		if !strings.Contains(tool.Description, "acknowledgements") {
			t.Fatalf("bus_react description = %q, want acknowledgement guidance", tool.Description)
		}
		if !reflect.DeepEqual(tool.InputSchema.Required, []string{"message_id", "emoji"}) {
			t.Fatalf("bus_react required = %v", tool.InputSchema.Required)
		}
	}
	if !found {
		t.Fatal("bus_react tool not registered")
	}
}

func TestMemorySearchToolsAreBusScoped(t *testing.T) {
	for _, tt := range []struct {
		name  string
		wants []string
	}{
		{
			name: "bus_memory_list",
			wants: []string{
				"Only use this if your task is about the aimebu message bus",
				"do not call it and do not register just to use it",
				"aimebu bus memory",
				"requires bus_register first",
				"not a general notes, file, or knowledge search",
			},
		},
		{
			name: "bus_recall",
			wants: []string{
				"Only use this if your task is about the aimebu message bus",
				"do not call it and do not register just to use it",
				"aimebu message-bus history",
				"requires bus_register first",
				"not a general notes, file, or knowledge search",
				"not your conversation history",
				"not for recalling the current chat",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var desc string
			for _, tool := range tools {
				if tool.Name == tt.name {
					desc = tool.Description
					break
				}
			}
			if desc == "" {
				t.Fatalf("%s tool not registered", tt.name)
			}
			for _, want := range tt.wants {
				if !strings.Contains(desc, want) {
					t.Fatalf("%s description = %q, want substring %q", tt.name, desc, want)
				}
			}
		})
	}
}

func TestBusRegisterDescriptionDiscouragesUnlockOnlyRegistration(t *testing.T) {
	var desc string
	for _, tool := range tools {
		if tool.Name == "bus_register" {
			desc = tool.Description
			break
		}
	}
	if desc == "" {
		t.Fatal("bus_register tool not registered")
	}
	for _, want := range []string{
		"REQUIRED FIRST CALL for aimebu message-bus work",
		"do not register solely to unlock another bus tool",
		"bus_recall",
		"bus_memory_list",
		"register only when the user's task is actually about collaborating on the aimebu message bus",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("bus_register description = %q, want substring %q", desc, want)
		}
	}
}

func TestToolsListReturnsFullSetBeforeRegister(t *testing.T) {
	resp := handle(&client.Client{}, request{
		JSONRPC: "2.0",
		Method:  "tools/list",
		ID:      json.RawMessage(`1`),
	}, nil)
	if resp == nil || resp.Error != nil {
		t.Fatalf("response = %+v, want successful tools/list", resp)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %T, want map", resp.Result)
	}
	listed, ok := result["tools"].([]tool)
	if !ok {
		t.Fatalf("tools = %T, want []tool", result["tools"])
	}

	seen := map[string]bool{}
	for _, tool := range listed {
		seen[tool.Name] = true
	}
	for _, want := range []string{"bus_register", "bus_agents", "bus_recall", "bus_memory_list", "bus_say"} {
		if !seen[want] {
			t.Fatalf("tools/list before register missing %s; got %v", want, seen)
		}
	}
}

// TestMCP_InitializeReturnsOverriddenEtiquette proves that handle("initialize")
// reads the bus_etiquette prompt from the server rather than the compiled
// constant. This is the end-to-end wiring test: store override → fetchPrompts
// → promptVal → instructions field in the initialize response.
func TestMCP_InitializeReturnsOverriddenEtiquette(t *testing.T) {
	const overrideBody = "custom etiquette for testing"

	// Serve a fake /settings/prompts that returns an override for bus_etiquette.
	overrideEntries := []map[string]any{
		{"key": "bus_etiquette", "body": overrideBody, "overridden": true},
	}
	overrideJSON, _ := json.Marshal(overrideEntries)

	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/settings/prompts" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(overrideJSON)
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL}
	resp := handle(c, request{
		JSONRPC: "2.0",
		Method:  "initialize",
		ID:      json.RawMessage(`1`),
	}, nil)

	if resp == nil {
		t.Fatal("handle returned nil for initialize")
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	instructions, ok := result["instructions"].(string)
	if !ok {
		t.Fatalf("instructions is not a string: %T", result["instructions"])
	}
	if instructions != overrideBody {
		t.Fatalf("instructions = %q, want override %q", instructions, overrideBody)
	}
}

func TestMCP_JSONRPCToolRoundTrip(t *testing.T) {
	t.Parallel()

	var calls []string
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agents":
			var got struct {
				Kind    string            `json:"kind"`
				Model   string            `json:"model"`
				Harness string            `json:"harness"`
				Project string            `json:"project"`
				Meta    map[string]string `json:"meta"`
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Kind != "ai" || got.Model != "gpt5" || got.Harness != "codex" || got.Meta["spawn_tag"] != "0123456789abcdef" {
				t.Fatalf("register body = %+v", got)
			}
			io.WriteString(w, `{"id":"alice@aimebu","name":"alice","kind":"ai","model":"gpt5","harness":"codex","project":"aimebu","reclaimed":false}`)
		case r.Method == http.MethodPost && r.URL.Path == "/rooms/general/join":
			io.WriteString(w, `{"id":"general","members":["alice@aimebu"],"roles":{}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/rooms/general/send":
			var got struct {
				From string `json:"from"`
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.From != "alice@aimebu" || got.Body != "hello" {
				t.Fatalf("send body = %+v", got)
			}
			io.WriteString(w, `{"id":42,"room":"general"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/agents/alice@aimebu/wait":
			if r.URL.Query().Get("timeout") != "1" {
				t.Fatalf("wait timeout = %q, want 1", r.URL.Query().Get("timeout"))
			}
			io.WriteString(w, `{"messages":[{"id":42,"room_id":"general","from":"bob","from_kind":"human","body":"hello back"}],"agent":"alice@aimebu"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/messages/42/reactions":
			var got map[string]string
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got["agent_id"] != "alice@aimebu" || got["emoji"] != "👍" {
				t.Fatalf("reaction body = %v", got)
			}
			io.WriteString(w, `{"message_id":42,"room":"general","reactions":[{"emoji":"👍","count":1,"me":true}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/memory":
			if r.URL.Query().Get("agent_id") != "alice@aimebu" {
				t.Fatalf("memory agent_id = %q", r.URL.Query().Get("agent_id"))
			}
			io.WriteString(w, `{"records":[{"id":"mem1","scope":"agent_shared_notes","scope_key":"global","body":"note","version":1,"author":"alice@aimebu"}],"usage":[],"rendered":"note"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/memory":
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got["agent_id"] != "alice@aimebu" || got["scope"] != "agent_shared_notes" || got["body"] != "note" {
				t.Fatalf("memory add body = %v", got)
			}
			io.WriteString(w, `{"record":{"id":"mem1","scope":"agent_shared_notes","scope_key":"global","body":"note","version":1,"author":"alice@aimebu"}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/memory/mem1":
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got["agent_id"] != "alice@aimebu" || got["version"].(float64) != 1 || got["body"] != "edited" {
				t.Fatalf("memory update body = %v", got)
			}
			io.WriteString(w, `{"record":{"id":"mem1","body":"edited","version":2}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/memory/mem1":
			if r.URL.Query().Get("agent_id") != "alice@aimebu" || r.URL.Query().Get("version") != "2" {
				t.Fatalf("memory delete query = %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"record":{"id":"mem1","body":"edited","version":2}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/recall":
			if r.URL.Query().Get("agent_id") != "alice@aimebu" || r.URL.Query().Get("query") != "needle" {
				t.Fatalf("recall query = %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"results":[{"message_id":42,"room_id":"general","from":"bob","snippet":"needle","score":10}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/rooms/general/roles":
			var got map[string]string
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got["agent_id"] != "alice@aimebu" || got["role_key"] != "worker" {
				t.Fatalf("role assign body = %v", got)
			}
			io.WriteString(w, `{"id":"general","members":["alice@aimebu"],"roles":{"alice@aimebu":"worker"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/rooms/general/roles/alice@aimebu":
			io.WriteString(w, `{"role_key":"worker","label":"Worker","icon":"W","body":"do the work"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/agents/alice@aimebu/rooms":
			io.WriteString(w, `{"agent":"alice@aimebu","rooms":[{"id":"general","unread_count":0}]}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	t.Cleanup(fakeSrv.Close)

	c := &client.Client{BaseURL: fakeSrv.URL}
	registerText := requireMCPTextResult(t, callMCPToolForTest(t, c, 1, "bus_register", map[string]any{
		"model":   "gpt5",
		"harness": "codex",
		"meta": map[string]string{
			"project":   "aimebu",
			"spawn_tag": "0123456789abcdef",
		},
	}))
	if !strings.Contains(registerText, `"id":"alice@aimebu"`) || c.AgentID != "alice@aimebu" || c.AgentName != "alice" {
		t.Fatalf("register result=%q AgentID=%q AgentName=%q", registerText, c.AgentID, c.AgentName)
	}

	for _, step := range []struct {
		name string
		args any
		want string
	}{
		{name: "bus_join", args: map[string]any{"room": "general"}, want: `"id":"general"`},
		{name: "bus_say", args: map[string]any{"room": "general", "body": "hello"}, want: `"id":42`},
		{name: "bus_wait", args: map[string]any{"timeout": 1}, want: `"body":"hello back"`},
		{name: "bus_react", args: map[string]any{"message_id": 42, "emoji": "👍"}, want: `"message_id":42`},
		{name: "bus_memory_list", args: map[string]any{}, want: `"rendered":"note"`},
		{name: "bus_memory_add", args: map[string]any{"scope": "agent_shared_notes", "body": "note"}, want: `"id":"mem1"`},
		{name: "bus_memory_update", args: map[string]any{"id": "mem1", "version": 1, "body": "edited"}, want: `"version":2`},
		{name: "bus_memory_remove", args: map[string]any{"id": "mem1", "version": 2}, want: `"body":"edited"`},
		{name: "bus_recall", args: map[string]any{"query": "needle", "limit": 3}, want: `"message_id":42`},
		{name: "bus_role_assign", args: map[string]any{"room": "general", "agent_id": "alice@aimebu", "role_key": "worker"}, want: `"worker"`},
		{name: "bus_role_get", args: map[string]any{"room": "general"}, want: `"body":"do the work"`},
	} {
		t.Run(step.name, func(t *testing.T) {
			text := requireMCPTextResult(t, callMCPToolForTest(t, c, 2, step.name, step.args))
			if !strings.Contains(text, step.want) {
				t.Fatalf("%s result = %q, want substring %q", step.name, text, step.want)
			}
		})
	}

	for _, want := range []string{
		"POST /agents",
		"POST /rooms/general/join",
		"POST /rooms/general/send",
		"GET /agents/alice@aimebu/wait?timeout=1",
		"PUT /messages/42/reactions",
		"GET /memory?agent_id=alice%40aimebu",
		"POST /memory",
		"PUT /memory/mem1",
		"DELETE /memory/mem1?agent_id=alice%40aimebu&version=2",
		"GET /recall?agent_id=alice%40aimebu&limit=3&query=needle",
		"POST /rooms/general/roles",
		"GET /rooms/general/roles/alice@aimebu",
	} {
		found := false
		for _, call := range calls {
			if call == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing call %q in %v", want, calls)
		}
	}
}

func TestMCP_JSONRPCErrorPaths(t *testing.T) {
	t.Parallel()

	c := &client.Client{BaseURL: "http://127.0.0.1"}

	t.Run("tool before register returns MCP tool error", func(t *testing.T) {
		resp := callMCPToolForTest(t, c, 1, "bus_join", map[string]any{"room": "general"})
		if resp.Error != nil {
			t.Fatalf("unexpected JSON-RPC error: %+v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result = %T, want map", resp.Result)
		}
		if isError, _ := result["isError"].(bool); !isError {
			t.Fatalf("isError = %v, want true", result["isError"])
		}
		content, ok := result["content"].([]textContent)
		if !ok || len(content) != 1 {
			t.Fatalf("content = %#v, want one textContent", result["content"])
		}
		if !strings.Contains(content[0].Text, "Call `bus_register` first") {
			t.Fatalf("error text = %q", content[0].Text)
		}
		for _, want := range []string{
			"`bus_join` requires an aimebu bus identity",
			"then retry `bus_join`",
			"If you did not intend to use the aimebu message bus, do not call bus tools",
		} {
			if !strings.Contains(content[0].Text, want) {
				t.Fatalf("error text = %q, want substring %q", content[0].Text, want)
			}
		}
	})

	t.Run("not registered prompt override can name attempted tool", func(t *testing.T) {
		c := &client.Client{
			BaseURL: "http://127.0.0.1",
			Prompts: map[string]string{
				"error.not_registered": "custom {{tool}} message",
			},
		}
		resp := callMCPToolForTest(t, c, 4, "bus_recall", map[string]any{"query": "needle"})
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result = %T, want map", resp.Result)
		}
		content, ok := result["content"].([]textContent)
		if !ok || len(content) != 1 {
			t.Fatalf("content = %#v, want one textContent", result["content"])
		}
		if got := content[0].Text; !strings.Contains(got, "custom bus_recall message") {
			t.Fatalf("error text = %q, want custom prompt with tool name", got)
		}
	})

	t.Run("unknown method returns method not found", func(t *testing.T) {
		resp := handle(c, request{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "missing/method"}, nil)
		if resp == nil || resp.Error == nil {
			t.Fatalf("response = %+v, want JSON-RPC error", resp)
		}
		if resp.Error.Code != -32601 || !strings.Contains(resp.Error.Message, "method not found") {
			t.Fatalf("error = %+v, want method not found", resp.Error)
		}
	})

	t.Run("bad tool params returns invalid params", func(t *testing.T) {
		resp := handle(c, request{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`3`),
			Method:  "tools/call",
			Params:  json.RawMessage(`{"name":`),
		}, nil)
		if resp == nil || resp.Error == nil {
			t.Fatalf("response = %+v, want JSON-RPC error", resp)
		}
		if resp.Error.Code != -32602 || !strings.Contains(resp.Error.Message, "invalid params") {
			t.Fatalf("error = %+v, want invalid params", resp.Error)
		}
	})
}

// ── Roles MCP tool tests ──────────────────────────────────────────

func TestMCP_RoleAssign_PostsToServer(t *testing.T) {
	var gotPath, gotBody string
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := json.Marshal(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"testroom","members":["alice@aimebu"],"created_at":"2026-01-01T00:00:00Z","created_by":"test","roles":{"alice@aimebu":"worker"}}`))
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL, AgentID: "alice@aimebu"}
	args, _ := json.Marshal(map[string]string{
		"room":     "testroom",
		"agent_id": "alice@aimebu",
		"role_key": "worker",
	})
	result, err := handleToolCall(c, "bus_role_assign", args, nil)
	if err != nil {
		t.Fatalf("handleToolCall bus_role_assign: %v", err)
	}
	if gotPath != "/rooms/testroom/roles" {
		t.Fatalf("expected POST to /rooms/testroom/roles, got %s", gotPath)
	}
	_ = gotBody
	if result == "" {
		t.Fatal("expected non-empty result")
	}
}

func TestMCP_React_PutsAndDeletesReaction(t *testing.T) {
	var calls []struct {
		Method string
		Path   string
		Body   map[string]string
	}
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		calls = append(calls, struct {
			Method string
			Path   string
			Body   map[string]string
		}{Method: r.Method, Path: r.URL.Path, Body: body})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"message_id":42,"room":"general","reactions":[{"emoji":"👍","count":1,"me":true}]}`))
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL, AgentID: "alice@aimebu"}
	addArgs, _ := json.Marshal(map[string]any{"message_id": 42, "emoji": "👍"})
	if _, err := handleToolCall(c, "bus_react", addArgs, nil); err != nil {
		t.Fatalf("handleToolCall bus_react add: %v", err)
	}
	removeArgs, _ := json.Marshal(map[string]any{"message_id": 42, "emoji": "👍", "remove": true})
	if _, err := handleToolCall(c, "bus_react", removeArgs, nil); err != nil {
		t.Fatalf("handleToolCall bus_react remove: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].Method != http.MethodPut || calls[0].Path != "/messages/42/reactions" {
		t.Fatalf("add call = %s %s, want PUT /messages/42/reactions", calls[0].Method, calls[0].Path)
	}
	if calls[1].Method != http.MethodDelete || calls[1].Path != "/messages/42/reactions" {
		t.Fatalf("remove call = %s %s, want DELETE /messages/42/reactions", calls[1].Method, calls[1].Path)
	}
	for i, call := range calls {
		if call.Body["agent_id"] != "alice@aimebu" || call.Body["emoji"] != "👍" {
			t.Fatalf("call %d body = %v", i, call.Body)
		}
	}
}

func TestMCP_RoleGet_ReturnsRoleWhenAssigned(t *testing.T) {
	roleResp := `{"role_key":"worker","label":"Worker","icon":"🛠️","body":"do the work"}`
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(roleResp))
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL, AgentID: "alice@aimebu"}
	args, _ := json.Marshal(map[string]string{"room": "testroom"})
	result, err := handleToolCall(c, "bus_role_get", args, nil)
	if err != nil {
		t.Fatalf("handleToolCall bus_role_get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["role_key"] != "worker" {
		t.Fatalf("expected role_key=worker, got %v", got["role_key"])
	}
	if got["icon"] != "🛠️" {
		t.Fatalf("expected icon, got %v", got["icon"])
	}
}

func TestMCP_RoleGet_ReturnsEmptyWhenUnassigned(t *testing.T) {
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"role_key":"","label":"","body":""}`))
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL, AgentID: "alice@aimebu"}
	args, _ := json.Marshal(map[string]string{"room": "testroom"})
	result, err := handleToolCall(c, "bus_role_get", args, nil)
	if err != nil {
		t.Fatalf("handleToolCall bus_role_get: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["role_key"] != "" {
		t.Fatalf("expected empty role_key, got %v", got["role_key"])
	}
}

func TestMCP_BusSayIncludesUnreadFooter(t *testing.T) {
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rooms/general/send":
			w.Write([]byte(`{"id":42,"room":"general"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/agents/alice@aimebu/rooms":
			w.Write([]byte(`{"agent":"alice@aimebu","rooms":[{"id":"general","unread_count":2}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL, AgentID: "alice@aimebu", AgentName: "alice"}
	args, _ := json.Marshal(map[string]any{
		"room": "general",
		"body": "implementation note",
	})
	params, _ := json.Marshal(map[string]any{
		"name":      "bus_say",
		"arguments": json.RawMessage(args),
	})
	resp := handle(c, request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		ID:      json.RawMessage(`1`),
		Params:  params,
	}, nil)
	if resp == nil || resp.Error != nil {
		t.Fatalf("tools/call returned error: %+v", resp)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	content, ok := result["content"].([]textContent)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want one text item", result["content"])
	}
	if !strings.Contains(content[0].Text, "2 unread total") {
		t.Fatalf("bus_say response missing unread footer:\n%s", content[0].Text)
	}
}

func TestMCP_BusSayForwardsProposedAnswers(t *testing.T) {
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rooms/general/send":
			var got struct {
				From            string               `json:"from"`
				Body            string               `json:"body"`
				NeedsAttention  bool                 `json:"needs_attention"`
				ProposedAnswers []string             `json:"proposed_answers"`
				OpenQuestions   []types.OpenQuestion `json:"open_questions"`
				VisualPlan      []types.PlanBlock    `json:"visual_plan"`
				AppendixPages   []types.AppendixPage `json:"appendix_pages"`
				ReplyTo         int64                `json:"reply_to"`
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			want := []string{"Proceed", "Hold"}
			if got.From != "alice@aimebu" || got.Body != "@martin approve?" || !got.NeedsAttention {
				t.Fatalf("forwarded send = %+v", got)
			}
			if !reflect.DeepEqual(got.ProposedAnswers, want) {
				t.Fatalf("proposed_answers = %#v, want %#v", got.ProposedAnswers, want)
			}
			wantQuestions := []types.OpenQuestion{{Question: "Pick one", Description: "More context", Options: []string{"A", "B"}}}
			if !reflect.DeepEqual(got.OpenQuestions, wantQuestions) {
				t.Fatalf("open_questions = %#v, want %#v", got.OpenQuestions, wantQuestions)
			}
			if len(got.VisualPlan) != 1 || got.VisualPlan[0].Type != "markdown" {
				t.Fatalf("visual_plan = %#v, want markdown block", got.VisualPlan)
			}
			wantAppendix := []types.AppendixPage{{Title: "Full plan", Body: "Details"}}
			if !reflect.DeepEqual(got.AppendixPages, wantAppendix) {
				t.Fatalf("appendix_pages = %#v, want %#v", got.AppendixPages, wantAppendix)
			}
			if got.ReplyTo != 42 {
				t.Fatalf("reply_to = %d, want 42", got.ReplyTo)
			}
			w.Write([]byte(`{"id":1,"room":"general"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/agents/alice@aimebu/rooms":
			w.Write([]byte(`{"agent":"alice@aimebu","rooms":[]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL, AgentID: "alice@aimebu", AgentName: "alice"}
	args, _ := json.Marshal(map[string]any{
		"room":             "general",
		"body":             "@martin approve?",
		"needs_attention":  true,
		"proposed_answers": []string{"Proceed", "Hold"},
		"open_questions":   []types.OpenQuestion{{Question: "Pick one", Description: "More context", Options: []string{"A", "B"}}},
		"visual_plan":      []map[string]any{{"type": "markdown", "data": map[string]any{"text": "Summary"}}},
		"appendix_pages":   []types.AppendixPage{{Title: "Full plan", Body: "Details"}},
		"reply_to":         42,
	})
	if _, err := handleToolCall(c, "bus_say", args, nil); err != nil {
		t.Fatalf("handleToolCall bus_say: %v", err)
	}
}

func TestMCP_BusDMForwardsProposedAnswers(t *testing.T) {
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/dm":
			var got struct {
				From            string               `json:"from"`
				To              string               `json:"to"`
				Body            string               `json:"body"`
				ProposedAnswers []string             `json:"proposed_answers"`
				OpenQuestions   []types.OpenQuestion `json:"open_questions"`
				VisualPlan      []types.PlanBlock    `json:"visual_plan"`
				AppendixPages   []types.AppendixPage `json:"appendix_pages"`
				ReplyTo         int64                `json:"reply_to"`
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			want := []string{"Proceed", "Revise"}
			if got.From != "alice@aimebu" || got.To != "martin" || !reflect.DeepEqual(got.ProposedAnswers, want) {
				t.Fatalf("forwarded dm = %+v, want answers %#v", got, want)
			}
			wantQuestions := []types.OpenQuestion{{Question: "Pick one", Description: "More context", Options: []string{"A", "B"}}}
			if !reflect.DeepEqual(got.OpenQuestions, wantQuestions) {
				t.Fatalf("open_questions = %#v, want %#v", got.OpenQuestions, wantQuestions)
			}
			if len(got.VisualPlan) != 1 || got.VisualPlan[0].Type != "diagram" {
				t.Fatalf("visual_plan = %#v, want diagram block", got.VisualPlan)
			}
			wantAppendix := []types.AppendixPage{{Title: "Full plan", Body: "Details"}}
			if !reflect.DeepEqual(got.AppendixPages, wantAppendix) {
				t.Fatalf("appendix_pages = %#v, want %#v", got.AppendixPages, wantAppendix)
			}
			if got.ReplyTo != 7 {
				t.Fatalf("reply_to = %d, want 7", got.ReplyTo)
			}
			w.Write([]byte(`{"id":1,"room":"dm:alice@aimebu:martin"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/agents/alice@aimebu/rooms":
			w.Write([]byte(`{"agent":"alice@aimebu","rooms":[]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL, AgentID: "alice@aimebu", AgentName: "alice"}
	args, _ := json.Marshal(map[string]any{
		"to":               "martin",
		"body":             "approve?",
		"proposed_answers": []string{"Proceed", "Revise"},
		"open_questions":   []types.OpenQuestion{{Question: "Pick one", Description: "More context", Options: []string{"A", "B"}}},
		"visual_plan":      []map[string]any{{"type": "diagram", "data": map[string]any{"text": "graph TD; A-->B"}}},
		"appendix_pages":   []types.AppendixPage{{Title: "Full plan", Body: "Details"}},
		"reply_to":         7,
	})
	if _, err := handleToolCall(c, "bus_dm", args, nil); err != nil {
		t.Fatalf("handleToolCall bus_dm: %v", err)
	}
}

func TestMCP_JoinEnrichesWithYourRole(t *testing.T) {
	roleResp := `{"role_key":"leader","label":"Leader","icon":"👑","body":"lead the team"}`
	// The fake server must handle both /rooms/{room}/join and /rooms/{room}/roles/{agentID}
	joinResp := `{"id":"testroom","members":["alice@aimebu"],"created_at":"2026-01-01T00:00:00Z","created_by":"test","roles":{"alice@aimebu":"leader"}}`
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			w.Write([]byte(roleResp))
		} else {
			w.Write([]byte(joinResp))
		}
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL, AgentID: "alice@aimebu"}
	args, _ := json.Marshal(map[string]string{"room": "testroom"})
	result, err := handleToolCall(c, "bus_join", args, nil)
	if err != nil {
		t.Fatalf("handleToolCall bus_join: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	yourRole, ok := got["your_role"].(map[string]any)
	if !ok {
		t.Fatalf("expected your_role in join response, got %T: %v", got["your_role"], got)
	}
	if yourRole["key"] != "leader" {
		t.Fatalf("your_role.key = %v, want leader", yourRole["key"])
	}
	if yourRole["body"] != "lead the team" {
		t.Fatalf("your_role.body = %v, want 'lead the team'", yourRole["body"])
	}
	if yourRole["icon"] != "👑" {
		t.Fatalf("your_role.icon = %v, want icon", yourRole["icon"])
	}
}

func TestSessionHeartbeatFiresAndStopsOnClose(t *testing.T) {
	// Not parallel: shares mcpHeartbeatInterval with the other heartbeat test.
	var mu sync.Mutex
	var hbCount int
	var hbIDs []string
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/heartbeat") {
			parts := strings.Split(r.URL.Path, "/")
			mu.Lock()
			hbCount++
			if len(parts) >= 3 {
				hbIDs = append(hbIDs, parts[len(parts)-2])
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"agent":"alice@aimebu"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL}
	var agentID atomic.Pointer[string]
	id := "alice@aimebu"
	agentID.Store(&id)

	done := make(chan struct{})
	startSessionHeartbeat(done, &agentID, c, 20*time.Millisecond)

	// Allow at least 2 ticks.
	time.Sleep(60 * time.Millisecond)
	close(done)
	// Brief grace period to let the goroutine exit cleanly.
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	count := hbCount
	ids := append([]string(nil), hbIDs...)
	mu.Unlock()

	if count < 2 {
		t.Fatalf("expected >=2 heartbeats, got %d", count)
	}
	for _, id := range ids {
		if id != "alice@aimebu" {
			t.Fatalf("heartbeat for wrong agent %q, want alice@aimebu", id)
		}
	}

	// After done is closed, count must not increase.
	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	after := hbCount
	mu.Unlock()
	if after != count {
		t.Fatalf("heartbeat continued after done closed: got %d (was %d)", after, count)
	}
}

func TestSessionHeartbeatSkipsWhenNoAgentID(t *testing.T) {
	// Not parallel: shares mcpHeartbeatInterval with the other heartbeat test.
	var mu sync.Mutex
	var hbCount int
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			mu.Lock()
			hbCount++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL}
	var agentID atomic.Pointer[string] // nil → skips heartbeat

	done := make(chan struct{})
	startSessionHeartbeat(done, &agentID, c, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	close(done)
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	count := hbCount
	mu.Unlock()
	if count != 0 {
		t.Fatalf("expected 0 heartbeats before registration, got %d", count)
	}
}

// TestSessionHeartbeatConcurrentRegisterIsRaceFree verifies that concurrent
// atomic stores (simulating bus_register) and goroutine loads are race-free
// under -race. Without the atomic.Pointer guard this test would race.
func TestSessionHeartbeatConcurrentRegisterIsRaceFree(t *testing.T) {
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL}
	var agentID atomic.Pointer[string]

	done := make(chan struct{})
	startSessionHeartbeat(done, &agentID, c, 5*time.Millisecond)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("agent%d@test", n)
			agentID.Store(&id)
		}(i)
	}
	wg.Wait()
	time.Sleep(30 * time.Millisecond)
	close(done)
	time.Sleep(10 * time.Millisecond)
}
