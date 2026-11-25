package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestDMDeletionPermissionsAndFreshChat(t *testing.T) {
	w := newAdminWorld(t)
	agent, _ := w.f.createToolAgent(t, "role_worker_default")
	other, _ := w.f.createToolAgentNamed(t, "role_worker_default", "otherworker")
	open := func(kind string) store.Room {
		t.Helper()
		switch kind {
		case "person-agent":
			room, err := w.f.db.OpenDM(t.Context(), w.viewer.id, agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			return room
		case "person-person":
			room, err := w.f.db.OpenUserDM(t.Context(), w.f.project.ID, w.viewer.id, w.oper.id)
			if err != nil {
				t.Fatal(err)
			}
			return room
		default:
			var room store.Room
			err := w.f.db.Tx(t.Context(), func(tx *store.Tx) error {
				var err error
				room, _, err = tx.OpenAgentDM(agent.ID, other.ID, agent.ID)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return room
		}
	}
	cases := []struct {
		kind             string
		allowed, refused []adminActor
	}{
		{"person-agent", []adminActor{w.viewer, w.owner, w.admin}, []adminActor{w.oper, w.pOwner, w.out}},
		{"person-person", []adminActor{w.viewer, w.oper, w.owner, w.admin}, []adminActor{w.pOwner, w.out}},
		{"agent-agent", []adminActor{w.owner, w.admin, w.pOwner}, []adminActor{w.viewer, w.oper, w.out}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			for _, actor := range tc.refused {
				room := open(tc.kind)
				if got := w.call(t, actor, http.MethodDelete, "/api/rooms/"+room.ID, nil); got != 403 {
					t.Fatalf("%s refused delete: %d", actor.name, got)
				}
				if _, err := w.f.db.RoomByID(t.Context(), room.ID); err != nil {
					t.Fatal(err)
				}
			}
			for _, actor := range tc.allowed {
				room := open(tc.kind)
				if got := w.call(t, actor, http.MethodDelete, "/api/rooms/"+room.ID, nil); got != 204 {
					t.Fatalf("%s delete: %d", actor.name, got)
				}
				if _, err := w.f.db.RoomByID(t.Context(), room.ID); err == nil {
					t.Fatal("deleted room remains")
				}
				if tc.kind == "agent-agent" {
					events, err := w.f.db.EventsAfter(t.Context(), w.viewer.id, 0, 1<<62)
					if err != nil {
						t.Fatal(err)
					}
					seen := false
					for _, event := range events {
						if event.Kind == "room_removed" && strings.Contains(event.PayloadJSON, room.ID) {
							seen = true
						}
					}
					if !seen {
						t.Fatal("an eligible person's stream missed the removed agent chat")
					}
				}
				fresh := open(tc.kind)
				if fresh.ID == room.ID || fresh.MessageCount != 0 {
					t.Fatalf("chat did not start fresh: %+v", fresh)
				}
			}
		})
	}
}
