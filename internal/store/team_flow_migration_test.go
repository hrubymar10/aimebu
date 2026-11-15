package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func flowMigrationFixture(t *testing.T, key string) (*membersFixture, TeamSetupResult) {
	t.Helper()
	f := newMembersFixture(t)
	target := TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}
	in := TeamSetupInput{FlowKey: key, Leader: target}
	if key == "leader-dispatcher" {
		in.Dispatcher = &target
	}
	if key == "leader-advisor" {
		in.Advisor = &target
	}
	var result TeamSetupResult
	if err := f.s.Tx(t.Context(), func(tx *Tx) error {
		var err error
		result, err = tx.CreateTeamSetup(f.project.ID, f.alice, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f, result
}

func TestFlowMigrationAtoBKeepsLeaderAndWorkers(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	var generalID string
	if err := f.s.db.QueryRow(`SELECT id FROM room WHERE project_id=? AND team_flow_room_key='general'`, f.project.ID).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	var worker AgentCard
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		var err error
		worker, err = tx.SpawnAgent(SpawnInput{RoomID: generalID, ActorKind: "user", ActorID: f.alice, MachineID: "mch_members", Path: f.project.Folder, Slug: "worker-one", RoleKey: "worker", Tool: "fake", Variant: "test", Model: "test", Provider: "fake"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	target := &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}
	in := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev, Dispatcher: target}
	plan, err := f.s.PreviewTeamFlowMigration(ctx, f.project.ID, f.alice, TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev})
	if err != nil || plan.CanApply || len(plan.Roles) != 4 || plan.Roles[1].Status != "new" {
		t.Fatalf("plan without target: %+v %v", plan, err)
	}
	preview, err := f.s.PreviewTeamFlowMigration(ctx, f.project.ID, f.alice, in)
	if err != nil || !preview.CanApply || len(preview.Roles) != 4 || preview.Roles[0].Status != "kept" || preview.Roles[1].Status != "new" || preview.Roles[2].Status != "kept" {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	controls, err := f.s.AdvanceTeamFlowMigrations(ctx)
	if err != nil || len(controls) != 1 {
		t.Fatalf("apply controls: %+v %v", controls, err)
	}
	state, err := f.s.TeamFlowState(ctx, f.project.ID, f.alice)
	if err != nil || state.Key != "leader-dispatcher" || state.Rev != setup.Flow.Rev+1 {
		t.Fatalf("state: %+v %v", state, err)
	}
	var role string
	if err := f.s.db.QueryRow(`SELECT r.role_key FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, worker.ID).Scan(&role); err != nil || role != "worker" {
		t.Fatalf("worker: %q %v", role, err)
	}
	messages, err := f.s.ListMessagesAfter(ctx, generalID, 0)
	if err != nil {
		t.Fatal(err)
	}
	notices, summaries := 0, 0
	for _, message := range messages {
		if strings.Contains(message.Body, "The team flow changed from") {
			notices++
		}
		if strings.Contains(message.Body, "Team flow changed from") {
			summaries++
		}
	}
	if notices == 0 || summaries != 1 {
		t.Fatalf("notices=%d summaries=%d", notices, summaries)
	}
}

func TestFlowMigrationReactivatesStoppedDispatcher(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	ctx := t.Context()
	dispatcherID := setup.Agents[1].AgentID
	toA := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, toA); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE switch_handoff SET overall_deadline='2000-01-01T00:00:00.000000000Z' WHERE agent_id=?`, dispatcherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, dispatcherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var rev int64
	if err := f.s.db.QueryRow(`SELECT team_flow_rev FROM project WHERE id=?`, f.project.ID).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	target := &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "other", Provider: "fake"}
	toB := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: rev, Dispatcher: target}
	plan, err := f.s.PreviewTeamFlowMigration(ctx, f.project.ID, f.alice, TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: rev})
	if err != nil || plan.CanApply || plan.Roles[1].Status != "new" {
		t.Fatalf("reactivation plan: %+v %v", plan, err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, toB); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var desired string
	if err := f.s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, dispatcherID).Scan(&desired); err != nil || desired != "active" {
		progress, _ := f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
		t.Fatalf("dispatcher reactivated: %q %v progress=%+v", desired, err, progress)
	}
	var switchedModel string
	if err := f.s.db.QueryRow(`SELECT model FROM agent_switch WHERE agent_id=?`, dispatcherID).Scan(&switchedModel); err != nil || switchedModel != "other" {
		t.Fatalf("reactivated model switch: %q %v", switchedModel, err)
	}
	var count int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND r.role_key='dispatcher'`, f.project.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dispatcher identity not reused: %d %v", count, err)
	}
}

func TestFlowMigrationCreatesMissingLeaderFromCustom(t *testing.T) {
	f := newMembersFixture(t)
	ctx := t.Context()
	custom, err := f.s.ApplyTeamFlow(ctx, f.project.ID, f.alice, "custom", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}
	in := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: custom.Rev, Leader: target}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	controls, err := f.s.AdvanceTeamFlowMigrations(ctx)
	if err != nil || len(controls) != 1 || controls[0].RoleKey != "leader" {
		t.Fatalf("new leader controls: %+v %v", controls, err)
	}
	var key string
	if err := f.s.db.QueryRow(`SELECT team_flow_key FROM project WHERE id=?`, f.project.ID).Scan(&key); err != nil || key != "leader-workers" {
		t.Fatalf("flow after new leader: %q %v", key, err)
	}
}

func TestFlowMigrationUnexpectedHandoffFailsOnlyMigration(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	ctx := t.Context()
	id := setup.Agents[1].AgentID
	in := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		active, err := tx.AgentInActiveFlowMigration(id)
		if err != nil || !active {
			t.Fatalf("active switch guard: %v %v", active, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE switch_handoff SET status='started' WHERE agent_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatalf("dispatcher must continue: %v", err)
	}
	progress, err := f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
	if err != nil || progress.State != "failed" || progress.Error == "" {
		t.Fatalf("failed progress: %+v %v", progress, err)
	}
}

func TestFlowMigrationApplyingDeadline(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	target := &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}
	in := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev, Dispatcher: target}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE team_flow_migration SET state='applying',applied_at=? WHERE project_id=?`, time.Now().UTC().Add(-16*time.Minute).Format(timestampLayout), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE team_flow_migration_agent SET action='spawn',state='running' WHERE role_key='leader'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, setup.Agents[0].AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	progress, err := f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
	if err != nil || progress.State != "applying" {
		t.Fatalf("switch still inside its window: %+v %v", progress, err)
	}
	if _, err := f.s.db.Exec(`UPDATE team_flow_migration SET applied_at=? WHERE project_id=?`, time.Now().UTC().Add(-21*time.Minute).Format(timestampLayout), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	progress, err = f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
	if err != nil || progress.State != "failed" || progress.Error == "" {
		t.Fatalf("deadline progress: %+v %v", progress, err)
	}
}

func TestFlowMigrationPreviewSkipsStoppedDepartingRole(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	if _, err := f.s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, setup.Agents[1].AgentID); err != nil {
		t.Fatal(err)
	}
	preview, err := f.s.PreviewTeamFlowMigration(t.Context(), f.project.ID, f.alice,
		TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev})
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range preview.Roles {
		if role.RoleKey == "dispatcher" {
			t.Fatalf("stopped departing role in preview: %+v", preview.Roles)
		}
	}
}

