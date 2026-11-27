package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// The cross-project authorization test. A person who belongs to project P1 only, and an agent of P1, call every route of the server
// with the ids of objects of project P2 (its project, room, agent, message, attachment, inbox item, command, memory, schedule, ...), and
// each answer must be a refusal (403 or 404) that carries nothing of P2.
//
// The routes come from the server's own registrations: every mux.Handle / mux.HandleFunc call of the package's non-test sources is read
// from the syntax tree, so a route added later is picked up without touching this file. A route whose path has an id is called with P2's
// object for that id (and, when it has a parent id too, once more with P1's own parent and P2's child). A route without an id in its
// path must be on crossProjectAllowlist with the reason it needs no such check; a route that is neither fails the test, so the check
// stays in force for the routes of the future. The agent MCP tools are checked the same way (see TestCrossProjectAgentMCPTools).
// CONTRIBUTING.md, "The cross-project authorization test", says what to do when this test fails for a new route.

const p2Mark = "P2MARK"

// crossProjectAllowlist is every route without an object id in its path (or an intentionally global one), with the reason it is not
// called with a P2 id. A route here is still called when it is a GET, with P2's ids in the query string, to see that its answer carries
// nothing of P2 (TestCrossProjectListsCarryNothingOfP2); the others are never called.
var crossProjectAllowlist = map[string]string{}

func allow(reason string, patterns ...string) {
	for _, pattern := range patterns {
		if _, dup := crossProjectAllowlist[pattern]; dup {
			panic("allowlisted twice: " + pattern)
		}
		crossProjectAllowlist[pattern] = reason
	}
}

func init() {
	allow("the page and static assets: no data of any project",
		"GET /")
	allow("before or without a session: a login, a setup step, an invite, or a probe of the caller's own session; nothing of a project is addressed",
		"POST /api/login", "POST /api/logout", "GET /api/session", "POST /api/setup", "GET /api/setup/status", "POST /api/setup/token",
		"POST /api/invites/inspect", "POST /api/invites/redeem", "GET /api/health", "GET /api/buildinfo")
	allow("the caller's own account, preferences and presence: scoped to the session's user, no id in the request",
		"GET /api/me", "GET /api/me/preferences", "PUT /api/me/preferences", "GET /api/me/telemetry", "PUT /api/me/telemetry",
		"POST /api/me/telemetry/fingerprint", "POST /api/presence", "POST /api/welcome/seen", "PUT /api/inline-plans",
		"POST /api/update-status/remind", "POST /api/update-status/skip", "GET /api/update-status")
	allow("lists and streams that are filtered by the caller's memberships in the handler; checked by TestCrossProjectListsCarryNothingOfP2 (the event stream is long-lived and is never called here)",
		"GET /api/projects", "GET /api/inbox", "GET /api/events", "GET /api/events/cursor", "GET /api/system-events", "GET /api/agent-dms",
		"GET /api/team-flows", "GET /api/team-flows/recommended-models", "GET /api/roles", "GET /api/users", "GET /api/macros",
		"GET /api/prompts", "GET /api/spawn-catalog", "GET /api/machines", "GET /api/machine", "GET /api/machines/directories",
		"GET /api/cron/preview", "GET /api/credentials/targets", "GET /api/usages", "GET /api/limit-guard", "GET /api/quota-switcher",
		"GET /api/switcher", "GET /api/sounds", "GET /api/usage-limit-default")
	allow("an instance-wide setting or action for the owner and global admins (or the project owner when it creates something of its own): the answer to anyone else is 403 by the handler's own guard, and none of it takes a project id in the path",
		"POST /api/projects", "POST /api/roles", "POST /api/cleanup", "GET /api/cleanup/log", "GET /api/cleanup/settings", "PUT /api/cleanup/settings",
		"GET /api/disk", "GET /api/retention", "PUT /api/retention", "PUT /api/macros", "PUT /api/usage-limit-default",
		"GET /api/agent-isolation", "PUT /api/agent-isolation", "GET /api/telemetry/settings", "PUT /api/telemetry/settings",
		"GET /api/settings-backup", "POST /api/settings-backup/apply", "POST /api/settings-backup/preview", "POST /api/local-supervisor/restart",
		"POST /api/invites", "POST /api/sounds", "PUT /api/switcher", "POST /api/usages/refresh", "POST /api/usages/providers",
		"POST /api/usages/settings", "POST /api/usages/mistral/config", "POST /api/usages/ollama/config", "POST /api/usages/copilot/login/start",
		"POST /api/usages/copilot/login/poll", "POST /api/usages/copilot/login/logout")
	allow("the doctor and backup routes: authorized by the instance's doctor key from the server's own host, never by a session, and they describe the instance (its database, its sessions, its launchers and models), not a project's objects",
		"GET /api/doctor", "GET /api/doctor/sessions", "GET /api/doctor/self-test", "GET /api/backup/database")
	allow("the v1 import and its status: owner-only, one instance-wide job, no project or room id",
		"POST /api/import-v1/commit", "POST /api/import-v1/dry-run", "GET /api/import-v1/preview", "POST /api/import-v1/skip", "GET /api/import-v1/status")
	allow("machine pairing: the redeem is authorized by a one-time code, the creation by an owner session; a machine is not a project's object",
		"POST /api/machine-pairings", "POST /api/machine-pairings/redeem")
	allow("pprof: gated by the debug key and a loopback check (TestDebugPprof...), and no project data",
		"GET /api/debug/pprof/", "GET /api/debug/pprof/cmdline", "GET /api/debug/pprof/profile", "GET /api/debug/pprof/symbol",
		"POST /api/debug/pprof/symbol", "GET /api/debug/pprof/trace")
	allow("the agent toolbox call that acts on the caller's own project and takes its ids from the body; its ID-taking tools are checked by TestCrossProjectAgentMCPTools",
		"GET /api/agent/agents", "POST /api/agent/agents", "GET /api/agent/commands", "POST /api/agent/commands", "POST /api/agent/dms",
		"GET /api/agent/handbook", "PUT /api/agent/handbook", "POST /api/agent/human-dms", "GET /api/agent/macros", "POST /api/agent/mcp",
		"GET /api/agent/memory", "POST /api/agent/memory", "GET /api/agent/recall", "GET /api/agent/recoveries", "GET /api/agent/rooms",
		"POST /api/agent/rooms", "GET /api/agent/usage-left")
}

