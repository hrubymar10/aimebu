package server

import (
	"fmt"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

// Any active agent, a worker as much as a lead, may start a private chat with a person of its own project; the server decides who, and how much.
func TestDMHumanToolLetsAWorkerMessageAPersonOfItsProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, workerKey := f.createToolAgentNamed(t, "role_worker_default", "worker")
	lead, leadKey := f.createToolAgent(t, "role_lead_default")
	f.member(t, "viewer", "viewer")
	f.member(t, "operator", "operator")
	// A person who has an account but no part in the project.
	registerSecondUser(t, f, "stranger")
	viewer, err := f.db.UserByLogin(t.Context(), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	send := func(key, request string, body map[string]string) (*store.HumanDMResult, int, map[string]any) {
		t.Helper()
		resp := f.toolPost(t, "/api/agent/human-dms", key, request, body)
		defer resp.Body.Close()
		var raw map[string]any
		if resp.StatusCode != 200 {
			_ = json.NewDecoder(resp.Body).Decode(&raw)
			return nil, resp.StatusCode, raw
		}
		var result store.HumanDMResult
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return &result, resp.StatusCode, nil
	}
	ask := map[string]string{"target_user": "viewer", "body": "I need a decision on the schema"}
	first, status, _ := send(workerKey, "h-1", ask)
	if status != 200 || !first.Created || first.RoomID == "" || first.MessageID == "" {
		t.Fatalf("a worker's first message: %d %+v", status, first)
	}
	if replay, status, _ := send(workerKey, "h-1", ask); status != 200 || replay.MessageID != first.MessageID {
		t.Fatalf("a retry with the same key posts nothing new: %d %+v", status, replay)
	}
	if _, status, _ := send(workerKey, "h-1", map[string]string{"target_user": "viewer", "body": "different"}); status != 409 {
		t.Fatalf("a key reused with another request: %d", status)
	}
	later, status, _ := send(workerKey, "h-2", map[string]string{"target_user": viewer.ID, "body": "one more thing"}) // by id as well as by login
	if status != 200 || later.Created || later.RoomID != first.RoomID {
		t.Fatalf("a second message goes into the same chat: %d %+v", status, later)
	}
	// The chat is a private room of the two, which the person can read and the project's other people cannot.
	room, err := f.db.RoomByID(t.Context(), first.RoomID)
	if err != nil || room.Kind != "dm" || room.ProjectID != f.project.ID {
		t.Fatalf("room=%+v err=%v", room, err)
	}
	if err := f.db.UserRoomAccess(t.Context(), viewer.ID, first.RoomID); err != nil {
		t.Fatalf("the person cannot read the chat: %v", err)
	}
	operator, err := f.db.UserByLogin(t.Context(), "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UserRoomAccess(t.Context(), operator.ID, first.RoomID); err == nil {
		t.Fatal("another member of the project can read a private chat")
	}
	messages, err := f.db.ListMessagesAfter(t.Context(), first.RoomID, 0)
	if err != nil {
		t.Fatal(err)
	}
	written := 0
	for _, m := range messages {
		if m.AuthorKind == "agent" && m.Body != "" {
			written++
		}
	}
	if written != 2 {
		t.Fatalf("messages in the chat: %d, want 2", written)
	}
	// The lead may too, and the chat of each agent is its own.
	if other, status, _ := send(leadKey, "l-1", ask); status != 200 || other.RoomID == first.RoomID || !other.Created {
		t.Fatalf("the lead's chat: %d %+v", status, other)
	}
	_ = lead
	// Refusals: nobody to talk to, a person outside the project, a stranger's login and an unknown one answer alike, so a login cannot be probed.
	for name, body := range map[string]map[string]string{
		"a person outside the project": {"target_user": "stranger", "body": "hi"},
		"an unknown person":            {"target_user": "nobody-at-all", "body": "hi"},
		"an agent id":                  {"target_user": lead.ID, "body": "hi"},
	} {
		if _, status, raw := send(workerKey, "refuse-"+name, body); status != 403 || raw["error"] != "forbidden" {
			t.Fatalf("%s: %d %v", name, status, raw)
		}
	}
	for name, body := range map[string]map[string]string{
		"no target":  {"body": "hi"},
		"empty body": {"target_user": "viewer", "body": "  "},
	} {
		if _, status, _ := send(workerKey, "bad-"+name, body); status != 400 {
			t.Fatalf("%s: %d", name, status)
		}
	}
}

// A person is not a queue: one agent writes only so many messages into one chat an hour, and opens only so many chats.
func TestDMHumanToolIsCapped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgentNamed(t, "role_worker_default", "worker")
	f.member(t, "viewer", "viewer")
	post := func(i int, target string) (int, map[string]any) {
		resp := f.toolPost(t, "/api/agent/human-dms", key, fmt.Sprintf("cap-%s-%d", target, i), map[string]string{"target_user": target, "body": fmt.Sprintf("message %d", i)})
		defer resp.Body.Close()
		var raw map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		return resp.StatusCode, raw
	}
	for i := 0; i < store.HumanDMMessagesPerHour; i++ {
		if status, raw := post(i, "viewer"); status != 200 {
			t.Fatalf("message %d: %d %v", i, status, raw)
		}
	}
	if status, raw := post(store.HumanDMMessagesPerHour, "viewer"); status != 429 || raw["error"] != "human_dm_cap" {
		t.Fatalf("the message over the cap: %d %v", status, raw)
	}
}

func TestDMHumanToolRefusesAnArchivedProjectAndAStoppedAgent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgentNamed(t, "role_worker_default", "worker")
	f.member(t, "viewer", "viewer")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.toolPost(t, "/api/agent/human-dms", key, "stopped", map[string]string{"target_user": "viewer", "body": "hi"})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("a stopped agent: %d", resp.StatusCode)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE agent SET desired='active' WHERE id=?`, agent.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE project SET archived_at='2026-01-01T00:00:00.000000000Z' WHERE id=?`, f.project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.toolPost(t, "/api/agent/human-dms", key, "archived", map[string]string{"target_user": "viewer", "body": "hi"})
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("an archived project: %d", resp.StatusCode)
	}
}
