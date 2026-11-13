package store

import (
	"context"
	"fmt"
	"testing"
)

// The badge on a room is a count query of its own (a project's room list asks for it, so building every inbox item for it was wasted
// work). It must still equal what the inbox lists: for every kind of item, in a project room and in a private chat, for each role.
func TestAttentionCountsMatchTheInboxEachPersonSees(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	people := map[string]string{"usr_owner": "owner", "usr_operator": "operator", "usr_viewer": "viewer", "usr_peer": "viewer"}
	for id, role := range people {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, project.ID, id, role); err != nil {
			t.Fatal(err)
		}
	}
	dm, err := s.OpenDM(ctx, "usr_peer", agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{"question", "approval", "recovery", "delivery_failed", "usage_info", "usage_limit"}
	n := 0
	for _, roomID := range []string{room.ID, dm.ID} {
		for _, kind := range kinds {
			n++
			if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,created_at)
				VALUES (?,?,?,?, 'agent',?,?,?)`, fmt.Sprintf("inb_att_%d", n), project.ID, roomID, agent.ID, agent.ID, kind, timestamp()); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A resolved item, which nobody sees.
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,state,created_at)
		VALUES ('inb_att_done',?,?,?, 'agent',?,'question','resolved',?)`, project.ID, room.ID, agent.ID, agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	for id := range people {
		entries, err := s.InboxForUser(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{}
		for _, entry := range entries {
			want[entry.RoomID]++
		}
		got, err := s.attentionByRoom(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: counts %v, inbox %v", id, got, want)
		}
		for roomID, count := range want {
			if got[roomID] != count {
				t.Fatalf("%s: room %s counts %d, inbox lists %d", id, roomID, got[roomID], count)
			}
		}
	}
	// The setup is only a test if people see different things.
	owner, _ := s.attentionByRoom(ctx, "usr_owner")
	viewer, _ := s.attentionByRoom(ctx, "usr_viewer")
	peer, _ := s.attentionByRoom(ctx, "usr_peer")
	if owner[room.ID] == 0 || viewer[room.ID] != 0 || peer[dm.ID] == 0 {
		t.Fatalf("expected different views: owner %v viewer %v peer %v", owner, viewer, peer)
	}
	state, err := s.MarkRoomRead(ctx, "usr_owner", room.ID, 0)
	if err != nil || state.AttentionCount != 0 {
		t.Fatalf("mark room read: %+v err=%v", state, err)
	}
	owner, err = s.attentionByRoom(ctx, "usr_owner")
	if err != nil || owner[room.ID] != 0 {
		t.Fatalf("seen bell stayed: %v err=%v", owner, err)
	}
	entries, err := s.InboxForUser(ctx, "usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	seen, open := 0, 0
	for _, entry := range entries {
		if entry.RoomID == room.ID {
			open++
			if entry.Seen {
				seen++
			}
		}
	}
	if open == 0 || seen != open {
		t.Fatalf("room Inbox should stay open and seen: open=%d seen=%d", open, seen)
	}
	operator, err := s.attentionByRoom(ctx, "usr_operator")
	if err != nil || operator[room.ID] == 0 {
		t.Fatalf("another person's bell changed: %v err=%v", operator, err)
	}
}
