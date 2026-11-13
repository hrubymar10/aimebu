package server

import (
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/store"
)

// A message an agent wrote carries what its turn spent, for anyone who can read the room; a person's message and a turn that reported nothing do
// not.
func TestMessagesCarryTheUsageOfTheTurnThatWroteThem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_leader_default")
	ctx := t.Context()
	var written []store.Message
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_usage',?,0,'fresh_with_history','now')`, agent.ID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.RecordTurn(ctx, store.Turn{AgentID: agent.ID, SessionID: "ses_usage", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"first reply", "second reply"} {
		outID := "out_usage_" + string(rune('a'+i))
		if err := f.db.Tx(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO outbox(id,turn_id,answer_rev,idx,agent_id,room_id,body,digest,state,dedupe_key) VALUES (?,?,0,?,?,?,?,'d','committed',?)`, outID, turn.ID, i, agent.ID, f.room.ID, body, outID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		message, err := f.db.InsertMessage(ctx, store.Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: body, TargetsJSON: "[]", SourceOutID: outID})
		if err != nil {
			t.Fatal(err)
		}
		written = append(written, message)
	}
	human := f.postReply(t, f.room.ID, "a person's message", "")
	frame, _ := json.Marshal(map[string]any{"type": "usage", "usage": map[string]any{"tokens_in": 12, "tokens_out": 3, "tokens_cache_read": 90, "cost_estimate": 0.004, "model": "claude-haiku-4-5"}})

	list := func() map[string]map[string]any {
		resp := f.request("GET", "/api/rooms/"+f.room.ID+"/messages", nil, false)
		defer resp.Body.Close()
		var views []map[string]any
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&views) != nil {
			t.Fatalf("messages: %d", resp.StatusCode)
		}
		out := map[string]map[string]any{}
		for _, view := range views {
			out[view["id"].(string)] = view
		}
		return out
	}
	for _, view := range list() {
		if _, ok := view["usage"]; ok {
			t.Fatalf("a turn that reported nothing has a usage: %+v", view)
		}
	}
	if err := f.db.RecordTurnEvent(ctx, turn.ID, agent.ID, "usage-1", "usage", "", frame); err != nil {
		t.Fatal(err)
	}
	views := list()
	if _, ok := views[human.ID]["usage"]; ok {
		t.Fatal("a person's message has a usage")
	}
	for i, message := range written {
		usage, _ := views[message.ID]["usage"].(map[string]any)
		if usage == nil || usage["index"] != float64(i+1) || usage["count"] != float64(2) || usage["tokens_in"] != float64(12) || usage["tokens_cache_read"] != float64(90) ||
			usage["cost"] != 0.004 || usage["model"] != "claude-haiku-4-5" || usage["turn_id"] != turn.ID {
			t.Fatalf("message %d usage: %+v", i, usage)
		}
		if _, ok := usage["tokens_cache_write"]; ok {
			t.Fatalf("a figure nobody reported must be absent, not zero: %+v", usage)
		}
	}
	// Someone who cannot read the room is told nothing.
	cookie, csrf := registerSecondUser(t, f, "stranger")
	resp := f.requestAs(t, cookie, csrf, "GET", "/api/rooms/"+f.room.ID+"/messages", nil)
	resp.Body.Close()
	if resp.StatusCode != 403 && resp.StatusCode != 404 {
		t.Fatalf("a stranger reads the messages: %d", resp.StatusCode)
	}
}

// A room's and a project's usage totals are readable by whoever can read them, by range, and nobody else.
func TestUsageTotalsRoutes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_leader_default")
	ctx := t.Context()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_totals',?,0,'fresh_with_history','now')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.RecordTurn(ctx, store.Turn{AgentID: agent.ID, SessionID: "ses_totals", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	frame, _ := json.Marshal(map[string]any{"type": "usage", "usage": map[string]any{"tokens_in": 12, "tokens_out": 3, "cost_estimate": 0.004, "model": "claude-haiku-4-5"}})
	if err := f.db.RecordTurnEvent(ctx, turn.ID, agent.ID, "usage-t", "usage", "", frame); err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, map[string]any) {
		resp := f.request("GET", path, nil, false)
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	for _, path := range []string{"/api/rooms/" + f.room.ID + "/usage", "/api/projects/" + f.project.ID + "/usage?range=7d", "/api/rooms/" + f.room.ID + "/usage?range=all"} {
		status, body := get(path)
		if status != 200 || body["turns"] != float64(1) || body["tokens_in"] != float64(12) || body["cost"] != 0.004 || body["cost_coverage"] != "known" {
			t.Fatalf("%s: %d %+v", path, status, body)
		}
		if byAgent, _ := body["by_agent"].([]any); len(byAgent) != 1 {
			t.Fatalf("%s by agent: %+v", path, body["by_agent"])
		}
		if byModel, _ := body["by_model"].([]any); len(byModel) != 1 || byModel[0].(map[string]any)["key"] != "claude-haiku-4-5" {
			t.Fatalf("%s by model: %+v", path, body["by_model"])
		}
	}
	if status, _ := get("/api/rooms/" + f.room.ID + "/usage?range=year"); status != 400 {
		t.Fatalf("a range that does not exist: %d", status)
	}
	if status, _ := get("/api/rooms/room_nope/usage"); status != 404 {
		t.Fatalf("a room that does not exist: %d", status)
	}
	cookie, csrf := registerSecondUser(t, f, "stranger")
	for _, path := range []string{"/api/rooms/" + f.room.ID + "/usage", "/api/projects/" + f.project.ID + "/usage"} {
		resp := f.requestAs(t, cookie, csrf, "GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode != 403 && resp.StatusCode != 404 {
			t.Fatalf("a stranger reads %s: %d", path, resp.StatusCode)
		}
	}
}
