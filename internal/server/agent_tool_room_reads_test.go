package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// toolRoomReads classifies every route an agent's toolbox can reach (each MCP tool is a call to one of them) by whether its answer carries
// content of a room. The conversation log records tool results, so a route whose answer carries room content must note those rooms
// against the caller's turn (recordToolRoomReads) before it answers; Store.TurnWithheldFrom then withholds the turn from a log reader who
// may not read one of them. TestEveryAgentToolRouteIsClassifiedForRoomReads fails for a route missing here, so a new tool cannot leak room
// content into the log unnoticed: add it as recordsRoomReads (and call recordToolRoomReads in its handler) or as noRoomContent with the
// reason. See docs/supervisor.md, "Private chats" under the conversation log.
const (
	recordsRoomReads = "records room reads"
	noRoomContent    = "returns no room content"
)

var toolRoomReads = map[string]struct{ class, why string }{
	"GET /api/agent/message-numbers/{no}":                    {recordsRoomReads, "message_get: a message of any room the agent may read"},
	"GET /api/agent/rooms/{room}/history":                    {recordsRoomReads, "room_history: the room's messages"},
	"GET /api/agent/recall":                                  {recordsRoomReads, "memory_recall: message and task-note hits of the agent's rooms"},
	"GET /api/agent/memory":                                  {recordsRoomReads, "memory_list: a task room's notes are that room's content"},
	"GET /api/agent/rooms/{room}/schedules":                  {recordsRoomReads, "list_schedules: what a schedule posts into its room"},
	"GET /api/agent/recoveries":                              {recordsRoomReads, "list_recoveries: the evidence is what a failed turn printed in its room"},
	"GET /api/agent/rooms":                                   {noRoomContent, "list_rooms: ids and titles, which every turn's context already lists"},
	"GET /api/agent/agents":                                  {noRoomContent, "list_agents: agent cards"},
	"GET /api/agent/macros":                                  {noRoomContent, "list_macros: project macros"},
	"GET /api/agent/usage-left":                              {noRoomContent, "usage_left: quotas"},
	"GET /api/agent/handbook":                                {noRoomContent, "handbook_get: the project handbook, not a room's"},
	"PUT /api/agent/handbook":                                {noRoomContent, "handbook_update: returns the handbook"},
	"GET /api/agent/commands":                                {noRoomContent, "list_commands: the agent's own shell jobs"},
	"GET /api/agent/commands/{id}/log":                       {noRoomContent, "command_log: output of the agent's own shell job"},
	"POST /api/agent/commands":                               {noRoomContent, "run_command: the job"},
	"POST /api/agent/commands/{id}/cancel":                   {noRoomContent, "cancel_command: the job"},
	"POST /api/agent/memory":                                 {noRoomContent, "memory_add: echoes the record the agent wrote"},
	"PUT /api/agent/memory/{id}":                             {noRoomContent, "memory_update: echoes the record the agent wrote"},
	"DELETE /api/agent/memory/{id}":                          {noRoomContent, "memory_remove: the removed record"},
	"POST /api/agent/recoveries/{item}/decision":             {noRoomContent, "resolve_recovery: the decision"},
	"POST /api/agent/rooms/{room}/schedules":                 {noRoomContent, "create_schedule: echoes what the agent wrote"},
	"DELETE /api/agent/rooms/{room}/schedules/{id}":          {noRoomContent, "delete_schedule: the id"},
	"POST /api/agent/dms":                                    {noRoomContent, "dm_agent: ids of what the agent sent"},
	"POST /api/agent/human-dms":                              {noRoomContent, "dm_human: ids of what the agent sent"},
	"POST /api/agent/rooms":                                  {noRoomContent, "create_room: the new room"},
	"POST /api/agent/rooms/{room}/close":                     {noRoomContent, "close_room: the room's state"},
	"POST /api/agent/agents":                                 {noRoomContent, "spawn_agent: the agent card"},
	"POST /api/agent/agents/{id}/stop":                       {noRoomContent, "stop_agent: the agent card"},
	"POST /api/agent/agents/{id}/role":                       {noRoomContent, "set_role: the agent card"},
	"PUT /api/agent/messages/{message}/reactions/{emoji}":    {noRoomContent, "react: 204"},
	"DELETE /api/agent/messages/{message}/reactions/{emoji}": {noRoomContent, "react: 204"},
	"GET /api/agent/attachments/{attachment}":                {noRoomContent, "attachment_get: only an attachment of a note in the live turn, whose room the turn already served"},
	"POST /api/agent/attachments/{attachment}/save":          {noRoomContent, "attachment_get: same rule; answers a path"},
	"GET /api/agent/turns/{turn}/attachments/{attachment}":   {noRoomContent, "attachment by turn capability: same rule as attachment_get"},
	"POST /api/agent/mcp":                                    {noRoomContent, "the MCP endpoint answers through the routes above, which record for themselves"},
}

