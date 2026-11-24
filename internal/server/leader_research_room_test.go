package server

import (
	"net/http"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// TestLeaderCreatesResearchRoomsInDispatcherFlow: in the leader-dispatcher
// flow the leader may create research rooms through create_room; every other
// purpose stays with the dispatcher. Other flows are unchanged.
func TestLeaderCreatesResearchRoomsInDispatcherFlow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := t.Context()
	_, leadKey := f.createToolAgentNamed(t, "role_leader_default", "lead")

	// Another flow (the seeded one): the lead creates task and research rooms as before.
	for i, purpose := range []string{"", "research"} {
		resp := f.toolPost(t, "/api/agent/rooms", leadKey, "pre-"+purpose+string(rune('a'+i)), map[string]any{"title": "pre " + purpose, "purpose": purpose})
		if resp.StatusCode != 201 {
			t.Fatalf("lead before the switch, purpose %q: %d", purpose, statusOf(resp))
		}
	}

	resp := f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/team-flow", map[string]any{"key": "leader-dispatcher", "rev": 0}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("apply flow: %d", statusOf(resp))
	}
	resp.Body.Close()
	rooms, err := f.db.RoomsForProject(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var routing store.Room
	var generalID string
	for _, room := range rooms {
		if room.TeamFlowRoomKey == "routing" {
			routing = room
		}
		if room.TeamFlowRoomKey == "general" {
			generalID = room.ID
		}
	}
	_, dispKey := f.flowAgent(t, "role_dispatcher_default", "disp", "test", routing)

	var research store.Room
	resp = f.toolPost(t, "/api/agent/rooms", leadKey, "lead-research", map[string]any{"title": "research", "purpose": "research"})
	if resp.StatusCode != 201 {
		t.Fatalf("lead research room: %d", statusOf(resp))
	}
	decodeInto(t, resp, &research)
	if research.Purpose != "research" {
		t.Fatalf("purpose %q", research.Purpose)
	}
	resp = f.toolPost(t, "/api/agent/rooms", leadKey, "lead-research-g", map[string]any{"title": "research g", "purpose": "research", "parent_id": generalID})
	if resp.StatusCode != 201 {
		t.Fatalf("lead research room under general: %d", statusOf(resp))
	}
	resp.Body.Close()
	for _, purpose := range []string{"", "task", "general"} {
		resp = f.toolPost(t, "/api/agent/rooms", leadKey, "lead-no-"+purpose, map[string]any{"title": "nope", "purpose": purpose})
		if resp.StatusCode != 409 {
			t.Fatalf("lead room purpose %q: %d, want 409", purpose, statusOf(resp))
		}
		resp.Body.Close()
	}
	for i, purpose := range []string{"", "research"} {
		resp = f.toolPost(t, "/api/agent/rooms", dispKey, "disp-"+purpose+string(rune('a'+i)), map[string]any{"title": "disp " + purpose, "purpose": purpose})
		if resp.StatusCode != 201 {
			t.Fatalf("dispatcher purpose %q: %d", purpose, statusOf(resp))
		}
		resp.Body.Close()
	}
}