// routePatterns reads the server's route registrations: the first argument of every mux.Handle and mux.HandleFunc call in the package's
// non-test sources. A registration whose pattern is not a string literal cannot be read this way and fails the test.
func routePatterns(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var patterns []string
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
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "mux" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("%s: a route is registered with a pattern that is not a string literal; this test cannot list it", fset.Position(call.Pos()))
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			patterns = append(patterns, pattern)
			return true
		})
	}
	sort.Strings(patterns)
	return patterns
}

func splitPattern(pattern string) (method, path string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return "GET", pattern
	}
	return method, path
}

func wildcardsOf(path string) []string {
	var names []string
	for _, segment := range strings.Split(path, "/") {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			names = append(names, strings.Trim(segment, "{}"))
		}
	}
	return names
}

// crossWorld is project P2 with everything in it, project P1, and the two callers that belong to P1 only.
type crossWorld struct {
	t    *testing.T
	f    *fixture
	p1   map[string]string // P1's own objects, by kind
	p2   map[string]string // P2's objects, by kind
	key  string            // P1 agent's key
	key2 string            // P2 agent's key for machine-scope checks
	// the P1 person's session
	cookie *http.Cookie
	csrf   string
	client *http.Client
}

func (w *crossWorld) ownerJSON(method, path string, body any, want int) map[string]any {
	w.t.Helper()
	resp := w.f.request(method, path, body, true)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != want {
		w.t.Fatalf("%s %s: %d (want %d): %v", method, path, resp.StatusCode, want, out)
	}
	return out
}

