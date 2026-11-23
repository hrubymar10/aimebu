package server

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// otherProjectAgent makes a second project with one worker in it.
func (f *fixture) otherProjectAgent(t *testing.T) (store.Project, store.Agent) {
	t.Helper()
	resp := f.request("POST", "/api/projects", map[string]string{"name": "second", "folder": t.TempDir()}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("project: %d", resp.StatusCode)
	}
	var project store.Project
	json.NewDecoder(resp.Body).Decode(&project)
	resp.Body.Close()
	var agent store.Agent
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_second',?,?,'/tmp/second')`, project.ID, f.machineID); err != nil {
			return err
		}
		var err error
		agent, err = tx.CreateAgent(store.Agent{Slug: "peer", ProjectID: project.ID, RoleID: "role_worker_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: "chk_second", KeyHash: "placeholder2", CreatedBy: "test"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return project, agent
}

// otherProjectLead adds a lead named boss to the second project (made by otherProjectAgent).
func (f *fixture) otherProjectLead(t *testing.T, project store.Project, keyHash string) store.Agent {
	t.Helper()
	var boss store.Agent
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		boss, err = tx.CreateAgent(store.Agent{Slug: "boss", ProjectID: project.ID, RoleID: "role_leader_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: "chk_second", KeyHash: keyHash, CreatedBy: "test"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return boss
}

// ownerTurn gives the lead a running turn that the instance owner's message caused.
func (f *fixture) ownerTurn(t *testing.T, lead store.Agent) {
	t.Helper()
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner.ID, Body: "talk to the other project"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_dm',?,0,'fresh_with_history',?)`, lead.ID, "2026-01-01T00:00:00.000Z"); err != nil {
			return err
		}
		turn, err := tx.RecordTurn(store.Turn{AgentID: lead.ID, SessionID: "ses_dm", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "running"})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO turn_note(turn_id,note_id) SELECT ?,id FROM note WHERE agent_id=? AND message_id=?`, turn.ID, lead.ID, msg.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDMAgentToolOpensAChatIdempotently(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	_, workerKey := f.createToolAgentNamed(t, "role_worker_default", "worker")
	project, worker := f.otherProjectAgent(t)
	peer := f.otherProjectLead(t, project, "placeholder-boss")
	f.ownerTurn(t, lead)
	send := func(key, request string, body map[string]string) (*store.AgentDMResult, int) {
		resp := f.toolPost(t, "/api/agent/dms", key, request, body)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, resp.StatusCode
		}
		var result store.AgentDMResult
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return &result, resp.StatusCode
	}
	ask := map[string]string{"target_agent_id": peer.ID, "body": "can you check the contract?"}
	if _, status := send(workerKey, "w-1", ask); status != 403 {
		t.Fatalf("a worker started a chat: %d", status)
	}
	first, status := send(key, "dm-1", ask)
	if status != 200 || !first.Created || first.RoomID == "" {
		t.Fatalf("first: %d %+v", status, first)
	}
	replay, status := send(key, "dm-1", ask)
	if status != 200 || replay.MessageID != first.MessageID {
		t.Fatalf("replay: %d %+v", status, replay)
	}
	if _, status := send(key, "dm-1", map[string]string{"target_agent_id": peer.ID, "body": "different"}); status != 409 {
		t.Fatalf("key reuse: %d", status)
	}
	later, status := send(key, "dm-2", map[string]string{"target_agent_id": peer.ID, "body": "one more thing"})
	if status != 200 || later.Created || later.RoomID != first.RoomID {
		t.Fatalf("second message: %d %+v", status, later)
	}
	messages, err := f.db.ListMessagesAfter(t.Context(), first.RoomID, 0)
	if err != nil {
		t.Fatal(err)
	}
	agentMessages := 0
	for _, m := range messages {
		if m.AuthorKind == "agent" {
			agentMessages++
		}
	}
	if agentMessages != 2 {
		t.Fatalf("messages in the chat=%d", agentMessages)
	}
	// Across projects a lead reaches only the other project's lead.
	if _, status := send(key, "dm-w", map[string]string{"target_agent_id": worker.ID, "body": "hi"}); status != 403 {
		t.Fatalf("another project's worker: %d", status)
	}
	if _, status := send(key, "dm-3", map[string]string{"target_agent_id": "agt_missing", "body": "hi"}); status != 403 {
		t.Fatalf("unknown agent: %d", status)
	}
	if _, status := send(key, "dm-4", map[string]string{"target_agent_id": peer.ID, "body": "  "}); status != 400 {
		t.Fatalf("empty body: %d", status)
	}
	// The lead sees the chat in its toolbox next to its project's rooms.
	rooms, err := f.db.RoomsForAgent(t.Context(), lead.ID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, room := range rooms {
		listed = listed || room.ID == first.RoomID
	}
	if !listed {
		t.Fatalf("the chat is not in the lead's rooms: %+v", rooms)
	}
}

// dm_agent takes slug@project; its errors say only what the caller's people could see anyway, and a note about an agent of another project
// carries that agent's ID.
func TestDMAgentToolTakesSlugAtProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	project, _ := f.otherProjectAgent(t)
	peer := f.otherProjectLead(t, project, "placeholder-boss")
	f.ownerTurn(t, lead)
	post := func(request, target string) (int, map[string]any) {
		resp := f.toolPost(t, "/api/agent/dms", key, request, map[string]string{"target_agent_id": target, "body": "hello"})
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	status, byName := post("slug-1", "boss@second")
	if status != 200 || byName["created"] != true {
		t.Fatalf("slug@project: %d %v", status, byName)
	}
	status, byID := post("slug-2", peer.ID)
	if status != 200 || byID["room_id"] != byName["room_id"] {
		t.Fatalf("by ID: %d %v", status, byID)
	}
	if status, out := post("slug-3", "ghost@second"); status != 404 || !strings.Contains(fmt.Sprint(out), "no agent ghost in project second") {
		t.Fatalf("missing agent in a reachable project: %d %v", status, out)
	}
	if status, _ := post("slug-4", "boss@nowhere"); status != 403 {
		t.Fatalf("unknown project: %d", status)
	}
	// Two projects of that name: ambiguous, listing the IDs.
	twin := f.request("POST", "/api/projects", map[string]string{"name": "Second", "folder": t.TempDir()}, true)
	var twinProject store.Project
	json.NewDecoder(twin.Body).Decode(&twinProject)
	twin.Body.Close()
	var twinAgent store.Agent
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_twin',?,?,'/tmp/twin')`, twinProject.ID, f.machineID); err != nil {
			return err
		}
		var err error
		twinAgent, err = tx.CreateAgent(store.Agent{Slug: "boss", ProjectID: twinProject.ID, RoleID: "role_leader_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: "chk_twin", KeyHash: "placeholder3", CreatedBy: "test"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.toolPost(t, "/api/agent/dms", key, "slug-5", map[string]string{"target_agent_id": "boss@second", "body": "hello"})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 409 || !strings.Contains(string(raw), peer.ID) || !strings.Contains(string(raw), twinAgent.ID) || !strings.Contains(string(raw), "ambiguous_agent") {
		t.Fatalf("ambiguous: %d %s", resp.StatusCode, raw)
	}
}

// list_agents adds other_leaders for an active lead only.
func TestListAgentsShowsOtherLeadersToALeadOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	_, workerKey := f.createToolAgentNamed(t, "role_worker_default", "worker")
	project, _ := f.otherProjectAgent(t)
	boss := f.otherProjectLead(t, project, "placeholder-boss")
	f.ownerTurn(t, lead)
	read := func(key string) map[string]json.RawMessage {
		resp := f.toolRequest(t, "/api/agent/agents", key)
		defer resp.Body.Close()
		var out map[string]json.RawMessage
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil {
			t.Fatalf("list_agents: %d", resp.StatusCode)
		}
		return out
	}
	var others []store.OtherLeader
	if err := json.Unmarshal(read(key)["other_leaders"], &others); err != nil || len(others) != 1 || others[0].ID != boss.ID || others[0].Slug != "boss" || others[0].ProjectSlug != "second" || others[0].Observed == "" {
		t.Fatalf("other_leaders=%+v err=%v", others, err)
	}
	if _, ok := read(workerKey)["other_leaders"]; ok {
		t.Fatal("a worker sees other_leaders")
	}
}
