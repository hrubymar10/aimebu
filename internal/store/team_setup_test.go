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
		Leader:     TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"},
		Dispatcher: &TeamSetupTarget{MachineID: "mch_missing", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"},
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
	if err := f.s.ValidateTeamSetup(ctx, f.project.ID, f.alice, input); !errors.Is(err, ErrTeamFlowRev) {
		t.Fatalf("stale revision after commit: %v", err)
	}
}
