package store

import (
	"context"
	"errors"
	"testing"
)

func TestTeamSetupCommitsOrRollsBackTogether(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	limit := 2
	input := TeamSetupInput{
		FlowKey: "leader-dispatcher", FlowRev: 0,
		Leader:     TeamSetupTarget{MachineID: "mch_members", Name: "leader-one", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"},
		Dispatcher: &TeamSetupTarget{MachineID: "mch_missing", Name: "dispatch-one", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"},
		Settings:   TeamSetupSettings{MaxLiveAgents: &limit},
	}
	if err := f.s.ValidateTeamSetup(ctx, f.project.ID, f.bob, input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer validation: %v", err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.CreateTeamSetup(f.project.ID, f.alice, input)
		return err
	}); err == nil {
		t.Fatal("expected second spawn to reject an unknown machine")
	}
	var key string
	var rev, agents, routing int
	if err := f.s.db.QueryRow(`SELECT team_flow_key,team_flow_rev FROM project WHERE id=?`, f.project.ID).Scan(&key, &rev); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM agent WHERE project_id=?`, f.project.ID).Scan(&agents); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room WHERE project_id=? AND team_flow_room_key='routing'`, f.project.ID).Scan(&routing); err != nil {
		t.Fatal(err)
	}
	if key != "" || rev != 0 || agents != 0 || routing != 0 {
		t.Fatalf("partial team escaped rollback: flow=%q rev=%d agents=%d routing=%d", key, rev, agents, routing)
	}
	input.Dispatcher.MachineID = "mch_members"
	var result TeamSetupResult
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		var err error
		result, err = tx.CreateTeamSetup(f.project.ID, f.alice, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if result.Flow.Rev != 1 || len(result.Agents) != 2 || result.Agents[0].Role != "leader" || result.Agents[1].Role != "dispatcher" {
		t.Fatalf("committed team: %+v", result)
	}
	for i, want := range []string{"leader-one", "dispatch-one"} {
		card, err := f.s.AgentCardByID(ctx, result.Agents[i].AgentID)
		if err != nil || card.Slug != want {
			t.Fatalf("agent %d name: %q %v", i, card.Slug, err)
		}
	}
	if len(result.CreatedRooms) != 2 {
		t.Fatalf("created flow rooms: %+v", result.CreatedRooms)
	}
	for _, room := range result.CreatedRooms {
		if room.ParentID != f.home.ID || room.Depth != f.home.Depth+1 {
			t.Fatalf("%s hierarchy: parent=%q depth=%d; want parent=%q depth=%d", room.TeamFlowRoomKey, room.ParentID, room.Depth, f.home.ID, f.home.Depth+1)
		}
	}
	rooms := map[string]string{"home": f.home.ID, "task": f.other.ID}
	for _, room := range result.CreatedRooms {
		rooms[room.TeamFlowRoomKey] = room.ID
	}
	member := func(roomID, agentID string) bool {
		t.Helper()
		var count int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, roomID, agentID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count != 0
	}
	leader, dispatcher := result.Agents[0].AgentID, result.Agents[1].AgentID
	if !member(rooms["home"], leader) || !member(rooms["routing"], leader) || !member(rooms["general"], leader) || member(rooms["task"], leader) {
		t.Fatal("leader should join home, routing, and general, but not task rooms")
	}
	if !member(rooms["routing"], dispatcher) || !member(rooms["general"], dispatcher) || member(rooms["home"], dispatcher) || member(rooms["task"], dispatcher) {
		t.Fatal("dispatcher should join routing and general, but not home or task rooms")
	}
	if err := f.s.ValidateTeamSetup(ctx, f.project.ID, f.alice, input); !errors.Is(err, ErrTeamFlowRev) {
		t.Fatalf("stale revision after commit: %v", err)
	}
}

func TestTeamFlowCoreRolesDoNotUseWorkerSlots(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	limit := 1
	input := TeamSetupInput{
		FlowKey: "leader-dispatcher", FlowRev: 0,
		Leader:     TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"},
		Dispatcher: &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"},
		Settings:   TeamSetupSettings{MaxLiveAgents: &limit},
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.CreateTeamSetup(f.project.ID, f.alice, input); return err }); err != nil {
		t.Fatalf("core team at worker cap one: %v", err)
	}
	headroom, err := f.s.ProjectToolHeadroom(ctx, f.project.ID)
	if err != nil || headroom.LiveAgents != 0 || headroom.LiveSlotsLeft != 1 {
		t.Fatalf("core team consumed worker slot: %+v %v", headroom, err)
	}
	spawn := func(slug string) error {
		return f.s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.SpawnAgent(SpawnInput{RoomID: f.other.ID, ActorKind: "user", ActorID: f.alice, MachineID: "mch_members", Path: f.project.Folder, RoleKey: "worker", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", Slug: slug, KeyHash: slug})
			return err
		})
	}
	if err := spawn("firstworker"); err != nil {
		t.Fatal(err)
	}
	if err := spawn("secondworker"); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("second worker bypassed cap: %v", err)
	}
	headroom, err = f.s.ProjectToolHeadroom(ctx, f.project.ID)
	if err != nil || headroom.LiveAgents != 1 || headroom.LiveSlotsLeft != 0 {
		t.Fatalf("worker cap after first spawn: %+v %v", headroom, err)
	}
}