func newCrossWorld(t *testing.T) *crossWorld {
	t.Helper()
	f := newFixture(t)
	w := &crossWorld{t: t, f: f, p1: map[string]string{}, p2: map[string]string{}, client: &http.Client{Timeout: 20 * time.Second}}
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	// Both projects are made by the instance owner; the P1 person is then given P1 and nothing else.
	for _, side := range []struct {
		ids  map[string]string
		name string
	}{{w.p1, "P1 project"}, {w.p2, p2Mark + " project"}} {
		project := w.ownerJSON("POST", "/api/projects", map[string]string{"name": side.name, "folder": t.TempDir()}, 201)
		side.ids["project"] = project["id"].(string)
		room := w.ownerJSON("POST", "/api/projects/"+side.ids["project"]+"/rooms", map[string]string{"title": side.name + " room"}, 201)
		side.ids["room"] = room["id"].(string)
	}
	// The P1 person (a project owner of P1, the strongest role a non-admin can hold there) and a person who is in P2 only.
	cookie, csrf := registerSecondUser(t, f, "p1person")
	w.cookie, w.csrf = cookie, csrf
	p1person, err := f.db.UserByLogin(t.Context(), "p1person")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = registerSecondUser(t, f, "p2person")
	p2person, err := f.db.UserByLogin(t.Context(), "p2person")
	if err != nil {
		t.Fatal(err)
	}
	w.p2["user"] = p2person.ID
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, w.p1["project"], p1person.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, w.p2["project"], p2person.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The agents: a lead in P1 (the caller) and a worker in P2.
	agentIn := func(project, room, slug, role string) (store.Agent, string) {
		var agent store.Agent
		if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
			checkout := "chk_" + slug
			if _, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES (?,?,?,?)`, checkout, project, f.machineID, "/tmp/"+slug); err != nil {
				return err
			}
			var err error
			agent, err = tx.CreateAgent(store.Agent{Slug: slug, ProjectID: project, RoleID: role, Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: checkout})
			if err != nil {
				return err
			}
			return tx.AddMember(room, "agent", agent.ID)
		}); err != nil {
			t.Fatal(err)
		}
		key, err := f.srv.Auth.IssueAgentKey(context.Background(), agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.AgentToolIdentityByKeyHash(t.Context(), fmt.Sprintf("%x", sha256.Sum256([]byte(key)))); err != nil {
			t.Fatalf("the key of %s does not resolve: %v", slug, err)
		}
		return agent, key
	}
	agent1, key1 := agentIn(w.p1["project"], w.p1["room"], "p1lead", "role_leader_default")
	w.p1["agent"], w.key = agent1.ID, key1
	agent2, key2 := agentIn(w.p2["project"], w.p2["room"], "p2markworker", "role_worker_default")
	w.p2["agent"] = agent2.ID
	w.key2 = key2

	// P2's objects, each with the marker in what it holds.
	message, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: w.p2["room"], AuthorKind: "user", AuthorID: owner.ID, Body: p2Mark + " message body"})
	if err != nil {
		t.Fatal(err)
	}
	w.p2["message"], w.p2["no"] = message.ID, strconv.Itoa(int(message.No))
	attachment, err := f.db.SaveAttachment(t.Context(), w.p2["room"], owner.ID, "p2mark.txt", strings.NewReader(p2Mark+" attachment"))
	if err != nil {
		t.Fatal(err)
	}
	w.p2["attachment"] = attachment.ID
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
			VALUES ('inb_p2mark',?,?,?,'agent',?,'recovery',?,?)`, w.p2["project"], w.p2["room"], agent2.ID, agent2.ID,
			`{"text":"`+p2Mark+` recovery","options":["Continue","Abandon"]}`, "2026-01-01T00:00:00.000000000Z"); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO command_job(id,agent_id,generation,machine_id,project_id,room_id,label,cmd,cwd,timeout_seconds,state,log_path,created_at)
			VALUES ('cmd_p2mark',?,0,?,?,?,?,'echo p2mark','.',60,'running','/tmp/p2mark.log',?)`, agent2.ID, f.machineID, w.p2["project"], w.p2["room"], p2Mark+" label", "2026-01-01T00:00:00.000000000Z"); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_p2mark',?,0,'fresh_with_history',?)`, agent2.ID, "2026-01-01T00:00:00.000Z"); err != nil {
			return err
		}
		turn, err := tx.RecordTurn(store.Turn{AgentID: agent2.ID, SessionID: "ses_p2mark", Generation: 0, RoomID: w.p2["room"], Attempt: 1, Mode: "normal", Exec: "running"})
		if err != nil {
			return err
		}
		w.p2["turn"] = turn.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.p2["item"], w.p2["command"] = "inb_p2mark", "cmd_p2mark"
	w.ownerJSON("PUT", "/api/projects/"+w.p2["project"]+"/memory-enabled", map[string]bool{"enabled": true}, 200)
	w.ownerJSON("PUT", "/api/projects/"+w.p1["project"]+"/memory-enabled", map[string]bool{"enabled": true}, 200) // so a P1 parent with a P2 memory record reaches the record's own check
	fact, err := f.db.ManageKnowledge(t.Context(), "project", w.p2["project"], "user", owner.ID, "", 0, p2Mark+" project fact", false)
	if err != nil {
		t.Fatal(err)
	}
	w.p2["memory"] = fact.ID
	note, err := f.db.ManageKnowledge(t.Context(), "task", w.p2["room"], "user", owner.ID, "", 0, p2Mark+" task note", false)
	if err != nil {
		t.Fatal(err)
	}
	w.p2["note"] = note.ID
	schedule := w.ownerJSON("POST", "/api/rooms/"+w.p2["room"]+"/schedules", map[string]any{"target_lead": true, "body": p2Mark + " schedule", "kind": "interval", "timezone": "UTC", "interval_seconds": 300}, 201)
	w.p2["schedule"] = schedule["id"].(string)
	w.p2["machine"] = f.machineID
	return w
}