func TestFlowMigrationStallIgnoresRecoveredLostTurn(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	leaderID := setup.Agents[0].AgentID
	var sessionID string
	if err := f.s.db.QueryRow(`SELECT id FROM session WHERE agent_id=? LIMIT 1`, leaderID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	lost, err := f.s.RecordTurn(ctx, Turn{AgentID: leaderID, SessionID: sessionID, RoomID: f.home.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='lost',settle='settled',started_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, lost.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "recovery_override", ProjectID: f.project.ID, RoomID: f.home.ID, AgentID: leaderID, TurnID: lost.ID, PayloadJSON: "{}"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RecordTurn(ctx, Turn{AgentID: leaderID, SessionID: sessionID, RoomID: f.home.ID, Attempt: 1, Mode: "normal", Exec: "running"}); err != nil {
		t.Fatal(err)
	}
	in := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev,
		Dispatcher: &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	progress, err := f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
	if err != nil || progress.StalledSince != "" {
		t.Fatalf("recovered turn must not mark a recent drain as stalled: %+v %v", progress, err)
	}
}

func TestFlowMigrationRetriesTransientDrainError(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	ctx := t.Context()
	in := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	dispatcherID := setup.Agents[1].AgentID
	if _, err := f.s.db.Exec(`UPDATE agent SET observed='idle',recovery_block=0 WHERE id=?`, dispatcherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE switch_handoff SET status='waiting' WHERE agent_id=?`, dispatcherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_migration_drain BEFORE UPDATE ON team_flow_migration_agent WHEN NEW.state='draining' BEGIN SELECT RAISE(ABORT,'temporary drain failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts int
	if err := f.s.db.QueryRow(`SELECT state,drain_errors FROM team_flow_migration WHERE project_id=?`, f.project.ID).Scan(&state, &attempts); err != nil || state != "draining" || attempts != 1 {
		t.Fatalf("transient error state=%q attempts=%d err=%v", state, attempts, err)
	}
	var handoffState string
	var handoffTurnCount int
	if err := f.s.db.QueryRow(`SELECT status FROM switch_handoff WHERE agent_id=?`, dispatcherID).Scan(&handoffState); err != nil || handoffState != "waiting" {
		t.Fatalf("handoff write escaped savepoint: status=%q err=%v", handoffState, err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM switch_handoff_turn WHERE agent_id=?`, dispatcherID).Scan(&handoffTurnCount); err != nil || handoffTurnCount != 0 {
		t.Fatalf("handoff turn escaped savepoint: count=%d err=%v", handoffTurnCount, err)
	}
	if _, err := f.s.db.Exec(`DROP TRIGGER fail_migration_drain`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT state,drain_errors FROM team_flow_migration WHERE project_id=?`, f.project.ID).Scan(&state, &attempts); err != nil || state != "draining" || attempts != 0 {
		t.Fatalf("recovered drain state=%q attempts=%d err=%v", state, attempts, err)
	}
}

func TestFlowMigrationDrainErrorStopsAfterThreeAttempts(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	ctx := t.Context()
	in := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_migration_drain BEFORE UPDATE ON team_flow_migration_agent WHEN NEW.state='draining' BEGIN SELECT RAISE(ABORT,'temporary drain failure'); END`); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= flowMigrationDrainErrorLimit; attempt++ {
		if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
			t.Fatal(err)
		}
		var state string
		var count int
		if err := f.s.db.QueryRow(`SELECT state,drain_errors FROM team_flow_migration WHERE project_id=?`, f.project.ID).Scan(&state, &count); err != nil {
			t.Fatal(err)
		}
		want := "draining"
		if attempt == flowMigrationDrainErrorLimit {
			want = "failed"
		}
		if state != want || count != attempt {
			t.Fatalf("attempt %d state=%q count=%d want=%q", attempt, state, count, want)
		}
	}
}

func TestFlowMigrationFailedDrainCancelsPendingHandoff(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	ctx := t.Context()
	dispatcherID := setup.Agents[1].AgentID
	in := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	var turnID string
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		var roomID string
		if err := tx.tx.QueryRowContext(ctx, `SELECT room_id FROM switch_handoff WHERE agent_id=?`, dispatcherID).Scan(&roomID); err != nil {
			return err
		}
		return tx.beginSwitchHandoffTurn(dispatcherID, roomID, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT turn_id FROM switch_handoff WHERE agent_id=?`, dispatcherID).Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE switch_handoff SET status='started' WHERE agent_id=?`, dispatcherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var migrationState, handoffState, turnExec string
	if err := f.s.db.QueryRow(`SELECT state FROM team_flow_migration WHERE project_id=?`, f.project.ID).Scan(&migrationState); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT status FROM switch_handoff WHERE agent_id=?`, dispatcherID).Scan(&handoffState); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT exec FROM turn WHERE id=?`, turnID).Scan(&turnExec); err != nil {
		t.Fatal(err)
	}
	if migrationState != "failed" || handoffState != "cancelled" || turnExec != "killed" {
		t.Fatalf("failed drain left handoff active: migration=%q handoff=%q turn=%q", migrationState, handoffState, turnExec)
	}
}

func TestFlowMigrationCancelAndArchivedRefusal(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	if _, err := f.s.PreviewTeamFlowMigration(ctx, f.project.ID, f.alice, TeamFlowMigrationInput{FlowKey: "custom", FlowRev: setup.Flow.Rev}); !errors.Is(err, ErrTeamFlowMigration) {
		t.Fatalf("custom uses ordinary flow route: %v", err)
	}
	target := &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}
	in := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev, Dispatcher: target}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	progress, err := f.s.CancelTeamFlowMigration(ctx, f.project.ID, f.alice)
	if err != nil || progress.State != "cancelled" {
		t.Fatalf("cancel: %+v %v", progress, err)
	}
	state, err := f.s.TeamFlowState(ctx, f.project.ID, f.alice)
	if err != nil || state.Key != "leader-workers" || state.Rev != setup.Flow.Rev {
		t.Fatalf("state after cancel: %+v %v", state, err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE project SET archived_at=? WHERE id=?`, timestamp(), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PreviewTeamFlowMigration(ctx, f.project.ID, f.alice, in); !errors.Is(err, ErrProjectArchived) {
		t.Fatalf("archived preview: %v", err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); !errors.Is(err, ErrProjectArchived) {
		t.Fatalf("archived apply: %v", err)
	}
}

func TestFlowMigrationBtoAHandsDispatcherToLeader(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	ctx := t.Context()
	leaderID, dispatcherID := setup.Agents[0].AgentID, setup.Agents[1].AgentID
	if err := f.s.AddMember(ctx, f.other.ID, "agent", dispatcherID); err != nil {
		t.Fatal(err)
	}
	in := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev}
	preview, err := f.s.PreviewTeamFlowMigration(ctx, f.project.ID, f.alice, in)
	if err != nil || len(preview.Roles) != 4 || preview.Roles[1].RoleKey != "dispatcher" || preview.Roles[1].Status != "leaving" {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE switch_handoff SET overall_deadline='2000-01-01T00:00:00.000000000Z' WHERE agent_id=?`, dispatcherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var key, desired string
	if err := f.s.db.QueryRow(`SELECT team_flow_key FROM project WHERE id=?`, f.project.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, dispatcherID).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if key != "leader-workers" || desired != "stopped" {
		t.Fatalf("flow=%q dispatcher desired=%q", key, desired)
	}
	var member bool
	if err := f.s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`, f.other.ID, leaderID).Scan(&member); err != nil || !member {
		t.Fatalf("leader task-room join=%v %v", member, err)
	}
	var generalID string
	if err := f.s.db.QueryRow(`SELECT id FROM room WHERE project_id=? AND team_flow_room_key='general'`, f.project.ID).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	messages, err := f.s.ListMessagesAfter(ctx, generalID, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		found = found || strings.Contains(message.Body, "handed off to the leader")
		if strings.Contains(message.Body, "current_task") {
			t.Fatal("handoff content leaked to general")
		}
	}
	if !found {
		t.Fatal("no leader handoff in general")
	}
	var routingID string
	if err := f.s.db.QueryRow(`SELECT id FROM room WHERE project_id=? AND team_flow_room_key='routing'`, f.project.ID).Scan(&routingID); err != nil {
		t.Fatal(err)
	}
	routingMessages, err := f.s.ListMessagesAfter(ctx, routingID, 0)
	if err != nil {
		t.Fatal(err)
	}
	privateHandoff := false
	for _, message := range routingMessages {
		privateHandoff = privateHandoff || strings.Contains(message.Body, "current_task")
	}
	if !privateHandoff {
		t.Fatal("handoff content missing from routing")
	}
}

func TestFlowMigrationHandoffFallsBackToAgentDM(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	ctx := t.Context()
	leaderID, dispatcherID := setup.Agents[0].AgentID, setup.Agents[1].AgentID
	var routingID string
	if err := f.s.db.QueryRow(`SELECT id FROM room WHERE project_id=? AND team_flow_room_key='routing'`, f.project.ID).Scan(&routingID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RemoveMember(ctx, routingID, "agent", dispatcherID); err != nil {
		t.Fatal(err)
	}
	in := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE switch_handoff SET overall_deadline='2000-01-01T00:00:00.000000000Z' WHERE agent_id=?`, dispatcherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var dmID string
	if err := f.s.db.QueryRow(`SELECT r.id FROM room r JOIN room_member a ON a.room_id=r.id AND a.member_id=? AND a.left_at IS NULL
		JOIN room_member b ON b.room_id=r.id AND b.member_id=? AND b.left_at IS NULL WHERE r.kind='dm' LIMIT 1`, leaderID, dispatcherID).Scan(&dmID); err != nil {
		t.Fatal(err)
	}
	messages, err := f.s.ListMessagesAfter(ctx, dmID, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		found = found || strings.Contains(message.Body, "current_task")
	}
	if !found {
		t.Fatal("handoff details missing from private DM")
	}
}

func TestFlowMigrationKeptAgentModelSwitch(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	change := TeamFlowMigrationChange{AgentID: setup.Agents[0].AgentID,
		Target: TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "other", Provider: "fake"}}
	in := TeamFlowMigrationInput{FlowKey: "leader-advisor", FlowRev: setup.Flow.Rev,
		Advisor: &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}, Changes: []TeamFlowMigrationChange{change}}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var state, model string
	if err := f.s.db.QueryRow(`SELECT state,model FROM agent_switch WHERE agent_id=?`, change.AgentID).Scan(&state, &model); err != nil {
		t.Fatal(err)
	}
	if state != "draining" || model != "other" {
		t.Fatalf("switch=%q model=%q", state, model)
	}
}