func TestTeamSetupLeaderJoinsGeneralInWorkerFlow(t *testing.T) {
	f := newMembersFixture(t)
	input := TeamSetupInput{
		FlowKey: "leader-workers", FlowRev: 0,
		Leader: TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"},
	}
	var result TeamSetupResult
	if err := f.s.Tx(context.Background(), func(tx *Tx) error {
		var err error
		result, err = tx.CreateTeamSetup(f.project.ID, f.alice, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(result.Agents) != 1 || len(result.CreatedRooms) != 1 || result.CreatedRooms[0].TeamFlowRoomKey != "general" {
		t.Fatalf("setup result: %+v", result)
	}
	for _, roomID := range []string{f.home.ID, result.CreatedRooms[0].ID} {
		var count int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, roomID, result.Agents[0].AgentID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("leader missing from room %s", roomID)
		}
	}
}

func TestCustomFlowMetersMultiLeadRoles(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	f.role(t, "role_multi_lead_cap", f.project.ID, "multi-lead", "Multi lead", "m", "multi")
	f.role(t, "role_single_lead_cap", f.project.ID, "single-lead", "Single lead", "s", "singleton")
	if _, err := f.s.db.Exec(`UPDATE role SET cap_is_lead=1 WHERE id IN ('role_multi_lead_cap','role_single_lead_cap')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE project SET team_flow_key='custom' WHERE id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE settings_project SET max_live_agents=1 WHERE project_id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	f.agent(t, "singleleadcap", "role_single_lead_cap", &f.home)
	headroom, err := f.s.ProjectToolHeadroom(ctx, f.project.ID)
	if err != nil || headroom.LiveAgents != 0 || headroom.LiveSlotsLeft != 1 {
		t.Fatalf("singleton lead used a worker slot: %+v %v", headroom, err)
	}
	f.agent(t, "multileadcap", "role_multi_lead_cap", &f.other)
	headroom, err = f.s.ProjectToolHeadroom(ctx, f.project.ID)
	if err != nil || headroom.LiveAgents != 1 || headroom.LiveSlotsLeft != 0 {
		t.Fatalf("multi lead did not use a worker slot: %+v %v", headroom, err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SpawnAgent(SpawnInput{RoomID: f.other.ID, ActorKind: "user", ActorID: f.alice, MachineID: "mch_members", Path: f.project.Folder, RoleKey: "worker", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", Slug: "cappedworker", KeyHash: "cappedworker"})
		return err
	}); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("multi lead did not enforce worker cap: %v", err)
	}
}
