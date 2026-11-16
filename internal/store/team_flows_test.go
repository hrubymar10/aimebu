package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestTeamFlowMigrationMarksExistingProjectCustom(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "before-flow.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(ctx, db, 105); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_before_flow','before','/tmp',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(ctx, db, 0); err != nil {
		t.Fatal(err)
	}
	var key string
	var rev int
	var pending bool
	if err := db.QueryRow(`SELECT team_flow_key,team_flow_rev,team_flow_review_pending FROM project WHERE id='prj_before_flow'`).Scan(&key, &rev, &pending); err != nil {
		t.Fatal(err)
	}
	if key != "custom" || rev != 0 || !pending {
		t.Fatalf("migrated flow: key=%q rev=%d pending=%v", key, rev, pending)
	}
}

func TestFlowRoomHierarchyMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "flow-hierarchy.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrateUpTo(ctx, db, 108); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"has_home", "no_home"} {
		if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES (?,?,?,?)`, project, project, "/tmp/"+project, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	for _, room := range []struct {
		id, project, parent, kind, key string
		depth                          int
	}{
		{"home", "has_home", "", "home", "", 0},
		{"routing", "has_home", "", "task", "routing", 0},
		{"general", "has_home", "", "task", "general", 0},
		{"general_task", "has_home", "general", "task", "", 1},
		{"nested_task", "has_home", "general_task", "task", "", 2},
		{"deep_task", "has_home", "nested_task", "task", "", 3},
		{"ordinary", "has_home", "", "task", "", 0},
		{"already_child", "has_home", "home", "task", "routing", 1},
		{"orphan", "no_home", "", "task", "routing", 0},
	} {
		if _, err := db.Exec(`INSERT INTO room(id,project_id,parent_id,depth,kind,title,created_by,created_at,team_flow_room_key)
			VALUES (?,?,NULLIF(?,''),?,?,?,?,?,?)`, room.id, room.project, room.parent, room.depth, room.kind, room.id, "owner", timestamp(), room.key); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUpTo(ctx, db, 109); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		id, parent string
		depth      int
	}{
		{"routing", "home", 1}, {"general", "home", 1},
		{"general_task", "general", 2}, {"nested_task", "general_task", 3},
		{"deep_task", "nested_task", 4},
		{"ordinary", "", 0}, {"already_child", "home", 1}, {"orphan", "", 0},
	} {
		var parent sql.NullString
		var depth int
		if err := db.QueryRow(`SELECT parent_id,depth FROM room WHERE id=?`, want.id).Scan(&parent, &depth); err != nil {
			t.Fatal(err)
		}
		if parent.String != want.parent || depth != want.depth {
			t.Fatalf("%s hierarchy: parent=%q depth=%d; want parent=%q depth=%d", want.id, parent.String, depth, want.parent, want.depth)
		}
	}
}

func TestHomeDepthBudgetAllowsSpawnAtFourthStoredLevel(t *testing.T) {
	f := newMembersFixture(t)
	ctx := t.Context()
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-workers", 0, nil); err != nil {
		t.Fatal(err)
	}
	var generalID string
	if err := f.s.db.QueryRow(`SELECT id FROM room WHERE project_id=? AND team_flow_room_key='general'`, f.project.ID).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	parent := generalID
	for depth := 2; depth <= defaultMaxDepth+1; depth++ {
		room, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: parent, Kind: "task", Title: fmt.Sprintf("level-%d", depth), CreatedBy: f.alice})
		if err != nil || room.Depth != depth {
			t.Fatalf("depth %d: room=%+v err=%v", depth, room, err)
		}
		parent = room.ID
	}
	if _, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: parent, Kind: "task", Title: "too deep", CreatedBy: f.alice}); !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("room below the budget: %v", err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SpawnAgent(SpawnInput{RoomID: parent, ActorKind: "user", ActorID: f.alice,
			MachineID: "mch_members", Path: filepath.Join(f.project.Folder, "co"), RoleKey: "worker",
			Tool: "fake", Variant: "test", Model: "m", Provider: "p", KeyHash: "depth_worker"})
		return err
	}); err != nil {
		t.Fatalf("spawn in deepest allowed room: %v", err)
	}
}

func TestTeamFlowSwitchPreservesRestrictedRoom(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	routed, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var routingID string
	for _, room := range routed.CreatedRooms {
		if room.TeamFlowRoomKey == "routing" {
			routingID = room.ID
		}
	}
	if routingID == "" {
		t.Fatal("routing room missing")
	}
	var parentID string
	var depth int
	if err := f.s.db.QueryRow(`SELECT parent_id,depth FROM room WHERE id=?`, routingID).Scan(&parentID, &depth); err != nil {
		t.Fatal(err)
	}
	if parentID != f.home.ID || depth != f.home.Depth+1 {
		t.Fatalf("routing hierarchy: parent=%q depth=%d; want parent=%q depth=%d", parentID, depth, f.home.ID, f.home.Depth+1)
	}
	preview, err := f.s.PreviewTeamFlow(ctx, f.project.ID, f.alice, "custom")
	if err != nil || len(preview.PreservedRestrictedRooms) != 1 || preview.PreservedRestrictedRooms[0].RoomID != routingID {
		t.Fatalf("preview did not disclose retained restriction: %+v %v", preview, err)
	}
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "custom", routed.Rev, nil); err != nil {
		t.Fatal(err)
	}
	var restricted bool
	if err := f.s.db.QueryRow(`SELECT restricted FROM room WHERE id=?`, routingID).Scan(&restricted); err != nil || !restricted {
		t.Fatalf("routing restriction after custom flow: %v %v", restricted, err)
	}
	workers, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-workers", routed.Rev+1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT restricted FROM room WHERE id=?`, routingID).Scan(&restricted); err != nil || !restricted {
		t.Fatalf("routing restriction after worker flow: %v %v", restricted, err)
	}
	preview, err = f.s.PreviewTeamFlow(ctx, f.project.ID, f.alice, "leader-workers")
	if err != nil || len(preview.PreservedRestrictedRooms) != 1 || preview.PreservedRestrictedRooms[0].RoomID != routingID || workers.Rev != 3 {
		t.Fatalf("retained restriction preview: %+v %v", preview, err)
	}
}

