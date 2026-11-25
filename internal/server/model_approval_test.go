package server

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// seedFakePool configures a role's default pool with the fake harness of the
// fixture machine, so the model-approval gate has a pool to judge against: the
// fake/test/test choice is in it, model-b is not.
func seedFakePool(t *testing.T, f *fixture, role string) {
	t.Helper()
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled) VALUES ('lnch_fake',?,'fake','test','fake',1),('lnch_other',?,'fake','alternate','fake',1)`, f.machineID, f.machineID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO harness_model(launcher_id,slug,label,is_default,position) VALUES ('lnch_fake','test','Test',1,0),('lnch_fake','model-b','Model Bee',0,1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/role-spawn-defaults/"+role,
		map[string]any{"inherit": false, "default_agents": []map[string]string{{"launcher_id": "lnch_fake", "model": "test"}}, "pick_mode": "in_turn"}, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set %s pool: %d", role, resp.StatusCode)
	}
}

// personMessage posts a message as the owner and returns its id and number.
func personMessage(t *testing.T, f *fixture, roomID, body string) (string, int64) {
	t.Helper()
	resp := f.request(http.MethodPost, "/api/rooms/"+roomID+"/messages", map[string]string{"body": body}, true)
	defer resp.Body.Close()
	var message store.Message
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil || resp.StatusCode/100 != 2 || message.ID == "" {
		t.Fatalf("post message: %d %v", resp.StatusCode, err)
	}
	return message.ID, message.No
}

// TestAgentSpawnModelApproval drives the model-approval gate through the
// spawn_agent tool: a harness choice outside the role's pool is refused with
// 409 model_not_approved unless a person's message approves it, an in-pool
// choice never needs one, and a person's own spawn is never gated. The fake
// test harness only starts fake/test/test, so an approved out-of-pool model
// gets past the gate and is then refused by the harness's own model list: the
// test asserts that later refusal to show the gate opened.
func TestAgentSpawnModelApproval(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	seedFakePool(t, f, "worker")
	connectFakeSupervisor(t, f, map[string]string{lead.ID: key})

	args := func(requestKey string, choice map[string]string, approvedBy string) map[string]any {
		out := map[string]any{"room_id": f.room.ID, "role_key": "worker", "reason": "the task asked for it", "request_key": requestKey}
		for name, value := range choice {
			out[name] = value
		}
		if approvedBy != "" {
			out["approved_by"] = approvedBy
		}
		return out
	}
	inPool := map[string]string{"tool": "fake", "variant": "test", "model": "test"}
	outOfPool := map[string]string{"tool": "fake", "variant": "test", "model": "model-b"}
	otherTool := map[string]string{"tool": "codex", "variant": "test", "model": "test"}

	// A model outside the pool without an approval is refused, reason or not.
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-refused", outOfPool, "")); !failed ||
		!strings.HasPrefix(text, "HTTP 409") || !strings.Contains(text, "model_not_approved") {
		t.Fatalf("spawn outside the pool without approval: failed=%v %s", failed, text)
	}
	// So is a choice outside the pool by its tool.
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-tool", otherTool, "")); !failed ||
		!strings.Contains(text, "model_not_approved") {
		t.Fatalf("spawn of a tool outside the pool: failed=%v %s", failed, text)
	}

	// An agent's message approves nothing, and neither does a system message,
	// an unknown message, or a message of another project.
	agentMessage, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "take model-b"})
	if err != nil {
		t.Fatal(err)
	}
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-agent", outOfPool, agentMessage.ID)); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "person") {
		t.Fatalf("spawn approved by an agent's message: failed=%v %s", failed, text)
	}
	systemMessage, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "notice"})
	if err != nil {
		t.Fatal(err)
	}
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-system", outOfPool, systemMessage.ID)); !failed ||
		!strings.Contains(text, "model_not_approved") {
		t.Fatalf("spawn approved by a system message: failed=%v %s", failed, text)
	}
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-unknown", outOfPool, "msg_missing")); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "no message") {
		t.Fatalf("spawn approved by an unknown message: failed=%v %s", failed, text)
	}
	resp := f.request(http.MethodPost, "/api/projects", map[string]string{"name": "other", "folder": f.t.TempDir()}, true)
	var other store.Project
	if err := json.NewDecoder(resp.Body).Decode(&other); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("other project: %d %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/projects/"+other.ID+"/rooms", nil, false)
	var otherRooms []store.Room
	if err := json.NewDecoder(resp.Body).Decode(&otherRooms); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var otherHome string
	for _, room := range otherRooms {
		if room.Kind == "home" {
			otherHome = room.ID
			break
		}
	}
	otherID, _ := personMessage(t, f, otherHome, "approve model-b over there")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-other", outOfPool, otherID)); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "another project") {
		t.Fatalf("spawn approved by another project's message: failed=%v %s", failed, text)
	}

	// A person's unrelated, expired, or unreadable message cannot authorize
	// the model, even though each is a valid message reference.
	unrelatedID, _ := personMessage(t, f, f.room.ID, "Please finish the review")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-unrelated", outOfPool, unrelatedID)); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "does not name") {
		t.Fatalf("unrelated approval: failed=%v %s", failed, text)
	}
	toolOnlyID, _ := personMessage(t, f, f.room.ID, "FAKE is fine for this task")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-tool-only", outOfPool, toolOnlyID)); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "requested model") {
		t.Fatalf("tool-only approval: failed=%v %s", failed, text)
	}
	wrongToolID, _ := personMessage(t, f, f.room.ID, "codex model-b is fine")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-wrong-tool", outOfPool, wrongToolID)); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "tool or variant") {
		t.Fatalf("wrong-tool approval: failed=%v %s", failed, text)
	}
	wrongVariantID, _ := personMessage(t, f, f.room.ID, "model-b on alternate is fine")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-wrong-variant", outOfPool, wrongVariantID)); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "tool or variant") {
		t.Fatalf("wrong-variant approval: failed=%v %s", failed, text)
	}
	oldID, _ := personMessage(t, f, f.room.ID, "MODEL-B is approved")
	if _, err := f.exec(`UPDATE message SET created_at=? WHERE id=?`, time.Now().UTC().Add(-8*24*time.Hour).Format(time.RFC3339Nano), oldID); err != nil {
		t.Fatal(err)
	}
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-old", outOfPool, oldID)); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "older than 7 days") {
		t.Fatalf("expired approval: failed=%v %s", failed, text)
	}
	hidden, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "task", Title: "private approval", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`UPDATE room SET restricted=1 WHERE id=?`, hidden.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`DELETE FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=?`, hidden.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	hiddenID, _ := personMessage(t, f, hidden.ID, "model-b approved")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-hidden", outOfPool, hiddenID)); !failed ||
		!strings.Contains(text, "model_not_approved") || !strings.Contains(text, "cannot read") {
		t.Fatalf("unreadable approval: failed=%v %s", failed, text)
	}

	// A person's message opens the gate: the out-of-pool model is no longer
	// refused with model_not_approved (the fake harness's own model list
	// refuses it later, which is not this gate's answer).
	approvedID, approvedNo := personMessage(t, f, f.room.ID, "MODEL-B is fine for this task")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-opened", outOfPool, approvedID)); !failed ||
		!strings.Contains(text, "adapter_unlisted") || strings.Contains(text, "model_not_approved") {
		t.Fatalf("spawn approved by a person's message: failed=%v %s", failed, text)
	}
	labelID, _ := personMessage(t, f, f.room.ID, "MODEL BEE is approved")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-label", outOfPool, labelID)); !failed ||
		!strings.Contains(text, "adapter_unlisted") || strings.Contains(text, "model_not_approved") {
		t.Fatalf("spawn approved by model label: failed=%v %s", failed, text)
	}

	// The approval is recorded on the agent: on the answered card, on the
	// stored card the API view serves, and on the spawn event. The message may
	// be named by its number as well as by its id.
	spawned := spawnViaTool(t, f, key, args("ma-recorded", inPool, approvedID))
	if spawned.ApprovedBy != approvedID {
		t.Fatalf("card approved_by=%q, want %q", spawned.ApprovedBy, approvedID)
	}
	byNumber := spawnViaTool(t, f, key, args("ma-number", inPool, "#"+strconv.Itoa(int(approvedNo))))
	if byNumber.ApprovedBy != approvedID {
		t.Fatalf("number approval recorded %q, want the message id %q", byNumber.ApprovedBy, approvedID)
	}
	card, err := f.db.AgentCardByID(t.Context(), spawned.ID)
	if err != nil || card.ApprovedBy != approvedID {
		t.Fatalf("stored card approved_by=%q err=%v", card.ApprovedBy, err)
	}
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.EventsAfter(t.Context(), owner.ID, 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	spawnEvent := false
	for _, event := range events {
		if event.Kind == "agent_spawn_requested" && event.AgentID == spawned.ID && strings.Contains(event.PayloadJSON, approvedID) {
			spawnEvent = true
			break
		}
	}
	if !spawnEvent {
		t.Fatal("the spawn event does not carry the approving message")
	}

	// A pool choice never needs an approval.
	spawnViaTool(t, f, key, args("ma-pool", inPool, ""))
}

// TestPersonSpawnIsNeverGated shows the model-approval gate is an agent-tool
// rule: a person's own spawn of the same role answers as it always did, and
// never with model_not_approved.
func TestPersonSpawnIsNeverGated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	seedFakePool(t, f, "worker")
	connectFakeSupervisor(t, f, map[string]string{})

	personSpawn := func(key string, choice map[string]string) (int, store.AgentCard, map[string]string) {
		t.Helper()
		body := map[string]any{"role_key": "worker", "reason": "owner choice"}
		for name, value := range choice {
			body[name] = value
		}
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents", strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		var card store.AgentCard
		encoded, _ := json.Marshal(raw)
		_ = json.Unmarshal(encoded, &card)
		refusal := map[string]string{}
		for name, value := range raw {
			if text, ok := value.(string); ok {
				refusal[name] = text
			}
		}
		return resp.StatusCode, card, refusal
	}

	status, personCard, _ := personSpawn("ma-person-1", map[string]string{"tool": "fake", "variant": "test", "model": "test"})
	if status != http.StatusCreated {
		t.Fatalf("person spawn from the pool: %d", status)
	}
	if personCard.ApprovedBy != "" {
		t.Fatalf("a person's spawn recorded an approval: %q", personCard.ApprovedBy)
	}

	// The same route with a model outside the pool is judged by the harness's
	// model list, not by the approval gate.
	status, _, refusal := personSpawn("ma-person-2", map[string]string{"tool": "fake", "variant": "test", "model": "model-b"})
	if status == http.StatusConflict && refusal["error"] == "model_not_approved" {
		t.Fatalf("a person's spawn was gated: %d %v", status, refusal)
	}
}

// TestLeadSpawnsResearcherInDispatcherFlow covers the who-spawns-researcher
// rules in the leader-dispatcher flow: the leader spawns a researcher itself
// (from the role's pool, so no approval is needed), is still refused a worker,
// and its own out-of-pool researcher spawn needs a person's approved_by just
// like any other agent's.
func TestLeadSpawnsResearcherInDispatcherFlow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	// The lead joins the seeded task room before the flow is applied: the
	// flow's join rules keep a leader out of a plain task room, so an agent
	// created afterwards could not be added to one.
	lead, key := f.createToolAgent(t, "role_leader_default")
	resp := f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/team-flow", map[string]any{"key": "leader-dispatcher", "rev": 0}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply leader-dispatcher flow: %d", resp.StatusCode)
	}
	resp.Body.Close()
	seedFakePool(t, f, "researcher")
	connectFakeSupervisor(t, f, map[string]string{lead.ID: key})
	general := generalRoom(t, f)

	args := func(requestKey, role, model, approvedBy string) map[string]any {
		out := map[string]any{"room_id": general.ID, "role_key": role, "tool": "fake", "variant": "test",
			"model": model, "reason": "the leader's own researcher", "request_key": requestKey}
		if approvedBy != "" {
			out["approved_by"] = approvedBy
		}
		return out
	}

	// Lead -> researcher from the pool: allowed, no approval needed.
	spawned := spawnViaTool(t, f, key, args("ma-lead-researcher", "researcher", "test", ""))
	if spawned.RoleKey != "researcher" {
		t.Fatalf("researcher spawn: %+v", spawned)
	}

	// Lead -> worker in this flow: refused by the flow's spawn rules, as before.
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-lead-worker", "worker", "test", "")); !failed ||
		!strings.Contains(text, "team_flow_rule") {
		t.Fatalf("lead spawned a worker in the dispatcher flow: failed=%v %s", failed, text)
	}

	// The lead's own out-of-pool researcher spawn is gated like any other:
	// refused without a person's approval, past the gate with one (the fake
	// harness's adapter list refuses the model later, which is not this gate's
	// answer).
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-lead-gated", "researcher", "model-b", "")); !failed ||
		!strings.Contains(text, "model_not_approved") {
		t.Fatalf("lead's out-of-pool researcher spawn without approval: failed=%v %s", failed, text)
	}
	approvedID, _ := personMessage(t, f, f.room.ID, "model-b is fine for this research")
	if text, failed := mcpToolCall(t, f, key, "spawn_agent", args("ma-lead-approved", "researcher", "model-b", approvedID)); !failed ||
		!strings.Contains(text, "adapter_unlisted") || strings.Contains(text, "model_not_approved") {
		t.Fatalf("lead's out-of-pool researcher spawn with approval: failed=%v %s", failed, text)
	}

	// The dispatcher's spawns are unchanged: a pool choice needs no approval.
	// A key the supervisor has not re-registered can race the agent key's
	// reissue, so the spawn is retried: a machine that dropped between
	// connections answers 503 machine_unavailable with a retry invitation.
	dispatcher, dispatcherKey := f.createToolAgentNamed(t, "role_dispatcher_default", "mapdispatcher")
	connectFakeSupervisor(t, f, map[string]string{lead.ID: key, dispatcher.ID: dispatcherKey})
	spawnArgs := map[string]any{"room_id": general.ID, "role_key": "worker",
		"tool": "fake", "variant": "test", "model": "test", "reason": "the specification asked for it", "request_key": "ma-disp-worker"}
	text, failed := mcpToolCall(t, f, dispatcherKey, "spawn_agent", spawnArgs)
	if failed && strings.Contains(text, "machine_unavailable") {
		text, failed = mcpToolCall(t, f, dispatcherKey, "spawn_agent", spawnArgs)
	}
	if failed {
		t.Fatalf("dispatcher spawn from the pool: %s", text)
	}
	var dispCard store.AgentCard
	if err := json.Unmarshal([]byte(text), &dispCard); err != nil || dispCard.ID == "" {
		t.Fatalf("dispatcher spawn answer %s: %v", text, err)
	}
	if dispCard.ApprovedBy != "" {
		t.Fatalf("dispatcher pool spawn recorded an approval: %q", dispCard.ApprovedBy)
	}
}