// agentToolRoutes reads the package's route registrations (mux.Handle and mux.HandleFunc calls) and returns, for every route under
// /api/agent/ or behind an agent-key guard, the name of the Server method that handles it.
func agentToolRoutes(t *testing.T) (map[string]string, map[string]*ast.FuncDecl) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	routes := map[string]string{}
	methods := map[string]*ast.FuncDecl{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Body != nil {
				methods[fn.Name.Name] = fn
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			var handlers []string
			guarded := false
			ast.Inspect(call.Args[1], func(node ast.Node) bool {
				if sel, ok := node.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "s" {
						if strings.HasPrefix(sel.Sel.Name, "requireAgentTool") {
							guarded = true
						} else {
							handlers = append(handlers, sel.Sel.Name)
						}
					}
				}
				return true
			})
			if !guarded && !strings.Contains(pattern, " /api/agent/") {
				return true
			}
			if len(handlers) != 1 {
				t.Fatalf("%s: cannot tell its handler from %v", pattern, handlers)
			}
			routes[pattern] = handlers[0]
			return true
		})
	}
	return routes, methods
}

// Every route an agent's toolbox reaches is classified in toolRoomReads, and every route classified as carrying room content notes the
// rooms it read (recordToolRoomReads) in its handler.
func TestEveryAgentToolRouteIsClassifiedForRoomReads(t *testing.T) {
	routes, methods := agentToolRoutes(t)
	if len(routes) < 30 {
		t.Fatalf("found only %d agent routes; the route table was not read: %v", len(routes), routes)
	}
	patterns := make([]string, 0, len(routes))
	for pattern := range routes {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	for _, pattern := range patterns {
		entry, ok := toolRoomReads[pattern]
		if !ok {
			t.Errorf("%s (%s) is not classified in toolRoomReads: add it as %q, calling recordToolRoomReads before it answers, or as %q with the reason",
				pattern, routes[pattern], recordsRoomReads, noRoomContent)
			continue
		}
		if entry.class != recordsRoomReads && entry.class != noRoomContent || entry.why == "" {
			t.Errorf("%s: class %q, reason %q", pattern, entry.class, entry.why)
			continue
		}
		if entry.class != recordsRoomReads {
			continue
		}
		method, ok := methods[routes[pattern]]
		if !ok {
			t.Errorf("%s: handler %s not found", pattern, routes[pattern])
			continue
		}
		records := false
		ast.Inspect(method.Body, func(node ast.Node) bool {
			if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "recordToolRoomReads" {
				records = true
			}
			return !records
		})
		if !records {
			t.Errorf("%s carries room content but its handler %s does not call recordToolRoomReads", pattern, routes[pattern])
		}
	}
	for pattern := range toolRoomReads {
		if _, ok := routes[pattern]; !ok {
			t.Errorf("toolRoomReads names %s, which is not a route any more", pattern)
		}
	}
}