func TestTeamFlowWithoutHomeCreatesRootRooms(t *testing.T) {
	f := newMembersFixture(t)
	ctx := t.Context()
	// Legacy and imported projects can lack a usable home room.
	if _, err := f.s.db.Exec(`UPDATE room SET kind='task' WHERE id=?`, f.home.ID); err != nil {
		t.Fatal(err)
	}
	applied, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.CreatedRooms) == 0 {
		t.Fatal("flow did not create its missing rooms")
	}
	for _, room := range applied.CreatedRooms {
		if room.ParentID != "" || room.Depth != 0 {
			t.Fatalf("home-less flow room is not a root: %+v", room)
		}
	}
}

func TestDispatcherFlowKeepsLeaderInGeneralAndJoinsDispatcher(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	if _, err := f.s.db.Exec(`UPDATE room SET title='general' WHERE id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	f.role(t, "role_flow_leader", f.project.ID, "leader", "Leader", "l", "singleton")
	f.role(t, "role_flow_dispatcher", f.project.ID, "dispatcher", "Dispatcher", "d", "singleton")
	leader := f.agent(t, "flowleader", "role_flow_leader", &f.other)
	task, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: f.other.ID, Kind: "task", Title: "task", CreatedBy: f.alice})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := f.agent(t, "flowdispatcher", "role_flow_dispatcher", &task)
	preview, err := f.s.PreviewTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher")
	if err != nil || len(preview.Conflicts) != 0 {
		t.Fatalf("leader in general was flagged as a conflict: %+v %v", preview.Conflicts, err)
	}
	applied, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, nil)
	if err != nil || len(applied.Conflicts) != 0 {
		t.Fatalf("apply conflicts: %+v %v", applied.Conflicts, err)
	}
	for _, id := range []string{leader.ID, dispatcher.ID} {
		var count int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, f.other.ID, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s missing from general after flow switch", id)
		}
	}
}

func TestTeamFlowStateHidesRestrictedConflictsFromViewer(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	f.role(t, "role_state_worker", f.project.ID, "worker", "Worker", "w", "multi")
	if _, err := f.s.db.Exec(`UPDATE room SET title='routing' WHERE id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	worker := f.agent(t, "stateworker", "role_state_worker", &f.other)
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, nil); err != nil {
		t.Fatal(err)
	}
	owner, err := f.s.TeamFlowState(ctx, f.project.ID, f.alice)
	if err != nil || len(owner.Conflicts) != 1 || owner.Conflicts[0].AgentID != worker.ID {
		t.Fatalf("owner conflict: %+v %v", owner.Conflicts, err)
	}
	viewer, err := f.s.TeamFlowState(ctx, f.project.ID, f.bob)
	if err != nil || len(viewer.Conflicts) != 0 || viewer.Key != "leader-dispatcher" {
		t.Fatalf("viewer disclosed restricted conflict: %+v %v", viewer, err)
	}
	if err := f.s.AddMember(ctx, f.other.ID, "user", f.bob); err != nil {
		t.Fatal(err)
	}
	viewer, err = f.s.TeamFlowState(ctx, f.project.ID, f.bob)
	if err != nil || len(viewer.Conflicts) != 1 || viewer.Conflicts[0].AgentID != worker.ID {
		t.Fatalf("joined viewer conflict: %+v %v", viewer.Conflicts, err)
	}
}

