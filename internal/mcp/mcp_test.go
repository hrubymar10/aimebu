package mcp

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
	"github.com/hrubymar10/aimebu/internal/types"
)

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
	})

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
	result, err := handleToolCall(c, "bus_role_assign", args)
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
	if _, err := handleToolCall(c, "bus_react", addArgs); err != nil {
		t.Fatalf("handleToolCall bus_react add: %v", err)
	}
	removeArgs, _ := json.Marshal(map[string]any{"message_id": 42, "emoji": "👍", "remove": true})
	if _, err := handleToolCall(c, "bus_react", removeArgs); err != nil {
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
	result, err := handleToolCall(c, "bus_role_get", args)
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
	result, err := handleToolCall(c, "bus_role_get", args)
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
	})
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
	})
	if _, err := handleToolCall(c, "bus_say", args); err != nil {
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
	})
	if _, err := handleToolCall(c, "bus_dm", args); err != nil {
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
	result, err := handleToolCall(c, "bus_join", args)
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
