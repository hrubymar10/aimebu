package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestSpawnJoinParent covers the explicit join_parent option: a lead or the dispatcher can spawn a worker into a task room and also put it,
// quietly, into the room's parent (general) so it can post findings there and wake the leader by mention.
func TestSpawnJoinParent(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	leader := f.agent(t, "joinlead", "role_leader_default", &f.home)
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, nil); err != nil {
		t.Fatal(err)
	}
	var generalID, routingID string
	if err := f.s.db.QueryRow(`SELECT id FROM room WHERE project_id=? AND team_flow_room_key='general'`, f.project.ID).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT id FROM room WHERE project_id=? AND team_flow_room_key='routing'`, f.project.ID).Scan(&routingID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.AddMember(ctx, generalID, "agent", leader.ID); err != nil {
		t.Fatal(err)
	}
	spawn := func(actorID, roomID string, joinParent bool, key string) error {
		return f.s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.SpawnAgent(SpawnInput{RoomID: roomID, ActorKind: "agent", ActorID: actorID, MachineID: "mch_members", Path: filepath.Join(f.project.Folder, "co"),
				RoleKey: "worker", Tool: "fake", Variant: "test", Model: "m", Provider: "p", KeyHash: key, JoinParent: joinParent})
			return err
		})
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SpawnAgent(SpawnInput{RoomID: routingID, ActorKind: "agent", ActorID: leader.ID, MachineID: "mch_members", Path: filepath.Join(f.project.Folder, "co"),
			RoleKey: "dispatcher", Tool: "fake", Variant: "test", Model: "m", Provider: "p", KeyHash: "jp_dispatcher"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var dispatcherID string
	if err := f.s.db.QueryRow(`SELECT a.id FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND r.role_key='dispatcher'`, f.project.ID).Scan(&dispatcherID); err != nil {
		t.Fatal(err)
	}
	task, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: generalID, Kind: "task", Title: "review", CreatedBy: dispatcherID})
	if err != nil {
		t.Fatal(err)
	}
	agentID := func(key string) string {
		t.Helper()
		var id string
		if err := f.s.db.QueryRow(`SELECT id FROM agent WHERE key_hash=?`, key).Scan(&id); err != nil {
			t.Fatalf("agent %s: %v", key, err)
		}
		return id
	}
	membership := func(room, agent string) (count, auto int) {
		t.Helper()
		if err := f.s.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(auto),0) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, room, agent).Scan(&count, &auto); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Without the option the worker is in its task room only: it cannot post in general.
	if err := spawn(dispatcherID, task.ID, false, "jp_plain"); err != nil {
		t.Fatal(err)
	}
	if n, _ := membership(generalID, agentID("jp_plain")); n != 0 {
		t.Fatal("worker spawned without join_parent is in general")
	}

	// With it, the worker is a quiet member of general as well.
	if err := spawn(dispatcherID, task.ID, true, "jp_review"); err != nil {
		t.Fatalf("dispatcher spawn with join_parent: %v", err)
	}
	review := agentID("jp_review")
	if n, auto := membership(task.ID, review); n != 1 || auto != 0 {
		t.Fatalf("task room membership n=%d auto=%d", n, auto)
	}
	if n, auto := membership(generalID, review); n != 1 || auto != 1 {
		t.Fatalf("general membership n=%d auto=%d, want one quiet member", n, auto)
	}
	if err := spawn(leader.ID, task.ID, true, "jp_lead"); err == nil || errors.Is(err, ErrJoinParentRefused) || errors.Is(err, ErrForbidden) {
		// the leader may not spawn a worker in this flow (ErrTeamFlowRule); the point is that it is not a join_parent problem.
		if !errors.Is(err, ErrTeamFlowRule) {
			t.Fatalf("leader spawn: %v", err)
		}
	}

	// The worker's mention of the leader in general wakes the leader; an unaddressed person's message does not wake the worker.
	var slug string
	if err := f.s.db.QueryRow(`SELECT slug FROM agent WHERE id=?`, leader.ID).Scan(&slug); err != nil {
		t.Fatal(err)
	}
	finding, err := f.s.InsertMessage(ctx, Message{RoomID: generalID, AuthorKind: "agent", AuthorID: review, Body: "@" + slug + " findings: none"})
	if err != nil {
		t.Fatal(err)
	}
	var woken int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND message_id=?`, leader.ID, finding.ID).Scan(&woken); err != nil || woken != 1 {
		t.Fatalf("leader woken by the mention: %d %v", woken, err)
	}
	hello, err := f.s.InsertMessage(ctx, Message{RoomID: generalID, AuthorKind: "user", AuthorID: f.alice, Body: "hello everyone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND message_id=?`, review, hello.ID).Scan(&woken); err != nil || woken != 0 {
		t.Fatalf("worker woken by an unaddressed message in general: %d %v", woken, err)
	}

	// A worker cannot ask for it.
	if err := spawn(review, task.ID, true, "jp_by_worker"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("worker spawn with join_parent: %v", err)
	}

	// A parent the flow keeps the role out of (routing), the home room, or no parent: 409, and the spawn leaves nothing behind.
	routingTask, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: routingID, Kind: "task", Title: "under routing", CreatedBy: dispatcherID})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, room string }{{"routing parent", routingTask.ID}, {"home parent", f.other.ID}} {
		if err := spawn(dispatcherID, c.room, true, "jp_refused_"+c.name); !errors.Is(err, ErrJoinParentRefused) {
			t.Fatalf("%s: %v", c.name, err)
		}
		var n int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM agent WHERE key_hash=?`, "jp_refused_"+c.name).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s left an agent behind: %d %v", c.name, n, err)
		}
	}
}