func TestRestrictedRoomAllowsOwnerAndOperatorOversight(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	if _, err := f.s.db.Exec(`UPDATE room SET restricted=1 WHERE id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{f.alice, f.carol} {
		if err := f.s.UserRoomAccess(ctx, user, f.other.ID); err != nil {
			t.Fatalf("oversight access for %s: %v", user, err)
		}
		rooms, err := f.s.RoomsForUserProject(ctx, f.project.ID, user)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, room := range rooms {
			if room.ID == f.other.ID {
				found = true
				if !room.Restricted {
					t.Fatal("restricted room list omitted its restriction flag")
				}
			}
		}
		if !found {
			t.Fatalf("restricted room absent for %s", user)
		}
	}
	if err := f.s.UserRoomAccess(ctx, f.bob, f.other.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer opened restricted room: %v", err)
	}
	rooms, err := f.s.RoomsForUserProject(ctx, f.project.ID, f.bob)
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range rooms {
		if room.ID == f.other.ID {
			t.Fatal("viewer listed restricted room")
		}
	}
}

func TestTeamFlowRemovesOnlySelectedConflictingMemberships(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	leader := f.agent(t, "flowlead", "role_leader_default", &f.other)
	preview, err := f.s.PreviewTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher")
	if err != nil {
		t.Fatal(err)
	}
	var conflict FlowConflict
	for _, item := range preview.Conflicts {
		if item.AgentID == leader.ID && item.RoomID == f.other.ID {
			conflict = item
		}
	}
	if conflict.Slug != leader.Slug || conflict.RoleKey != "leader" || conflict.RoleLabel == "" || conflict.RoomTitle != f.other.Title {
		t.Fatalf("preview must identify the selectable membership: %+v", conflict)
	}
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, nil); err != nil {
		t.Fatal(err)
	}
	var stillJoined int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, f.other.ID, leader.ID).Scan(&stillJoined); err != nil || stillJoined != 1 {
		t.Fatalf("unselected membership changed: %d %v", stillJoined, err)
	}
	selected := FlowRemoval{AgentID: leader.ID, RoomID: f.other.ID}
	applied, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 1, []FlowRemoval{selected})
	if err != nil || len(applied.Conflicts) != 0 {
		t.Fatalf("selected removal: %+v %v", applied, err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, f.other.ID, leader.ID).Scan(&stillJoined); err != nil || stillJoined != 0 {
		t.Fatalf("selected membership remains: %d %v", stillJoined, err)
	}
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 2, []FlowRemoval{selected}); !errors.Is(err, ErrTeamFlowSelection) {
		t.Fatalf("stale removal: %v", err)
	}
	state, err := f.s.TeamFlowState(ctx, f.project.ID, f.alice)
	if err != nil || state.Rev != 2 {
		t.Fatalf("stale selection must roll back: %+v %v", state, err)
	}
}

func TestTeamFlowDetectsConflictInRoomReusedAsRouting(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	f.role(t, "role_routing_worker", f.project.ID, "worker", "Worker", "w", "multi")
	if _, err := f.s.db.Exec(`UPDATE room SET title='routing' WHERE id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	worker := f.agent(t, "routingworker", "role_routing_worker", &f.other)
	preview, err := f.s.PreviewTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher")
	if err != nil {
		t.Fatal(err)
	}
	selected := FlowRemoval{AgentID: worker.ID, RoomID: f.other.ID}
	if len(preview.Conflicts) != 1 || preview.Conflicts[0].AgentID != selected.AgentID || preview.Conflicts[0].RoomID != selected.RoomID {
		t.Fatalf("reused routing room conflict: %+v", preview.Conflicts)
	}
	applied, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, []FlowRemoval{selected})
	if err != nil || len(applied.Conflicts) != 0 {
		t.Fatalf("selected removal: %+v %v", applied, err)
	}
	var key string
	var restricted bool
	if err := f.s.db.QueryRow(`SELECT team_flow_room_key,restricted FROM room WHERE id=?`, f.other.ID).Scan(&key, &restricted); err != nil || key != "routing" || !restricted {
		t.Fatalf("reused room: %q %v %v", key, restricted, err)
	}
	var joined int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_id=? AND left_at IS NULL`, f.other.ID, worker.ID).Scan(&joined); err != nil || joined != 0 {
		t.Fatalf("worker retained restricted routing membership: %d %v", joined, err)
	}
}

func TestTeamFlowIgnoresClosedRoomsForReuseAndConflicts(t *testing.T) {
	f := newMembersFixture(t)
	f.role(t, "role_archived_worker", f.project.ID, "worker", "Worker", "w", "multi")
	if _, err := f.s.db.Exec(`UPDATE room SET title='routing' WHERE id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	worker := f.agent(t, "archivedworker", "role_archived_worker", &f.other)
	if _, err := f.s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	preview, err := f.s.PreviewTeamFlow(t.Context(), f.project.ID, f.alice, "leader-dispatcher")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Conflicts) != 0 {
		t.Fatalf("archived member %s appeared in conflicts: %+v", worker.ID, preview.Conflicts)
	}
	foundRouting := false
	for _, room := range preview.CreateRooms {
		foundRouting = foundRouting || room.Key == "routing"
	}
	if !foundRouting {
		t.Fatalf("archived routing room was reused: %+v", preview.CreateRooms)
	}
}

