package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

// flowAgent makes an agent with a toolbox key, spawned by createdBy, whose home room is home (and which joins it).
func (f *fixture) flowAgent(t *testing.T, roleID, slug, createdBy string, home store.Room) (store.Agent, string) {
	t.Helper()
	var agent store.Agent
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_flow',?,?,'/tmp/flow')`, f.project.ID, f.machineID); err != nil {
			return err
		}
		var err error
		agent, err = tx.CreateAgent(store.Agent{Slug: slug, ProjectID: f.project.ID, RoleID: roleID, Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: "chk_flow", KeyHash: "placeholder-" + slug, CreatedBy: createdBy})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE agent SET home_room_id=? WHERE id=?`, home.ID, agent.ID); err != nil {
			return err
		}
		return tx.AddMember(home.ID, "agent", agent.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.srv.Auth.IssueAgentKey(context.Background(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return agent, key
}

func (f *fixture) toolDelete(t *testing.T, path, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, f.http.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeInto(t *testing.T, resp *http.Response, value any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(value); err != nil {
		t.Fatal(err)
	}
}

func statusOf(resp *http.Response) int {
	resp.Body.Close()
	return resp.StatusCode
}

// TestDispatcherCanDispatch walks the leader-dispatcher flow: the dispatcher joins the task room it creates, messages
// and schedules for the worker it spawned there, deletes the schedule, and closes the room (stopping the worker).
// The leader keeps close and stop over the room and workers it started before the switch.
func TestDispatcherCanDispatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := t.Context()
	lead, leadKey := f.createToolAgentNamed(t, "role_leader_default", "lead")

	// Before the switch, the leader opens a task room and puts two workers to work: one it spawned, one someone else did.
	var leadRoom store.Room
	resp := f.toolPost(t, "/api/agent/rooms", leadKey, "lead-room", map[string]any{"title": "before the switch"})
	if resp.StatusCode != 201 {
		t.Fatalf("lead create_room: %d", statusOf(resp))
	}
	decodeInto(t, resp, &leadRoom)
	var otherRoom store.Room
	resp = f.toolPost(t, "/api/agent/rooms", leadKey, "lead-room-2", map[string]any{"title": "before the switch, too"})
	if resp.StatusCode != 201 {
		t.Fatalf("lead create_room 2: %d", statusOf(resp))
	}
	decodeInto(t, resp, &otherRoom)
	ownWorker, _ := f.flowAgent(t, "role_worker_default", "ownworker", lead.ID, leadRoom)
	roomWorker, _ := f.flowAgent(t, "role_worker_default", "roomworker", "test", leadRoom)

	resp = f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/team-flow", map[string]any{"key": "leader-dispatcher", "rev": 0}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("apply flow: %d", statusOf(resp))
	}
	resp.Body.Close()
	var routing store.Room
	rooms, err := f.db.RoomsForProject(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range rooms {
		if room.TeamFlowRoomKey == "routing" {
			routing = room
		}
	}
	if routing.ID == "" {
		t.Fatal("no routing room")
	}
	dispatcher, dispatcherKey := f.flowAgent(t, "role_dispatcher_default", "disp", lead.ID, routing)

	// (1) create_room joins the dispatcher and says so in member_count.
	var task store.Room
	resp = f.toolPost(t, "/api/agent/rooms", dispatcherKey, "disp-room", map[string]any{"title": "delegated"})
	if resp.StatusCode != 201 {
		t.Fatalf("dispatcher create_room: %d", statusOf(resp))
	}
	decodeInto(t, resp, &task)
	if task.MemberCount < 1 {
		t.Fatalf("create_room response member_count=%d", task.MemberCount)
	}
	if joined, err := f.db.AgentInRoom(ctx, dispatcher.ID, task.ID); err != nil || !joined {
		t.Fatalf("dispatcher not in its task room: %v %v", joined, err)
	}
	if joined, err := f.db.AgentInRoom(ctx, lead.ID, task.ID); err != nil || joined {
		t.Fatalf("leader joined the dispatcher's task room: %v %v", joined, err)
	}
	worker, workerKey := f.flowAgent(t, "role_worker_default", "worker", dispatcher.ID, task)

	// (2) dm_agent reaches the dispatcher's worker, but not the leader or another project.
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_disp',?,0,'fresh_with_history',?)`, dispatcher.ID, "2026-01-01T00:00:00.000Z"); err != nil {
			return err
		}
		_, err := tx.RecordTurn(store.Turn{AgentID: dispatcher.ID, SessionID: "ses_disp", Generation: 0, RoomID: routing.ID, Attempt: 1, Mode: "normal", Exec: "running"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var sent store.AgentDMResult
	resp = f.toolPost(t, "/api/agent/dms", dispatcherKey, "dm-1", map[string]string{"target_agent_id": worker.ID, "body": "your brief"})
	if resp.StatusCode != 200 {
		t.Fatalf("dispatcher dm_agent to its worker: %d", statusOf(resp))
	}
	decodeInto(t, resp, &sent)
	if sent.RoomID == "" {
		t.Fatalf("dm result: %+v", sent)
	}
	if status := statusOf(f.toolPost(t, "/api/agent/dms", dispatcherKey, "dm-2", map[string]string{"target_agent_id": lead.ID, "body": "hi"})); status != 403 {
		t.Fatalf("dispatcher dm_agent to the leader: %d", status)
	}
	_, peer := f.otherProjectAgent(t)
	if status := statusOf(f.toolPost(t, "/api/agent/dms", dispatcherKey, "dm-3", map[string]string{"target_agent_id": peer.ID, "body": "hi"})); status != 403 {
		t.Fatalf("dispatcher dm_agent to another project: %d", status)
	}
	if status := statusOf(f.toolPost(t, "/api/agent/dms", workerKey, "dm-4", map[string]string{"target_agent_id": dispatcher.ID, "body": "hi"})); status != 403 {
		t.Fatalf("worker dm_agent: %d", status)
	}

	// (3) Schedules: create, list and delete in the dispatcher's room; a worker may not; a person's schedule stays theirs.
	schedules := "/api/agent/rooms/" + task.ID + "/schedules"
	body := map[string]any{"target_agent_id": worker.ID, "body": "status?", "kind": "interval", "timezone": "UTC", "interval_seconds": 300}
	var item store.Schedule
	resp = f.toolPost(t, schedules, dispatcherKey, "sch-1", body)
	if resp.StatusCode != 201 {
		t.Fatalf("dispatcher create_schedule: %d", statusOf(resp))
	}
	decodeInto(t, resp, &item)
	if status := statusOf(f.toolPost(t, schedules, workerKey, "sch-2", body)); status != 403 {
		t.Fatalf("worker create_schedule: %d", status)
	}
	var listed struct {
		Schedules []store.Schedule `json:"schedules"`
	}
	decodeInto(t, f.toolRequest(t, schedules, dispatcherKey), &listed)
	if len(listed.Schedules) != 1 {
		t.Fatalf("listed: %+v", listed)
	}
	if status := statusOf(f.toolDelete(t, schedules+"/"+item.ID, workerKey)); status != 403 {
		t.Fatalf("worker delete_schedule: %d", status)
	}
	if status := statusOf(f.toolDelete(t, schedules+"/"+item.ID, dispatcherKey)); status != 200 {
		t.Fatalf("dispatcher delete_schedule: %d", status)
	}
	if status := statusOf(f.toolDelete(t, schedules+"/"+item.ID, dispatcherKey)); status != 404 {
		t.Fatalf("delete again: %d", status)
	}
	var human store.Schedule
	resp = f.request(http.MethodPost, "/api/rooms/"+task.ID+"/schedules", body, true)
	if resp.StatusCode != 201 {
		t.Fatalf("person create schedule: %d", statusOf(resp))
	}
	decodeInto(t, resp, &human)
	if status := statusOf(f.toolDelete(t, schedules+"/"+human.ID, dispatcherKey)); status != 403 {
		t.Fatalf("dispatcher deleted a person's schedule: %d", status)
	}

	// (4) The dispatcher may close a task room under home that the leader made before the switch, but may not close the
	// flow's own rooms, and may not stop the leader's pre-switch worker. The leader keeps close and stop over what it
	// started before the switch.
	resp = f.toolPost(t, "/api/agent/rooms/"+otherRoom.ID+"/close", dispatcherKey, "close-lead-room", map[string]any{"lifecycle_rev": otherRoom.LifecycleRev})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("dispatcher close_room on a pre-switch room under home: %d", statusOf(resp))
	}
	resp.Body.Close()
	for _, flowRoom := range rooms {
		if flowRoom.TeamFlowRoomKey == "" {
			continue
		}
		if status := statusOf(f.toolPost(t, "/api/agent/rooms/"+flowRoom.ID+"/close", dispatcherKey, "close-flow-"+flowRoom.TeamFlowRoomKey, map[string]any{"lifecycle_rev": flowRoom.LifecycleRev})); status != 409 {
			t.Fatalf("dispatcher closed flow room %s: %d", flowRoom.TeamFlowRoomKey, status)
		}
	}
	stop := func(key, requestKey string, target store.Agent) int {
		card, err := f.db.AgentCardByID(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		return statusOf(f.toolPost(t, "/api/agent/agents/"+target.ID+"/stop", key, requestKey, map[string]any{"rev": card.DesiredRev}))
	}
	if status := stop(leadKey, "lead-stop-disp-worker", worker); status != 409 {
		t.Fatalf("leader stopped the dispatcher's worker: %d", status)
	}
	if status := stop(leadKey, "lead-stop-own", ownWorker); status != 200 {
		t.Fatalf("leader stop_agent on its pre-switch worker: %d", status)
	}
	resp = f.toolPost(t, "/api/agent/rooms/"+leadRoom.ID+"/close", leadKey, "close-own", map[string]any{"lifecycle_rev": leadRoom.LifecycleRev})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("leader close_room on its pre-switch room: %d", statusOf(resp))
	}
	resp.Body.Close()
	desired := func(agent store.Agent) string {
		card, err := f.db.AgentCardByID(ctx, agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		return card.Desired
	}
	if got := desired(roomWorker); got == "active" {
		t.Fatalf("closing the leader's room left its worker %s", got)
	}

	// The dispatcher closes its own room, which winds its worker down; it can also close a room it created after leaving it.
	resp = f.toolPost(t, "/api/agent/rooms/"+task.ID+"/close", dispatcherKey, "close-task", map[string]any{"lifecycle_rev": task.LifecycleRev})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("dispatcher close_room: %d", statusOf(resp))
	}
	resp.Body.Close()
	if got := desired(worker); got == "active" {
		t.Fatalf("closing the dispatcher's room left its worker %s", got)
	}
	var second store.Room
	resp = f.toolPost(t, "/api/agent/rooms", dispatcherKey, "disp-room-2", map[string]any{"title": "left behind"})
	if resp.StatusCode != 201 {
		t.Fatalf("second create_room: %d", statusOf(resp))
	}
	decodeInto(t, resp, &second)
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE room_member SET left_at=? WHERE room_id=? AND member_id=?`, "2026-01-01T00:00:00.000Z", second.ID, dispatcher.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.toolPost(t, "/api/agent/rooms/"+second.ID+"/close", dispatcherKey, "close-second", map[string]any{"lifecycle_rev": second.LifecycleRev})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("dispatcher close_room on a room it created and left: %d", statusOf(resp))
	}
	resp.Body.Close()
}
