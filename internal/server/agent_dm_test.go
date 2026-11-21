package server

import (
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
	_, peer := f.otherProjectAgent(t)
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