func TestTeamFlowDuplicateRoutingTitlesUseOneRoom(t *testing.T) {
	for _, keyedSecond := range []bool{false, true} {
		name := "oldest title"
		if keyedSecond {
			name = "keyed room"
		}
		t.Run(name, func(t *testing.T) {
			f := newMembersFixture(t)
			ctx := context.Background()
			f.role(t, "role_duplicate_worker", f.project.ID, "worker", "Worker", "w", "multi")
			if _, err := f.s.db.Exec(`UPDATE room SET title='routing',created_at='2000-01-01T00:00:00Z' WHERE id=?`, f.other.ID); err != nil {
				t.Fatal(err)
			}
			second, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "routing", CreatedBy: f.alice})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec(`UPDATE room SET created_at='2001-01-01T00:00:00Z' WHERE id=?`, second.ID); err != nil {
				t.Fatal(err)
			}
			firstWorker := f.agent(t, "firstworker", "role_duplicate_worker", &f.other)
			secondWorker := f.agent(t, "secondworker", "role_duplicate_worker", &second)
			chosenRoom, chosenWorker := f.other.ID, firstWorker.ID
			otherRoom, otherWorker := second.ID, secondWorker.ID
			if keyedSecond {
				if _, err := f.s.db.Exec(`UPDATE room SET team_flow_room_key='routing' WHERE id=?`, second.ID); err != nil {
					t.Fatal(err)
				}
				chosenRoom, chosenWorker = second.ID, secondWorker.ID
				otherRoom, otherWorker = f.other.ID, firstWorker.ID
			}
			preview, err := f.s.PreviewTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher")
			if err != nil || len(preview.Conflicts) != 1 || preview.Conflicts[0].RoomID != chosenRoom || preview.Conflicts[0].AgentID != chosenWorker {
				t.Fatalf("preview chose wrong routing room: %+v %v", preview.Conflicts, err)
			}
			selected := FlowRemoval{AgentID: chosenWorker, RoomID: chosenRoom}
			applied, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, []FlowRemoval{selected})
			if err != nil || len(applied.Conflicts) != 0 {
				t.Fatalf("selected routing removal: %+v %v", applied, err)
			}
			var key string
			var restricted bool
			if err := f.s.db.QueryRow(`SELECT team_flow_room_key,restricted FROM room WHERE id=?`, chosenRoom).Scan(&key, &restricted); err != nil || key != "routing" || !restricted {
				t.Fatalf("chosen room: %q %t %v", key, restricted, err)
			}
			var joined int
			if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_id=? AND left_at IS NULL`, otherRoom, otherWorker).Scan(&joined); err != nil || joined != 1 {
				t.Fatalf("unselected worker membership: %d %v", joined, err)
			}
		})
	}
}

func TestTeamFlowRoutingAndExactReferenceGrant(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	f.role(t, "role_flow_worker", f.project.ID, "worker", "Worker", "w", "multi")
	worker := f.agent(t, "workerflow", "role_flow_worker", &f.other)
	leader := f.agent(t, "leaderflow", "role_leader_default", &f.home)
	state, err := f.s.TeamFlowState(ctx, f.project.ID, f.alice)
	if err != nil || state.Key != "" || state.Rev != 0 {
		t.Fatalf("new project state: %+v %v", state, err)
	}
	preview, err := f.s.PreviewTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher")
	if err != nil || len(preview.CreateRooms) != 2 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	applied, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, []FlowRemoval{{AgentID: leader.ID, RoomID: f.other.ID}})
	if err != nil || applied.Rev != 1 || len(applied.CreatedRooms) != 2 {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, nil); !errors.Is(err, ErrTeamFlowRev) {
		t.Fatalf("stale revision: %v", err)
	}
	var routing Room
	for _, room := range applied.CreatedRooms {
		if room.Title == "routing" {
			routing = room
		}
	}
	if routing.ID == "" {
		t.Fatal("no routing room")
	}
	if err := f.s.AddMember(ctx, routing.ID, "agent", worker.ID); !errors.Is(err, ErrTeamFlowRule) {
		t.Fatalf("worker joined routing: %v", err)
	}
	if err := f.s.AddMember(ctx, f.other.ID, "agent", leader.ID); !errors.Is(err, ErrTeamFlowRule) {
		t.Fatalf("leader joined task: %v", err)
	}
	dispatcher := f.agent(t, "dispatchflow", "role_dispatcher_default", &routing)
	if err := f.s.AddMember(ctx, f.home.ID, "agent", dispatcher.ID); !errors.Is(err, ErrTeamFlowRule) {
		t.Fatalf("dispatcher joined home: %v", err)
	}
	var inTask int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, f.other.ID, dispatcher.ID).Scan(&inTask); err != nil || inTask != 0 {
		t.Fatalf("dispatcher joined an existing task by default: %d %v", inTask, err)
	}
	var generalMembers int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member m JOIN room r ON r.id=m.room_id WHERE r.project_id=? AND r.team_flow_room_key='general' AND m.member_kind='agent' AND m.left_at IS NULL`, f.project.ID).Scan(&generalMembers); err != nil || generalMembers != 2 {
		t.Fatalf("leader and dispatcher should share general: %d %v", generalMembers, err)
	}
	duplicate, err := f.s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: GeneralRoomTitle, CreatedBy: f.alice})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, duplicate.ID, dispatcher.ID).Scan(&inTask); err != nil || inTask != 0 {
		t.Fatalf("dispatcher auto-joined an unkeyed general-titled room: %d %v", inTask, err)
	}
	if err := f.s.AddMember(ctx, f.other.ID, "agent", dispatcher.ID); err != nil {
		t.Fatal(err)
	}
	secret, err := f.s.InsertMessage(ctx, Message{RoomID: routing.ID, AuthorKind: "agent", AuthorID: dispatcher.ID, Body: "exact brief"})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := f.s.InsertMessage(ctx, Message{RoomID: routing.ID, AuthorKind: "agent", AuthorID: dispatcher.ID, Body: "other brief"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.AgentMessageByNo(ctx, worker.ID, f.project.ID, secret.No); err != nil || got.Available {
		t.Fatalf("ung ranted routing read: %+v %v", got, err)
	}
	if _, err := f.s.AgentRoomHistory(ctx, worker.ID, f.project.ID, routing.ID, 0, 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("routing history: %v", err)
	}
	source, err := f.s.InsertMessage(ctx, Message{RoomID: f.other.ID, AuthorKind: "agent", AuthorID: dispatcher.ID, Body: fmt.Sprintf("Worker, read #%d", secret.No)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.AgentMessageByNo(ctx, worker.ID, f.project.ID, secret.No); err != nil || !got.Available || got.Body != "exact brief" {
		t.Fatalf("cited routing read: %+v %v", got, err)
	}
	if got, err := f.s.AgentMessageByNo(ctx, worker.ID, f.project.ID, unrelated.No); err != nil || got.Available {
		t.Fatalf("uncited routing read: %+v %v", got, err)
	}
	if got, err := f.s.ReferencedMessageForUser(ctx, f.alice, source.ID, secret.No); err != nil || !got.Available {
		t.Fatalf("web citation: %+v %v", got, err)
	}
	if got, err := f.s.ReferencedMessageForUser(ctx, f.alice, source.ID, unrelated.No); err != nil || got.Available {
		t.Fatalf("web uncited read: %+v %v", got, err)
	}
	homeMessage, err := f.s.InsertMessage(ctx, Message{RoomID: f.home.ID, AuthorKind: "system", AuthorID: "system", Body: "public home note"})
	if err != nil {
		t.Fatal(err)
	}
	workerSource, err := f.s.InsertMessage(ctx, Message{RoomID: f.other.ID, AuthorKind: "agent", AuthorID: worker.ID, Body: fmt.Sprintf("See #%d", homeMessage.No)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.ReferencedMessageForUser(ctx, f.alice, workerSource.ID, homeMessage.No); err != nil || got.Available {
		t.Fatalf("worker created grant outside template role: %+v %v", got, err)
	}
	if _, err := f.s.db.Exec(`UPDATE room SET restricted=1 WHERE id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	privateTask, err := f.s.InsertMessage(ctx, Message{RoomID: f.other.ID, AuthorKind: "agent", AuthorID: dispatcher.ID, Body: "restricted task note"})
	if err != nil {
		t.Fatal(err)
	}
	leaderSource, err := f.s.InsertMessage(ctx, Message{RoomID: f.home.ID, AuthorKind: "agent", AuthorID: leader.ID, Body: fmt.Sprintf("See #%d", privateTask.No)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.ReferencedMessageForUser(ctx, f.alice, leaderSource.ID, privateTask.No); err != nil || got.Available {
		t.Fatalf("nonmember author created restricted grant: %+v %v", got, err)
	}
}

func TestTeamFlowSpawnAuthority(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	leader := f.agent(t, "flowlead", "role_leader_default", &f.home)
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0, nil); err != nil {
		t.Fatal(err)
	}
	var routingID string
	if err := f.s.db.QueryRow(`SELECT id FROM room WHERE project_id=? AND team_flow_room_key='routing'`, f.project.ID).Scan(&routingID); err != nil {
		t.Fatal(err)
	}
	spawn := func(actorID, roomID, role, key string) error {
		return f.s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.SpawnAgent(SpawnInput{RoomID: roomID, ActorKind: "agent", ActorID: actorID, MachineID: "mch_members", Path: filepath.Join(f.project.Folder, "co"), RoleKey: role, Tool: "fake", Variant: "test", Model: "m", Provider: "p", KeyHash: key})
			return err
		})
	}
	if err := spawn(leader.ID, f.other.ID, "worker", "flow_denied"); !errors.Is(err, ErrTeamFlowRule) {
		t.Fatalf("leader spawned worker in B: %v", err)
	}
	if err := spawn(leader.ID, routingID, "dispatcher", "flow_dispatcher"); err != nil {
		t.Fatalf("leader spawn dispatcher in B: %v", err)
	}
	var dispatcherID string
	if err := f.s.db.QueryRow(`SELECT a.id FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND r.role_key='dispatcher'`, f.project.ID).Scan(&dispatcherID); err != nil {
		t.Fatal(err)
	}
	if err := spawn(dispatcherID, f.other.ID, "worker", "flow_worker"); err != nil {
		t.Fatalf("dispatcher spawn worker in B: %v", err)
	}
	var workerID string
	var workerRev int64
	if err := f.s.db.QueryRow(`SELECT a.id,a.desired_rev FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND r.role_key='worker' ORDER BY a.created_at DESC LIMIT 1`, f.project.ID).Scan(&workerID, &workerRev); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesiredByAgent(workerID, leader.ID, "stopped", workerRev)
		return err
	}); !errors.Is(err, ErrTeamFlowRule) {
		t.Fatalf("leader stopped worker in B: %v", err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesiredByAgent(workerID, dispatcherID, "stopped", workerRev)
		return err
	}); err != nil {
		t.Fatalf("dispatcher stopped worker in B: %v", err)
	}
	var keyHash string
	if err := f.s.db.QueryRow(`SELECT key_hash FROM agent WHERE id=?`, dispatcherID).Scan(&keyHash); err != nil {
		t.Fatal(err)
	}
	identity, err := f.s.AgentToolIdentityByKeyHash(ctx, keyHash)
	if err != nil || identity.IsLead || !identity.CanSpawn || !identity.CanManage {
		t.Fatalf("dispatcher toolbox authority in B: %+v %v", identity, err)
	}
	var task Room
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		var err error
		task, err = tx.CreateTaskRoomAsTool(dispatcherID, f.project.ID, "", "delegated")
		return err
	}); err != nil {
		t.Fatalf("dispatcher create task room: %v", err)
	}
	if task.ParentID == f.home.ID {
		t.Fatalf("dispatcher task under home: %+v", task)
	}
	var leaderInTask, dispatcherInTask int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, task.ID, leader.ID).Scan(&leaderInTask); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, task.ID, dispatcherID).Scan(&dispatcherInTask); err != nil {
		t.Fatal(err)
	}
	if leaderInTask != 0 || dispatcherInTask != 1 {
		t.Fatalf("task members: leader=%d dispatcher=%d", leaderInTask, dispatcherInTask)
	}
	if err := spawn(dispatcherID, routingID, "dispatcher", "flow_denied2"); !errors.Is(err, ErrTeamFlowRule) {
		t.Fatalf("dispatcher spawned dispatcher in B: %v", err)
	}
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "custom", 1, nil); err != nil {
		t.Fatal(err)
	}
	identity, err = f.s.AgentToolIdentityByKeyHash(ctx, keyHash)
	if err != nil || identity.CanManage {
		t.Fatalf("dispatcher toolbox authority in C: %+v %v", identity, err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error { return tx.authorizeSpawnActor(f.project.ID, "agent", dispatcherID, true) }); !errors.Is(err, ErrForbidden) {
		t.Fatalf("dispatcher managed agents in C: %v", err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error { return tx.authorizeSpawnActor(f.project.ID, "agent", leader.ID, true) }); err != nil {
		t.Fatalf("leader lost C spawn authority: %v", err)
	}
}