// valueFor is the id a wildcard of a route takes. mixed puts P1's own project and room where the route has them as parents, with P2's
// object as the child. An id that names nothing of a project (a tool, a launcher) takes a plausible fixed value: those routes are
// instance-wide and answer to the caller's role, not to a project.
func (w *crossWorld) valueFor(pattern, name string, mixed bool) (string, bool) {
	_, path := splitPattern(pattern)
	switch name {
	case "project", "room":
		if mixed {
			return w.p1[name], true
		}
		return w.p2[name], true
	case "agent", "message", "no", "attachment", "item", "user", "turn", "machine":
		return w.p2[name], true
	case "emoji":
		return "👍", true
	case "scope":
		return "agent_shared_notes", true
	case "tool":
		return "claude", true
	case "launcher":
		return "mln_none", true
	case "variant":
		return "native", true
	case "journal":
		return "jrn_none", true
	case "key":
		return "prompt_none", true
	case "name":
		return "profile_none", true
	case "role":
		return "role_worker_default", true
	case "rev":
		return "1", true
	case "action":
		return "use", true
	case "sound":
		return "snd_none", true
	case "id":
		switch {
		case strings.HasPrefix(path, "/api/agents/{id}"), strings.HasPrefix(path, "/api/agent/agents/{id}"):
			return w.p2["agent"], true
		case strings.HasPrefix(path, "/api/machines/{id}"):
			return w.p2["machine"], true
		case strings.HasPrefix(path, "/api/roles/{id}"):
			return "role_worker_default", true
		case strings.HasPrefix(path, "/api/global-memory/{scope}/{id}"):
			return "knw_none", true
		case strings.HasPrefix(path, "/api/import-v1/jobs/{id}"):
			return "job_none", true
		case strings.HasPrefix(path, "/api/projects/{project}/memory/{id}"):
			return w.p2["memory"], true
		case strings.HasPrefix(path, "/api/agent/memory/{id}"):
			return w.p2["note"], true
		case strings.HasPrefix(path, "/api/rooms/{room}/schedules/{id}"), strings.HasPrefix(path, "/api/agent/rooms/{room}/schedules/{id}"):
			return w.p2["schedule"], true
		case strings.HasPrefix(path, "/api/rooms/{room}/task-notes/{id}"):
			return w.p2["note"], true
		case strings.HasPrefix(path, "/api/agent/commands/{id}"):
			return w.p2["command"], true
		}
	}
	return "", false
}

// parentChildPaths says whether a route has a project or room id and another id after it, so that mixing P1's parent with P2's child makes sense.
func parentChild(path string) bool {
	names := wildcardsOf(path)
	if len(names) < 2 || names[0] != "project" && names[0] != "room" {
		return false
	}
	for _, name := range names[1:] {
		switch name {
		case "id", "attachment", "agent", "user", "message", "rev":
			return true
		}
	}
	return false
}

// call sends one request as the P1 person (or as the P1 agent) and returns the status and the body.
func (w *crossWorld) call(asAgent bool, method, path string, body any) (int, string) {
	w.t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, w.f.http.URL+path, bytes.NewReader(data))
	if err != nil {
		w.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if asAgent {
		req.Header.Set("Authorization", "Bearer "+w.key)
	} else {
		req.AddCookie(w.cookie)
		if method != http.MethodGet {
			req.Header.Set("X-CSRF-Token", w.csrf)
		}
	}
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", "cross-"+fmt.Sprint(time.Now().UnixNano()))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(raw)
}

