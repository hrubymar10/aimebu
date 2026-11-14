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

func TestTeamFlowSwitchPreservesRestrictedRoom(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	routed, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0)
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
	preview, err := f.s.PreviewTeamFlow(ctx, f.project.ID, f.alice, "custom")
	if err != nil || len(preview.PreservedRestrictedRooms) != 1 || preview.PreservedRestrictedRooms[0].RoomID != routingID {
		t.Fatalf("preview did not disclose retained restriction: %+v %v", preview, err)
	}
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "custom", routed.Rev); err != nil {
		t.Fatal(err)
	}
	var restricted bool
	if err := f.s.db.QueryRow(`SELECT restricted FROM room WHERE id=?`, routingID).Scan(&restricted); err != nil || !restricted {
		t.Fatalf("routing restriction after custom flow: %v %v", restricted, err)
	}
	workers, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-workers", routed.Rev+1)
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
			found = found || room.ID == f.other.ID
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
	applied, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0)
	if err != nil || applied.Rev != 1 || len(applied.CreatedRooms) != 2 {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0); !errors.Is(err, ErrTeamFlowRev) {
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
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, f.other.ID, dispatcher.ID).Scan(&inTask); err != nil || inTask != 1 {
		t.Fatalf("dispatcher task autojoin: %d %v", inTask, err)
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
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "leader-dispatcher", 0); err != nil {
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
	if _, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "custom", 1); err != nil {
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
