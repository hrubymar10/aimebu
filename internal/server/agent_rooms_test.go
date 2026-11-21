package server

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestLeadRoomToolsCreateListAndClose(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	_, workerKey := f.createToolAgentNamed(t, "role_worker_default", "worker")
	var homeID string
	allRooms, err := f.db.RoomsForProject(t.Context(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range allRooms {
		if item.Kind == "home" {
			homeID = item.ID
		}
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.AddMember(homeID, "agent", lead.ID)
	}); err != nil {
		t.Fatal(err)
	}
	create := map[string]string{"title": "New task"}
	resp := f.toolPost(t, "/api/agent/rooms", workerKey, "worker-create", create)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("worker created room: %d", resp.StatusCode)
	}
	resp = f.toolPost(t, "/api/agent/rooms", key, "create-1", create)
	var room store.Room
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || room.ParentID != homeID || room.CreatedBy != lead.ID || room.Kind != "task" {
		t.Fatalf("created status=%d room=%+v", resp.StatusCode, room)
	}
	resp = f.toolPost(t, "/api/agent/rooms", key, "create-1", create)
	var replay store.Room
	if err := json.NewDecoder(resp.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || replay.ID != room.ID {
		t.Fatalf("create replay status=%d room=%+v", resp.StatusCode, replay)
	}
	resp = f.toolPost(t, "/api/agent/rooms", key, "create-1", map[string]string{"title": "Different"})
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("key conflict status=%d", resp.StatusCode)
	}
	rooms, err := f.db.RoomsForAgent(t.Context(), lead.ID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	for _, item := range rooms {
		if item.ID == room.ID {
			joined = true
		}
	}
	if !joined {
		t.Fatalf("lead not joined to task: %+v", rooms)
	}
	notices, err := f.db.ListMessagesAfter(t.Context(), homeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, message := range notices {
		if strings.Contains(message.Body, "created task room New task") {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("create notices=%d", created)
	}
	resp = f.toolPost(t, "/api/agent/rooms/"+room.ID+"/close", workerKey, "worker-close", map[string]any{"lifecycle_rev": 0})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("worker closed room: %d", resp.StatusCode)
	}
	resp = f.toolPost(t, "/api/agent/rooms/"+room.ID+"/close", key, "close-1", map[string]any{"lifecycle_rev": 0})
	var closing store.Room
	if err := json.NewDecoder(resp.Body).Decode(&closing); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 || closing.Lifecycle != "closing" || closing.LifecycleRev != 1 {
		t.Fatalf("close status=%d room=%+v", resp.StatusCode, closing)
	}
	resp = f.toolPost(t, "/api/agent/rooms/"+room.ID+"/close", key, "close-1", map[string]any{"lifecycle_rev": 0})
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("close replay status=%d", resp.StatusCode)
	}
	resp = f.toolPost(t, "/api/agent/rooms/"+room.ID+"/close", key, "close-stale", map[string]any{"lifecycle_rev": 0})
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("stale close status=%d", resp.StatusCode)
	}
	notices, err = f.db.ListMessagesAfter(t.Context(), homeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, message := range notices {
		if strings.Contains(message.Body, "archived task room New task") {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("close notices=%d", closed)
	}
	if _, failures, err := f.db.AdvanceRoomClosures(t.Context(), 0); err != nil || len(failures) != 0 {
		t.Fatalf("final advance failures=%+v err=%v", failures, err)
	}
	archived, err := f.db.RoomByID(t.Context(), room.ID)
	if err != nil || archived.Lifecycle != "closed" {
		t.Fatalf("archive result=%+v err=%v", archived, err)
	}
}

func TestLeadCreateSubroomRequiresParentMembership(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	other, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "task", Title: "Other", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	// The lead joins every project room automatically; a room it has left is one it is not in.
	if err := f.db.RemoveMember(t.Context(), other.ID, "agent", lead.ID); err != nil {
		t.Fatal(err)
	}
	resp := f.toolPost(t, "/api/agent/rooms", key, "outside", map[string]string{"title": "Child", "parent_id": other.ID})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("unjoined parent status=%d", resp.StatusCode)
	}
	resp = f.toolPost(t, "/api/agent/rooms", key, "inside", map[string]string{"title": "Child", "parent_id": f.room.ID})
	var child store.Room
	if err := json.NewDecoder(resp.Body).Decode(&child); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || child.ParentID != f.room.ID || child.CreatedBy != lead.ID {
		t.Fatalf("child status=%d room=%+v", resp.StatusCode, child)
	}
}