// crossProjectStatusExceptions lists the routes whose answer to a person outside the project is not a 403 or 404 by design: the status
// they answer with, the text that answer must hold, and the reason. The check that the body carries nothing of P2 still applies to them.
var crossProjectStatusExceptions = map[string]struct {
	status   int
	contains string
	reason   string
}{
	"GET /api/agent/message-numbers/{no}": {200, `"available":false`, "a message number the agent may not read answers like a number nothing carries (store.AgentMessageByNo), so the lookup does not tell that it exists"},
	"GET /api/global-memory/{scope}":      {200, `"records"`, "the instance-wide notes (user profiles, shared agent notes) are read by every signed-in person; they belong to no project"},
	"GET /api/machines/{id}/pair-status":  {401, "machine credential required", "the pairing probe of a machine, authorized by the machine's own credential and not by a session"},
}

// bodyFor is the body a write route needs to get past its validation, so the request reaches the check of who may do it: the handlers
// refuse fields they do not know, and each asks for different ones. A route not listed takes an empty object.
func (w *crossWorld) bodyFor(pattern string) any {
	p2 := w.p2
	table := map[string]map[string]any{
		"POST /api/agent/agents/{id}/role":                           {"role_key": "worker", "rev": 1},
		"POST /api/agent/recoveries/{item}/decision":                 {"choice": "abandon"},
		"PUT /api/agent/memory/{id}":                                 {"body": "x", "rev": 1},
		"POST /api/agents/{id}/control":                              {"desired": "stopped"},
		"POST /api/agents/{id}/switch":                               {"room_id": p2["room"], "tool": "claude", "variant": "native", "model": "x", "rev": 1},
		"POST /api/agents/{id}/switch/preview":                       {"room_id": p2["room"], "tool": "claude", "variant": "native", "model": "x"},
		"POST /api/global-memory/{scope}":                            {"scope_key": "global", "body": "x"},
		"PUT /api/global-memory/{scope}/{id}":                        {"scope_key": "global", "body": "x", "rev": 1},
		"PUT /api/global-memory/{scope}/{id}/activation":             {"scope_key": "global", "rev": 1, "active": true},
		"POST /api/inbox/{item}/answer":                              {"body": "x", "rev": 1},
		"POST /api/inbox/{item}/decision":                            {"choice": "abandon", "rev": 1},
		"POST /api/projects/{project}/dms":                           {"user_id": p2["user"]},
		"POST /api/projects/{project}/team-flow/migration/interrupt": {"agent_id": p2["agent"]},
		"POST /api/projects/{project}/team-flow/preview":             {"key": "leader-workers"},
		"PUT /api/projects/{project}/team-flow":                      {"key": "leader-workers", "rev": 1},
		"PUT /api/projects/{project}/machine-folders/{machine}":      {"folder": "/tmp/cross-p2"},
		"PUT /api/projects/{project}/memory-enabled":                 {"enabled": true},
		"PUT /api/projects/{project}/memory/{id}":                    {"body": "x", "rev": 1},
		"PUT /api/projects/{project}/settings":                       {"max_live_agents": 3},
		"PUT /api/prompts/{key}":                                     {"project_id": p2["project"], "value": "x", "rev": 1},
		"PUT /api/rooms/{room}/task-notes/{id}":                      {"body": "x", "rev": 1},
		"PUT /api/users/{user}/admin":                                {"admin": true},
	}
	if body, ok := table[pattern]; ok {
		return body
	}
	return map[string]any{}
}

// queryFor is the query string of a route that reads its scope from the URL: a DELETE names the revision, a prompt its project, and the
// agent's memory routes the scope of the record.
func (w *crossWorld) queryFor(pattern, method string) string {
	switch pattern {
	case "PUT /api/agent/memory/{id}", "DELETE /api/agent/memory/{id}":
		return "?scope=task&room_id=" + w.p2["room"] + "&rev=1"
	}
	if pattern == "DELETE /api/global-memory/{scope}/{id}" {
		return "?rev=1&scope_key=global"
	}
	if method == http.MethodDelete {
		return "?rev=1&project_id=" + w.p2["project"]
	}
	return ""
}

