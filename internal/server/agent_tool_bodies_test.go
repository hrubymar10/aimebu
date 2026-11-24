package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
)

// toolBodyRoute is a route an agent tool sends a JSON body to: the handler registered for it in Handler and the type that handler decodes
// the body into, with readJSON (strictly: an unknown field is 400 invalid_request).
type toolBodyRoute struct {
	handler string
	request func() any
}

// toolBodyRoutes lists every route that internal/agenttools sends a JSON body to. A new tool that posts a body, or a tool that moves to
// another route, fails TestAgentToolBodiesDecodeStrictly until it is listed here with the type its handler decodes.
var toolBodyRoutes = map[string]toolBodyRoute{
	"POST /api/agent/recoveries/{item}/decision": {"agentToolResolveRecovery", func() any { return new(agentResolveRecoveryRequest) }},
	"POST /api/agent/rooms/{room}/schedules":     {"agentToolSchedules", func() any { return new(scheduleRequest) }},
	"POST /api/agent/commands":                   {"agentToolCommandRun", func() any { return new(commandRunRequest) }},
	"POST /api/agent/rooms":                      {"agentToolCreateRoom", func() any { return new(agentCreateRoomRequest) }},
	"POST /api/agent/dms":                        {"agentToolDM", func() any { return new(agentDMRequest) }},
	"POST /api/agent/human-dms":                  {"agentToolDMHuman", func() any { return new(agentHumanDMRequest) }},
	"POST /api/agent/rooms/{room}/close":         {"agentToolCloseRoom", func() any { return new(agentCloseRoomRequest) }},
	"PUT /api/agent/handbook":                    {"agentHandbook", func() any { return new(agentHandbookUpdateRequest) }},
	"POST /api/agent/memory":                     {"agentToolMemoryAdd", func() any { return new(agentMemoryAddRequest) }},
	"PUT /api/agent/memory/{id}":                 {"agentToolMemoryRecord", func() any { return new(agentMemoryRecordRequest) }},
	"POST /api/agent/agents":                     {"agentToolSpawn", func() any { return new(agentSpawnRequest) }},
	"POST /api/agent/agents/{id}/stop":           {"agentToolStop", func() any { return new(agentStopRequest) }},
	"POST /api/agent/agents/{id}/role":           {"agentToolSetRole", func() any { return new(agentSetRoleRequest) }},
}

// toolBodyUnread lists the routes a tool sends a body to that the handler never reads: cancel_command sends {} and the cancel handler
// takes nothing from it, so no field can drift there.
var toolBodyUnread = map[string]string{
	"POST /api/agent/commands/{id}/cancel": "agentToolCommandCancel",
}

// toolArgOverrides are the values the tools' own checks require (an ID prefix, one of a few scopes); any other property gets a plausible
// value of its schema type.
var toolArgOverrides = map[string]any{
	"room_id": "room_bodytest", "parent_id": "room_parent", "item_id": "inb_bodytest", "job_id": "cmd_bodytest", "schedule_id": "sch_bodytest",
	"agent_id": "agt_bodytest", "target_agent_id": "agt_bodytest", "id": "knw_bodytest", "attachment_id": "att_bodytest",
	"message_id": "msg_bodytest", "scope": "project", "emoji": "👍", "cmd": "true",
}

// toolArgVariants splits the arguments of a tool whose properties exclude each other into several calls; each call still sets every
// other property.
var toolArgVariants = map[string][][]string{
	"handbook_update": {{"patch"}, {"content"}}, // exactly one of content or patch: each variant leaves one out
}

// capturedToolRequest is one request a tool sent, with the route pattern it matched.
type capturedToolRequest struct {
	tool    string
	pattern string
	body    []byte
}

