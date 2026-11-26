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
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
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
		if tool.Name == "submit_answer" {
			// This tool validates its envelope locally and sends no HTTP request.
			continue
		}
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

// launcherMachine is a machine conn that supports launchers and lists the given
// launchers as available, so a spawn resolves the launcher_id it is given
// instead of falling back to the variant name. It answers the folder check a
// launcher spawn makes through the supervisor: every folder as it is spelled.
func launcherMachine(t *testing.T, f *fixture, launchers ...store.Launcher) {
	t.Helper()
	rows := make([]protocol.LauncherRow, 0, len(launchers))
	capabilities := make([]protocol.CapabilityReport, 0, len(launchers))
	for _, launcher := range launchers {
		rows = append(rows, protocol.LauncherRow{LauncherDefinition: protocol.LauncherDefinition{ID: launcher.ID, Tool: launcher.Tool, Name: launcher.Name, Enabled: true}, Available: true})
		capabilities = append(capabilities, protocol.CapabilityReport{Tool: launcher.Tool, Environment: adapter.Environment{Variant: launcherAlias(launcher.Name)}})
	}
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 16), done: make(chan struct{}), bound: map[string]int{},
		supportsLaunchers: true, supportsPathProbe: true, launchers: rows, capabilities: capabilities}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	t.Cleanup(func() { close(machine.done) })
	go func() {
		for {
			select {
			case request := <-machine.sendQueue:
				if request.Type != protocol.PathProbe {
					continue
				}
				machine.harnessMu.Lock()
				waiter := machine.pathPending[request.ReqID]
				machine.harnessMu.Unlock()
				if waiter != nil {
					waiter <- protocol.Frame{V: protocol.Version, Type: protocol.PathResult, ReqID: request.ReqID, CanonicalPath: request.CheckoutPath}
				}
			case <-machine.done:
				return
			}
		}
	}()
}

// TestAgentToolSpawnLauncherID covers spawn_agent's launcher_id: the tool
// forwards it and the route validates it like the person's spawn route (an
// unknown launcher is a 4xx), a launcher outside the role's default pool is
// gated like tool, variant and model (409 model_not_approved unless
// approved_by, whose message must name the requested model and may not name a
// different tool or variant), and a role_key-only spawn uses the pool entry's
// launcher.
func TestAgentToolSpawnLauncherID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	_ = lead
	codexNative := f.harness(t, "codex", "native")
	claudeNative := f.harness(t, "claude", "native")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE machine_launcher SET enabled=1 WHERE id IN (?,?)`, codexNative.ID, claudeNative.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE role SET spawn_defaults_json=? WHERE role_key='worker'`,
			`[{"launcher_id":"`+codexNative.ID+`","model":"gpt-6-sol"}]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	launcherMachine(t, f, codexNative, claudeNative)

	args := func(requestKey string, choice map[string]any) map[string]any {
		out := map[string]any{"room_id": f.room.ID, "role_key": "worker", "reason": "the task asked for it", "request_key": requestKey}
		for name, value := range choice {
			out[name] = value
		}
		return out
	}

	// A role_key-only spawn runs on the pool entry's launcher.
	card := spawnViaTool(t, f, key, args("lnch-default", nil))
	if !card.DefaultUsed || card.LauncherID != codexNative.ID || card.Model != "gpt-6-sol" {
		t.Fatalf("pool default spawn: %+v", card)
	}

	// An explicit launcher_id is forwarded: tool and model with the pool's
	// launcher but no variant spawns on it (without the forwarded launcher_id
	// the request is refused for a missing variant). The choice matches the
	// pool entry, so the card says the default was used.
	card = spawnViaTool(t, f, key, args("lnch-forward", map[string]any{"tool": "codex", "launcher_id": codexNative.ID, "model": "gpt-6-sol"}))
	if card.LauncherID != codexNative.ID || card.Model != "gpt-6-sol" || card.Variant != "native" {
		t.Fatalf("explicit launcher spawn: %+v", card)
	}

	// A launcher outside the pool is gated: no approval, no spawn, even though
	// the tool, variant and model name the pool entry.
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("lnch-gate", map[string]any{"tool": "codex", "variant": "native", "model": "gpt-6-sol", "launcher_id": claudeNative.ID})); !failed ||
		!strings.Contains(text, "model_not_approved") {
		t.Fatalf("spawn on a launcher outside the pool: failed=%v %s", failed, text)
	}

	// A person's message that names the requested model opens the gate, and
	// the spawn then runs on the launcher it names; one that names another
	// variant does not.
	approvedID, _ := personMessage(t, f, f.room.ID, "haiku is fine for this task")
	card = spawnViaTool(t, f, key, args("lnch-approved", map[string]any{"tool": "claude", "variant": "native", "model": "haiku", "launcher_id": claudeNative.ID, "approved_by": approvedID}))
	if card.LauncherID != claudeNative.ID || card.Model != "haiku" {
		t.Fatalf("approved launcher spawn: %+v", card)
	}
	wrongVariantID, _ := personMessage(t, f, f.room.ID, "haiku on docker is fine")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("lnch-wrong-variant", map[string]any{"tool": "claude", "variant": "native", "model": "haiku", "launcher_id": claudeNative.ID, "approved_by": wrongVariantID})); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "tool or variant") {
		t.Fatalf("spawn approved with a wrong variant named: failed=%v %s", failed, text)
	}

	// An unknown launcher is refused with the person route's answer. On the
	// agent route the gate refuses it first without an approval; with one that
	// names the requested model, the launcher lookup itself fails.
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("lnch-unknown-gate", map[string]any{"tool": "codex", "variant": "native", "model": "gpt-6-sol", "launcher_id": "lnch_missing"})); !failed ||
		!strings.Contains(text, "model_not_approved") {
		t.Fatalf("unknown launcher without approval: failed=%v %s", failed, text)
	}
	unknownApproved, _ := personMessage(t, f, f.room.ID, "gpt-6-sol is approved")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("lnch-unknown", map[string]any{"tool": "codex", "variant": "native", "model": "gpt-6-sol", "launcher_id": "lnch_missing", "approved_by": unknownApproved})); !failed ||
		!strings.Contains(text, "HTTP 409") || !strings.Contains(text, "launcher_unavailable") {
		t.Fatalf("unknown launcher with approval: failed=%v %s", failed, text)
	}
	personReq, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents",
		strings.NewReader(`{"role_key":"worker","tool":"codex","variant":"native","model":"gpt-6-sol","launcher_id":"lnch_missing"}`))
	personReq.Header.Set("Content-Type", "application/json")
	personReq.Header.Set("X-CSRF-Token", f.csrf)
	personReq.Header.Set("Idempotency-Key", "lnch-person-unknown")
	personReq.AddCookie(f.cookie)
	personResp, err := http.DefaultClient.Do(personReq)
	if err != nil {
		t.Fatal(err)
	}
	defer personResp.Body.Close()
	if personResp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(personResp.Body)
		t.Fatalf("person route unknown launcher: %d %s", personResp.StatusCode, body)
	}
}