// Every route with an id in its path refuses a person of P1 (and, under /api/agent/, an agent of P1) when the id is P2's.
func TestCrossProjectRoutesRefuseTheOtherProjectsObjects(t *testing.T) {
	t.Parallel()
	patterns := routePatterns(t)
	if len(patterns) < 200 {
		t.Fatalf("only %d routes were read from the sources: the reader is broken", len(patterns))
	}
	w := newCrossWorld(t)

	// Controls: the harness would pass vacuously if the marker never showed in a genuine answer or P1's own objects were unreachable.
	if resp := w.f.requestAs(t, w.f.cookie, w.f.csrf, "GET", "/api/rooms/"+w.p2["room"], nil); true {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(raw), p2Mark) {
			t.Fatalf("control: the owner's view of P2's room is %d without the marker: %s", resp.StatusCode, raw)
		}
	}
	if code, body := w.call(false, "GET", "/api/rooms/"+w.p1["room"], nil); code != 200 {
		t.Fatalf("control: the P1 person cannot read P1's own room: %d %s", code, body)
	}
	if code, body := w.call(true, "GET", "/api/agent/rooms/"+w.p1["room"]+"/history", nil); code != 200 {
		t.Fatalf("control: the P1 agent cannot read P1's own room: %d %s", code, body)
	}

	exercised, mixedRuns := 0, 0
	for _, pattern := range patterns {
		method, path := splitPattern(pattern)
		names := wildcardsOf(path)
		if len(names) == 0 {
			if _, ok := crossProjectAllowlist[pattern]; !ok {
				t.Errorf("%s: a route without an object id is neither on the allowlist nor checked: add it to crossProjectAllowlist with the reason it needs no cross-project check", pattern)
			}
			continue
		}
		if reason, ok := crossProjectAllowlist[pattern]; ok {
			t.Errorf("%s is on the allowlist (%q) but has ids in its path: take it off", pattern, reason)
			continue
		}
		asAgent := strings.HasPrefix(path, "/api/agent/")
		for _, mixed := range []bool{false, true} {
			if mixed && !parentChild(path) {
				continue
			}
			concrete := path
			complete := true
			for _, name := range names {
				value, ok := w.valueFor(pattern, name, mixed)
				if !ok {
					t.Errorf("%s: no object of P2 is known for {%s}: teach valueFor", pattern, name)
					complete = false
					break
				}
				concrete = strings.Replace(concrete, "{"+name+"}", url.PathEscape(value), 1)
			}
			if !complete {
				break
			}
			var body any
			if method != http.MethodGet {
				body = w.bodyFor(pattern)
			}
			concrete += w.queryFor(pattern, method)
			code, answer := w.call(asAgent, method, concrete, body)
			label := fmt.Sprintf("%s as the P1 %s%s -> %s", pattern, map[bool]string{false: "person", true: "agent"}[asAgent], map[bool]string{false: "", true: " (P1 parent, P2 child)"}[mixed], concrete)
			if mixed {
				mixedRuns++
			} else {
				exercised++
			}
			if strings.Contains(answer, p2Mark) {
				t.Errorf("%s: the answer carries P2's data: %s", label, truncate(answer))
			}
			if exception, ok := crossProjectStatusExceptions[pattern]; ok {
				if code != exception.status || !strings.Contains(answer, exception.contains) {
					t.Errorf("%s: status %d, want %d holding %q (%s): %s", label, code, exception.status, exception.contains, exception.reason, truncate(answer))
				}
			} else if code != 403 && code != 404 {
				t.Errorf("%s: status %d, want 403 or 404: %s", label, code, truncate(answer))
			}
			if code == 404 && strings.HasPrefix(answer, "404 page not found") {
				t.Errorf("%s: the mux has no such route: the test and the server disagree about %s", label, pattern)
			}
		}
	}
	t.Logf("exercised %d routes with ids (%d more with a P1 parent and a P2 child) out of %d registrations", exercised, mixedRuns, len(patterns))
	if exercised < 150 {
		t.Fatalf("only %d routes with ids were exercised", exercised)
	}
	// Seed a legacy agent on a now-restricted host. The scope setter itself
	// refuses to create this situation; the direct seed checks every admission
	// path against a stale agent that still needs recovery.
	if err := w.f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE machine SET scope_mode='listed' WHERE id=?`, w.f.machineID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO machine_project_scope(machine_id,project_id) VALUES (?,?)`, w.f.machineID, w.p1["project"]); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE role SET spawn_defaults_json='[]' WHERE role_key='worker'`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE agent SET role_id='role_leader_default' WHERE id=?`, w.p2["agent"])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertScope := func(label string, code int, body string) {
		t.Helper()
		if code != 409 || !strings.Contains(body, "machine_scope") {
			t.Errorf("%s: %d %s", label, code, body)
		}
	}
	ownerCall := func(path string, body any) (int, string) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, w.f.http.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", w.f.csrf)
		req.Header.Set("Idempotency-Key", "scope-"+fmt.Sprint(time.Now().UnixNano()))
		req.AddCookie(w.f.cookie)
		resp, err := w.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(result)
	}
	code, body := ownerCall("/api/rooms/"+w.p2["room"]+"/agents", map[string]any{"role_key": "worker", "tool": "fake", "variant": "test", "model": "test", "machine_id": w.f.machineID})
	assertScope("person spawn", code, body)
	w.key = w.key2
	code, body = w.call(true, "POST", "/api/agent/agents", map[string]any{"room_id": w.p2["room"], "role_key": "worker", "tool": "fake", "variant": "test", "model": "test", "machine_id": w.f.machineID})
	assertScope("tool spawn", code, body)
	code, body = ownerCall("/api/agents/"+w.p2["agent"]+"/switch", map[string]any{"room_id": w.p2["room"], "tool": "fake", "variant": "test", "model": "test", "machine_id": w.f.machineID, "rev": 0})
	assertScope("switch", code, body)
	if err := w.f.db.CheckMachineProject(t.Context(), w.p2["project"], w.f.machineID); !errors.Is(err, store.ErrMachineScope) {
		t.Errorf("recovery admission: %v", err)
	}
	if _, err := w.f.db.ProjectMachineFolder(t.Context(), w.p2["project"], w.f.machineID); err != nil {
		t.Errorf("folder lookup for cleanup: %v", err)
	}
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// A route on the allowlist is still read when it is a GET: with P2's ids in the query string, its answer to a person (or an agent) of
// P1 must carry nothing of P2. The event stream and pprof are not called.
func TestCrossProjectListsCarryNothingOfP2(t *testing.T) {
	t.Parallel()
	w := newCrossWorld(t)
	query := "?project=" + w.p2["project"] + "&project_id=" + w.p2["project"] + "&room=" + w.p2["room"] + "&room_id=" + w.p2["room"] +
		"&agent_id=" + w.p2["agent"] + "&user=" + w.p2["user"] + "&q=" + p2Mark + "&query=" + p2Mark + "&scope=project&scope_id=" + w.p2["project"]
	skip := map[string]bool{"GET /": true, "GET /api/events": true}
	called := 0
	for _, pattern := range routePatterns(t) {
		method, path := splitPattern(pattern)
		if method != "GET" || len(wildcardsOf(path)) > 0 || skip[pattern] || strings.HasPrefix(path, "/api/debug/pprof") {
			continue
		}
		asAgent := strings.HasPrefix(path, "/api/agent/")
		code, answer := w.call(asAgent, method, path+query, nil)
		called++
		if code >= 500 || code == 0 {
			t.Errorf("%s: status %d: %s", pattern, code, truncate(answer))
		}
		if strings.Contains(answer, p2Mark) {
			t.Errorf("%s: a person or agent of P1 got data of P2: %s", pattern, truncate(answer))
		}
	}
	t.Logf("read %d id-less GET routes", called)
}