// TestAgentToolBodiesDecodeStrictly is the guard against a field that one side of the agent toolbox knows and the other does not: every
// tool of internal/agenttools is called with every property of its input schema set, and each JSON body it sends must decode, through the
// server's own readJSON (unknown fields refused), into the request type of the route it reaches. A field the tool sends that the type lacks
// fails it, and so does a field of the type that no tool call ever sends. Routes the tools reach are matched by the same patterns Handler
// registers, and each pattern's registration is checked against server.go.
func TestAgentToolBodiesDecodeStrictly(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var captured []capturedToolRequest
	var current string
	mux := http.NewServeMux()
	record := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedToolRequest{tool: current, pattern: r.Pattern, body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}
	for pattern := range toolBodyRoutes {
		mux.HandleFunc(pattern, record)
	}
	for pattern := range toolBodyUnread {
		mux.HandleFunc(pattern, record)
	}
	mux.HandleFunc("/", record) // every other request: a read, or a body to a route not listed above
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	toolbox := agenttools.Server{URL: fake.URL, Key: "aak_" + strings.Repeat("x", 43), Workdir: t.TempDir()}

	listed, _ := toolbox.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	var catalog struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]struct {
						Type string   `json:"type"`
						Enum []string `json:"enum"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listed, &catalog); err != nil || len(catalog.Result.Tools) == 0 {
		t.Fatalf("tools/list: %s %v", listed, err)
	}
	for _, tool := range catalog.Result.Tools {
		variants := toolArgVariants[tool.Name]
		if variants == nil {
			variants = [][]string{nil}
		}
		for _, leaveOut := range variants {
			args := map[string]any{}
			for name, property := range tool.InputSchema.Properties {
				if slices.Contains(leaveOut, name) {
					continue
				}
				switch {
				case toolArgOverrides[name] != nil:
					args[name] = toolArgOverrides[name]
				case len(property.Enum) > 0:
					args[name] = property.Enum[0]
				case property.Type == "integer":
					args[name] = 1
				case property.Type == "boolean":
					args[name] = true
				default:
					args[name] = "body-test-" + name
				}
			}
			call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool.Name, "arguments": args}})
			mu.Lock()
			current = tool.Name
			before := len(captured)
			mu.Unlock()
			answer, _ := toolbox.Process(t.Context(), call)
			mu.Lock()
			sent := len(captured) - before
			mu.Unlock()
			if sent == 0 {
				t.Errorf("%s %v sent no request (its own checks refused the arguments?): %s", tool.Name, args, answer)
			}
		}
	}

	seenFields := map[string]map[string]bool{}
	for _, request := range captured {
		if len(bytes.TrimSpace(request.body)) == 0 {
			continue
		}
		if _, ok := toolBodyUnread[request.pattern]; ok {
			continue
		}
		route, ok := toolBodyRoutes[request.pattern]
		if !ok {
			t.Errorf("%s sent a body to a route this test does not know (pattern %q): add it to toolBodyRoutes with its handler's request type: %s", request.tool, request.pattern, request.body)
			continue
		}
		// readJSON is the decoder every one of these handlers uses: strict, so an unknown field is 400 invalid_request.
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(request.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		if !readJSON(w, req, route.request()) {
			t.Errorf("%s: its body does not decode into %s's request type (%T): %d %s\nbody: %s", request.tool, route.handler, route.request(), w.Code, w.Body, request.body)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(request.body, &fields); err != nil {
			t.Errorf("%s: body is not a JSON object: %s", request.tool, request.body)
			continue
		}
		if seenFields[request.pattern] == nil {
			seenFields[request.pattern] = map[string]bool{}
		}
		for name := range fields {
			seenFields[request.pattern][name] = true
		}
	}

	// The other direction: a field of a request type that no tool sends is one the tool lost or never learned.
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	patterns := make([]string, 0, len(toolBodyRoutes))
	for pattern := range toolBodyRoutes {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	for _, pattern := range patterns {
		route := toolBodyRoutes[pattern]
		if !strings.Contains(string(source), `mux.Handle("`+pattern+`", s.requireAgentTool(http.HandlerFunc(s.`+route.handler+`)))`) {
			t.Errorf("server.go does not route %q to %s", pattern, route.handler)
		}
		seen := seenFields[pattern]
		if seen == nil {
			t.Errorf("no tool sent a body to %q: drop it from toolBodyRoutes or find the tool that should", pattern)
			continue
		}
		kind := reflect.TypeOf(route.request()).Elem()
		for i := range kind.NumField() {
			name, _, _ := strings.Cut(kind.Field(i).Tag.Get("json"), ",")
			if name != "" && name != "-" && !seen[name] {
				t.Errorf("%s (%s) has field %q that no tool sends", kind.Name(), pattern, name)
			}
		}
	}
	for pattern, handler := range toolBodyUnread {
		if !strings.Contains(string(source), `mux.Handle("`+pattern+`", s.requireAgentTool(http.HandlerFunc(s.`+handler+`)))`) {
			t.Errorf("server.go does not route %q to %s", pattern, handler)
		}
	}
}
