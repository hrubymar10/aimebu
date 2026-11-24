package server

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// mcpToolCall calls one tool through the server's HTTP MCP endpoint, so the REST body is the one internal/agenttools builds from the
// arguments, byte for byte, and returns the text of the result and whether the tool reported an error.
func mcpToolCall(t *testing.T, f *fixture, key, name string, args map[string]any) (string, bool) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/agent/mcp", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Result.Content) == 0 {
		t.Fatalf("MCP answer %s: %v", raw, err)
	}
	return decoded.Result.Content[0].Text, decoded.Result.IsError
}

// connectFakeSupervisor connects a supervisor with the fake adapter for the fixture's machine, so a spawn can start its agent. keys maps the
// test's tool agents to their keys: the supervisor holds them, and a connecting supervisor would otherwise have them reissued.
func connectFakeSupervisor(t *testing.T, f *fixture, keys map[string]string) {
	t.Helper()
	journal, err := supervisor.OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	for agentID, key := range keys {
		if err := client.SetAgentKey(agentID, key); err != nil {
			t.Fatal(err)
		}
	}
	client.OnReady = func() {}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("supervisor did not stop")
		}
		journal.Close()
	})
	waitFor(t, func() bool { _, err := f.srv.machineForID(f.machineID); return err == nil })
}

// generalRoom is the project's "general" task room, the parent join_parent targets.
func generalRoom(t *testing.T, f *fixture) store.Room {
	t.Helper()
	rooms, err := f.db.RoomsForProject(t.Context(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range rooms {
		if room.Kind == "task" && (room.TeamFlowRoomKey == "general" || room.Title == "general") {
			return room
		}
	}
	t.Fatalf("no general room in %+v", rooms)
	return store.Room{}
}

// spawnViaTool spawns a worker or reviewer through the spawn_agent tool and returns the new agent's card, failing on a tool error.
func spawnViaTool(t *testing.T, f *fixture, key string, args map[string]any) store.AgentCard {
	t.Helper()
	text, failed := mcpToolCall(t, f, key, "spawn_agent", args)
	if failed {
		t.Fatalf("spawn_agent %v: %s", args, text)
	}
	var card store.AgentCard
	if err := json.Unmarshal([]byte(text), &card); err != nil || card.ID == "" {
		t.Fatalf("spawn_agent answer %s: %v", text, err)
	}
	return card
}

// assertJoinParent checks what join_parent promises at the HTTP boundary: without it the new agent is not in the parent room; with it the
// agent is a quiet member there, woken by a mention but not by an unaddressed message (the store's own test covers the rule in depth).
func assertJoinParent(t *testing.T, f *fixture, parent store.Room, plain, joined store.AgentCard) {
	t.Helper()
	inRoom := func(agentID string) bool {
		rooms, err := f.db.RoomsForAgent(t.Context(), agentID, f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, room := range rooms {
			if room.ID == parent.ID {
				return true
			}
		}
		return false
	}
	if inRoom(plain.ID) {
		t.Fatal("agent spawned without join_parent is a member of the parent room")
	}
	if !inRoom(joined.ID) {
		t.Fatal("agent spawned with join_parent is not a member of the parent room")
	}
	post := func(body string) string {
		resp := f.request(http.MethodPost, "/api/rooms/"+parent.ID+"/messages", map[string]string{"body": body}, true)
		defer resp.Body.Close()
		var message store.Message
		if err := json.NewDecoder(resp.Body).Decode(&message); err != nil || resp.StatusCode/100 != 2 || message.ID == "" {
			t.Fatalf("post in parent: status=%d %v", resp.StatusCode, err)
		}
		return message.ID
	}
	if _, err := f.db.NoteForAgentMessage(t.Context(), joined.ID, post("hello everyone")); err == nil {
		t.Fatal("the quiet member was woken by an unaddressed message in the parent room")
	}
	if _, err := f.db.NoteForAgentMessage(t.Context(), joined.ID, post("@"+joined.Slug+" findings?")); err != nil {
		t.Fatalf("the quiet member was not woken by a mention in the parent room: %v", err)
	}
}

// TestSpawnAgentToolJoinParentLead drives spawn_agent through the MCP endpoint as an active lead. internal/agenttools always sends
// join_parent, and the route decodes strictly, so a spawn_agent body the route does not know answers 400 for every spawn.
func TestSpawnAgentToolJoinParentLead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	worker, workerKey := f.createToolAgentNamed(t, "role_worker_default", "jpworker")
	connectFakeSupervisor(t, f, map[string]string{lead.ID: key, worker.ID: workerKey})
	general := generalRoom(t, f)
	task, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, ParentID: general.ID, Kind: "task", Title: "review", CreatedBy: lead.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), task.ID, "agent", lead.ID); err != nil {
		t.Fatal(err)
	}
	args := func(room, requestKey string, joinParent bool) map[string]any {
		out := map[string]any{"room_id": room, "role_key": "reviewer", "tool": "fake", "variant": "test", "model": "test", "request_key": requestKey}
		if joinParent {
			out["join_parent"] = true
		}
		return out
	}
	plain := spawnViaTool(t, f, key, args(task.ID, "jp-lead-plain", false))
	joined := spawnViaTool(t, f, key, args(task.ID, "jp-lead-joined", true))
	assertJoinParent(t, f, general, plain, joined)

	// A room whose parent is the home room: the join is refused, and so is the spawn.
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args(f.room.ID, "jp-lead-refused", true)); !failed ||
		!strings.HasPrefix(text, "HTTP 409") || !strings.Contains(text, "join_parent_refused") {
		t.Fatalf("join_parent under the home room: failed=%v %s", failed, text)
	}

	// A worker may not spawn, join_parent or not.
	if text, failed := mcpToolCall(t, f, workerKey, "spawn_agent", args(task.ID, "jp-worker", true)); !failed || !strings.HasPrefix(text, "HTTP 403") {
		t.Fatalf("worker spawn with join_parent: failed=%v %s", failed, text)
	}
}

// TestSpawnAgentToolJoinParentDispatcher is the same for the leader-dispatcher flow's dispatcher, which spawns a worker into a task room
// under general and may put it, quietly, into general too.
func TestSpawnAgentToolJoinParentDispatcher(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp := f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/team-flow", map[string]any{"key": "leader-dispatcher", "rev": 0}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply leader-dispatcher flow: %d", resp.StatusCode)
	}
	resp.Body.Close()
	dispatcher, key := f.createToolAgentNamed(t, "role_dispatcher_default", "jpdispatcher")
	connectFakeSupervisor(t, f, map[string]string{dispatcher.ID: key})
	general := generalRoom(t, f)
	task, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, ParentID: general.ID, Kind: "task", Title: "review", CreatedBy: dispatcher.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), task.ID, "agent", dispatcher.ID); err != nil {
		t.Fatal(err)
	}
	args := func(room, requestKey string, joinParent bool) map[string]any {
		out := map[string]any{"room_id": room, "role_key": "worker", "tool": "fake", "variant": "test", "model": "test", "request_key": requestKey}
		if joinParent {
			out["join_parent"] = true
		}
		return out
	}
	plain := spawnViaTool(t, f, key, args(task.ID, "jp-disp-plain", false))
	joined := spawnViaTool(t, f, key, args(task.ID, "jp-disp-joined", true))
	assertJoinParent(t, f, general, plain, joined)
}