func TestFlowMigrationWaitsForCurrentTurnAndCancels(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	leaderID := setup.Agents[0].AgentID
	var sessionID string
	if err := f.s.db.QueryRow(`SELECT id FROM session WHERE agent_id=? LIMIT 1`, leaderID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RecordTurn(ctx, Turn{AgentID: leaderID, SessionID: sessionID, Generation: 0, RoomID: f.home.ID, Attempt: 1, Mode: "normal", Exec: "running"}); err != nil {
		t.Fatal(err)
	}
	in := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev,
		Dispatcher: &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	if controls, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil || len(controls) != 0 {
		t.Fatalf("draining: %+v %v", controls, err)
	}
	progress, err := f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
	if err != nil || progress.State != "draining" || !progress.CanCancel {
		t.Fatalf("progress: %+v %v", progress, err)
	}
	if _, err := f.s.db.Exec(`UPDATE turn SET started_at='2000-01-01T00:00:00.000000000Z' WHERE agent_id=? AND exec='running'`, leaderID); err != nil {
		t.Fatal(err)
	}
	progress, err = f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
	if err != nil || progress.StalledSince == "" {
		t.Fatalf("stalled progress: %+v %v", progress, err)
	}
	if _, err := f.s.CancelTeamFlowMigration(ctx, f.project.ID, f.alice); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := f.s.db.QueryRow(`SELECT team_flow_key FROM project WHERE id=?`, f.project.ID).Scan(&key); err != nil || key != "leader-workers" {
		t.Fatalf("cancelled key=%q %v", key, err)
	}
}

func TestFlowMigrationFailedApplyLeavesFlowUnchanged(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	in := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev,
		Dispatcher: &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE project SET team_flow_rev=team_flow_rev+1 WHERE id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if controls, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil || len(controls) != 0 {
		t.Fatalf("failed apply controls=%+v err=%v", controls, err)
	}
	progress, err := f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
	if err != nil || progress.State != "failed" || progress.CanCancel {
		t.Fatalf("failed progress: %+v %v", progress, err)
	}
	var key string
	if err := f.s.db.QueryRow(`SELECT team_flow_key FROM project WHERE id=?`, f.project.ID).Scan(&key); err != nil || key != "leader-workers" {
		t.Fatalf("flow changed on failed apply: %q %v", key, err)
	}
}

func TestFlowModelRecommendationShape(t *testing.T) {
	rows := TeamFlowRecommendedModels()
	if len(rows) != 22 {
		t.Fatalf("recommendation rows: %d", len(rows))
	}
	for _, row := range rows {
		if row.FlowKey == "custom" || row.RoleKey == "" || row.Tool == "" || len(row.Models) == 0 {
			t.Fatalf("invalid row: %+v", row)
		}
		for _, model := range row.Models {
			if model.Model == "" || model.Why == "" {
				t.Fatalf("invalid model: %+v", model)
			}
		}
		if row.FlowKey == "leader-advisor" && row.RoleKey == "advisor" && len(row.Models) != 2 {
			t.Fatalf("advisor should show both catalog models: %+v", row)
		}
	}
	for _, template := range TeamFlowTemplates() {
		if template.Recommendation == "" {
			t.Fatalf("missing recommendation for %s", template.Key)
		}
	}
}
