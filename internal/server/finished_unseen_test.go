package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// An agent whose turn ended with a message for the person shows as "finished, not yet seen" on its card and on the room list, until that
// person's read cursor reaches the message. A message for another agent (a worker's completion for its coordinator) never sets it for the
// person, and a reconnect (re-reading the lists, the event cursor) leaves it as it was.
func TestFinishedUnseenCardAndRoomFollowTheReadCursor(t *testing.T) {
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "finished-unseen", "worker")
	if status != 201 {
		t.Fatalf("spawn: %d %v", status, raw)
	}
	owner := f.ownerUserID(t)
	say := func(id string, seq int, targets string) {
		t.Helper()
		if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES (?,?,?,'agent',?,'hi',?,'2026-10-16T00:00:00Z')`,
				id, seq, f.room.ID, card.ID, targets)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := func() (cardSet, roomSet bool) {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/agents", nil, false)
		defer resp.Body.Close()
		var cards []struct {
			ID             string `json:"id"`
			FinishedUnseen bool   `json:"finished_unseen"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&cards); err != nil {
			t.Fatal(err)
		}
		for _, c := range cards {
			if c.ID == card.ID {
				cardSet = c.FinishedUnseen
			}
		}
		list := f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil, false)
		defer list.Body.Close()
		var rooms []struct {
			ID             string `json:"id"`
			FinishedUnseen bool   `json:"finished_unseen"`
		}
		if err := json.NewDecoder(list.Body).Decode(&rooms); err != nil {
			t.Fatal(err)
		}
		for _, r := range rooms {
			if r.ID == f.room.ID {
				roomSet = r.FinishedUnseen
			}
		}
		return
	}
	want := func(c, r bool, why string) {
		t.Helper()
		if gc, gr := seen(); gc != c || gr != r {
			t.Fatalf("%s: card=%v room=%v, want %v and %v", why, gc, gr, c, r)
		}
	}
	want(false, false, "no message yet")
	say("msg_fu_worker", 3000, `["agt_coordinator"]`)
	want(false, false, "a worker's completion for its coordinator")
	say("msg_fu_person", 3001, `["`+owner+`"]`)
	want(true, true, "a turn that ended with a message for the person")
	// A reconnect re-reads the lists and the event cursor; neither moves the read cursor.
	resp := f.request(http.MethodGet, "/api/events/cursor", nil, false)
	resp.Body.Close()
	want(true, true, "after a reconnect")
	resp = f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/read", map[string]int64{}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("read: %d", resp.StatusCode)
	}
	want(false, false, "after the person read the room")
}