// The agent MCP tools that take an id answer a P1 agent that names a P2 object with an error that carries nothing of P2. A tool that is
// neither here nor in mcpToolsWithoutObjectID fails the test.
func TestCrossProjectAgentMCPTools(t *testing.T) {
	t.Parallel()
	w := newCrossWorld(t)
	rpc := func(method string, params any) map[string]any {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest("POST", w.f.http.URL+"/api/agent/mcp", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+w.key)
		resp, err := w.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return out
	}
	listed := rpc("tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any)
	byTool := map[string]map[string]any{
		"resolve_recovery": {"item_id": w.p2["item"], "choice": "abandon"},
		"room_history":     {"room_id": w.p2["room"]},
		"list_schedules":   {"room_id": w.p2["room"]},
		"create_schedule":  {"room_id": w.p2["room"], "body": "x", "kind": "interval", "timezone": "UTC", "interval_seconds": 300, "target_lead": true},
		"delete_schedule":  {"room_id": w.p2["room"], "schedule_id": w.p2["schedule"]},
		"run_command":      {"room_id": w.p2["room"], "cmd": "true"},
		"command_log":      {"job_id": w.p2["command"]},
		"cancel_command":   {"job_id": w.p2["command"]},
		"create_room":      {"parent_id": w.p2["room"], "title": "x"},
		"dm_agent":         {"target_agent_id": w.p2["agent"], "body": "x"},
		"close_room":       {"room_id": w.p2["room"]},
		"memory_add":       {"scope": "task", "room_id": w.p2["room"], "body": "x"},
		"memory_list":      {"scope": "task", "room_id": w.p2["room"]},
		"memory_update":    {"scope": "project", "id": w.p2["memory"], "body": "x"},
		"memory_remove":    {"scope": "project", "id": w.p2["memory"]},
		"message_get":      {"no": w.p2["no"]},
		"attachment_get":   {"attachment_id": w.p2["attachment"]},
		"spawn_agent":      {"room_id": w.p2["room"], "role_key": "worker", "reason": "x"},
		"stop_agent":       {"agent_id": w.p2["agent"]},
		"agent_output":     {"agent_id": w.p2["agent"]},
		"set_role":         {"agent_id": w.p2["agent"], "role_key": "worker"},
		"react":            {"message_id": w.p2["message"], "emoji": "👍"},
	}
	mcpToolsWithoutObjectID := map[string]string{
		"submit_answer":   "ends the caller's own turn; no object",
		"list_recoveries": "lists the caller's own recoveries (checked by TestCrossProjectListsCarryNothingOfP2)",
		"list_rooms":      "lists the caller's own rooms (same test)",
		"list_agents":     "lists the caller's project (same test)",
		"list_macros":     "the shared macro map; no project data",
		"usage_left":      "the caller's own quota",
		"list_commands":   "the caller's own commands (same test)",
		"dm_human":        "a person by login, answered by the project's membership rules; the body goes to the project, no object id",
		"handbook_get":    "the caller's own project's handbook",
		"handbook_update": "the caller's own project's handbook",
		"memory_recall":   "searches the caller's own rooms; the query is checked by TestCrossProjectListsCarryNothingOfP2",
	}
	seen := 0
	for _, entry := range listed {
		name := entry.(map[string]any)["name"].(string)
		args, ok := byTool[name]
		if !ok {
			if _, ok := mcpToolsWithoutObjectID[name]; !ok {
				t.Errorf("tool %s is neither checked with a P2 id nor allowlisted: add it to byTool or mcpToolsWithoutObjectID", name)
			}
			continue
		}
		seen++
		if _, has := args["request_key"]; !has && name != "message_get" && name != "attachment_get" && name != "room_history" && name != "agent_output" && name != "list_schedules" && name != "memory_list" && name != "command_log" {
			args["request_key"] = "cross-" + name
		}
		result := rpc("tools/call", map[string]any{"name": name, "arguments": args})["result"].(map[string]any)
		text := fmt.Sprint(result["content"])
		if name == "message_get" {
			// a number the agent may not read answers like a number nothing carries
			if result["isError"] == true || !strings.Contains(text, "available:false") && !strings.Contains(text, `"available":false`) {
				t.Errorf("tool message_get with a P2 number: %s", truncate(text))
			}
		} else if result["isError"] != true {
			t.Errorf("tool %s with a P2 id was not refused: %s", name, truncate(text))
		}
		if strings.Contains(text, p2Mark) {
			t.Errorf("tool %s answered a P1 agent with data of P2: %s", name, truncate(text))
		}
	}
	for name := range byTool {
		found := false
		for _, entry := range listed {
			if entry.(map[string]any)["name"] == name {
				found = true
			}
		}
		if !found {
			t.Errorf("byTool names %s, which the toolbox no longer lists", name)
		}
	}
	t.Logf("exercised %d MCP tools of %d", seen, len(listed))
}
