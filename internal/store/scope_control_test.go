package store

import (
	"context"
	"errors"
	"testing"
)

func scopeReviewFixture(t *testing.T) (*Store, executionFixture) {
	t.Helper()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_scope','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(context.Background(), func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_scope") }); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(context.Background(), f.room.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	return s, f
}

func applyScopeReview(t *testing.T, s *Store, kind, id, action string) ScopeControl {
	t.Helper()
	ctx := context.Background()
	state, err := s.ScopeControl(ctx, kind, id)
	if err != nil {
		t.Fatal(err)
	}
	var out ScopeControl
	err = s.Tx(ctx, func(tx *Tx) error {
		var err error
		out, err = tx.ControlScope(kind, id, action, "usr_scope", state.Rev)
		return err
	})
	if err != nil {
		t.Fatalf("%s %s: %v", kind, action, err)
	}
	return out
}

func TestScopeStopRetriesFailedAgent(t *testing.T) {
	t.Parallel()
	s, f := scopeReviewFixture(t)
	if _, err := s.db.Exec(`UPDATE turn SET exec='lost' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='lost' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	first := applyScopeReview(t, s, "project", f.project.ID, "stop")
	if first.Desired != "stopped" || len(first.Failures) != 1 || first.Failures[0].AgentID != f.agent.ID {
		t.Fatalf("first stop: %+v", first)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	retry := applyScopeReview(t, s, "project", f.project.ID, "stop")
	if len(retry.Results) != 1 || retry.Results[0].Agent == nil || len(retry.Failures) != 0 {
		t.Fatalf("retry stop: %+v", retry)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	resumed := applyScopeReview(t, s, "project", f.project.ID, "resume")
	if len(resumed.Results) != 1 || resumed.Results[0].Agent == nil || resumed.Results[0].Agent.Desired != "active" {
		t.Fatalf("resume: %+v", resumed)
	}
}

func TestScopeResumeBeforeAgentStopsPreservesHoldAndRetry(t *testing.T) {
	t.Parallel()
	s, f := scopeReviewFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE turn SET exec='running' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='in_turn' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	stopped := applyScopeReview(t, s, "project", f.project.ID, "stop")
	if stopped.Desired != "stopped" {
		t.Fatalf("stop=%+v", stopped)
	}
	if _, err := s.db.Exec(`INSERT INTO scope_control_failure(scope_kind,scope_id,agent_id,action,error,attempted_at) VALUES ('project',?,?,'stop','still exiting',?)`, f.project.ID, f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ControlScope("project", f.project.ID, "resume", "usr_scope", stopped.Rev)
		return err
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("early resume error=%v", err)
	}
	state, err := s.ScopeControl(ctx, "project", f.project.ID)
	if err != nil || state.Desired != "stopped" || state.Rev != stopped.Rev || len(state.Failures) != 1 {
		t.Fatalf("preserved scope=%+v error=%v", state, err)
	}
	var hold string
	if err := s.db.QueryRow(`SELECT hold_desired FROM scope_control_agent WHERE scope_kind='project' AND scope_id=? AND agent_id=?`, f.project.ID, f.agent.ID).Scan(&hold); err != nil || hold != "stopped" {
		t.Fatalf("hold=%q error=%v", hold, err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='killed',settle='settled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	resumed := applyScopeReview(t, s, "project", f.project.ID, "resume")
	if resumed.Desired != "active" || len(resumed.Failures) != 0 {
		t.Fatalf("retry resume=%+v", resumed)
	}
}

func TestRetryStopLeavesIndividualOverrideAlone(t *testing.T) {
	t.Parallel()
	s, f := scopeReviewFixture(t)
	other, err := s.CreateAgent(context.Background(), Agent{Slug: "other", ProjectID: f.project.ID, RoleID: f.agent.RoleID, Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.agent.MachineID, CheckoutID: f.agent.CheckoutID, KeyHash: "hash", CreatedBy: "usr_scope"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(context.Background(), f.room.ID, "agent", other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='lost' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='lost' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	first := applyScopeReview(t, s, "project", f.project.ID, "stop")
	if len(first.Failures) != 1 {
		t.Fatalf("expected failed stop: %+v", first)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, other.ID); err != nil {
		t.Fatal(err)
	}
	var desiredRev int64
	if err := s.db.QueryRow(`SELECT desired_rev FROM agent WHERE id=?`, other.ID).Scan(&desiredRev); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(context.Background(), func(tx *Tx) error {
		_, err := tx.SetAgentDesired(other.ID, "usr_scope", "active", desiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	applyScopeReview(t, s, "project", f.project.ID, "stop")
	var otherDesired string
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, other.ID).Scan(&otherDesired); err != nil {
		t.Fatal(err)
	}
	if otherDesired != "active" {
		t.Fatalf("retry overrode individual control: %s", otherDesired)
	}
}

func TestOverlappingScopesRestoreInBothOrders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, firstKind, firstAction, secondKind, secondAction, firstResume, secondResume string }{
		{"room-pause-project-stop-project-first", "room", "pause", "project", "stop", "project", "room"},
		{"project-pause-room-stop-project-first", "project", "pause", "room", "stop", "project", "room"},
		{"room-pause-project-stop-room-first", "room", "pause", "project", "stop", "room", "project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := scopeReviewFixture(t)
			id := func(kind string) string {
				if kind == "room" {
					return f.room.ID
				}
				return f.project.ID
			}
			applyScopeReview(t, s, tc.firstKind, id(tc.firstKind), tc.firstAction)
			applyScopeReview(t, s, tc.secondKind, id(tc.secondKind), tc.secondAction)
			if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, f.agent.ID); err != nil {
				t.Fatal(err)
			}
			first := applyScopeReview(t, s, tc.firstResume, id(tc.firstResume), "resume")
			if len(first.Results) > 1 {
				t.Fatalf("first resume: %+v", first)
			}
			mid, err := s.AgentCardByID(context.Background(), f.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.firstResume == tc.secondKind && tc.firstAction == "pause" && mid.Desired != "not_accepting" {
				t.Fatalf("pause hold lost: %+v", mid)
			}
			second := applyScopeReview(t, s, tc.secondResume, id(tc.secondResume), "resume")
			end, err := s.AgentCardByID(context.Background(), f.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if end.Desired != "active" || len(second.Failures) != 0 {
				t.Fatalf("after both resumes: %+v %+v", end, second)
			}
		})
	}
}

func TestRoomScopeUsesCurrentRoomMembers(t *testing.T) {
	t.Parallel()
	s, f := scopeReviewFixture(t)
	var spawnEvents int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='agent_spawn_requested' AND room_id=? AND agent_id=?`, f.room.ID, f.agent.ID).Scan(&spawnEvents); err != nil {
		t.Fatal(err)
	}
	if spawnEvents != 0 {
		t.Fatal("fixture unexpectedly has a spawn event")
	}
	state := applyScopeReview(t, s, "room", f.room.ID, "pause")
	if len(state.Results) != 1 || state.Results[0].Agent == nil || state.Results[0].Agent.ID != f.agent.ID {
		t.Fatalf("room members not controlled: %+v", state)
	}
}

func TestRoomScopeLeavesNonHomeMemberAgentRunning(t *testing.T) {
	t.Parallel()
	s, f := scopeReviewFixture(t)
	other, err := s.CreateRoom(context.Background(), Room{ProjectID: f.project.ID, Kind: "task", Title: "other", CreatedBy: "usr_scope"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(context.Background(), other.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	state := applyScopeReview(t, s, "room", other.ID, "stop")
	if len(state.Results) != 0 {
		t.Fatalf("non-home agent controlled: %+v", state)
	}
	card, err := s.AgentCardByID(context.Background(), f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Desired != "active" {
		t.Fatalf("non-home agent stopped: %+v", card)
	}
}

func TestPrivateRoomBulkControlRejected(t *testing.T) {
	t.Parallel()
	s, f := scopeReviewFixture(t)
	dm, err := s.CreateRoom(context.Background(), Room{ProjectID: f.project.ID, Kind: "dm", Title: "private", CreatedBy: "usr_scope"})
	if err != nil {
		t.Fatal(err)
	}
	var state ScopeControl
	err = s.Tx(context.Background(), func(tx *Tx) error {
		var err error
		state, err = tx.ControlScope("room", dm.ID, "pause", "usr_scope", 0)
		return err
	})
	if !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("DM bulk result=%+v err=%v", state, err)
	}
}

func TestIndividualControlOverridesOverlappingScopeRestore(t *testing.T) {
	t.Parallel()
	s, f := scopeReviewFixture(t)
	applyScopeReview(t, s, "project", f.project.ID, "pause")
	applyScopeReview(t, s, "room", f.room.ID, "stop")
	if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	card, err := s.AgentCardByID(context.Background(), f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(context.Background(), func(tx *Tx) error {
		_, err := tx.SetAgentDesired(f.agent.ID, "usr_scope", "active", card.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	applyScopeReview(t, s, "room", f.room.ID, "resume")
	applyScopeReview(t, s, "project", f.project.ID, "resume")
	card, err = s.AgentCardByID(context.Background(), f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Desired != "active" {
		t.Fatalf("individual override lost: %+v", card)
	}
	var ledgers int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scope_control_agent WHERE agent_id=?`, f.agent.ID).Scan(&ledgers); err != nil {
		t.Fatal(err)
	}
	if ledgers != 0 {
		t.Fatalf("stale ledgers: %d", ledgers)
	}
}

func TestTwoPauseScopesKeepAgentPausedUntilBothResume(t *testing.T) {
	t.Parallel()
	s, f := scopeReviewFixture(t)
	applyScopeReview(t, s, "project", f.project.ID, "pause")
	applyScopeReview(t, s, "room", f.room.ID, "pause")
	applyScopeReview(t, s, "project", f.project.ID, "resume")
	middle, err := s.AgentCardByID(context.Background(), f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if middle.Desired != "not_accepting" {
		t.Fatalf("remaining room pause lost: %+v", middle)
	}
	applyScopeReview(t, s, "room", f.room.ID, "resume")
	end, err := s.AgentCardByID(context.Background(), f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if end.Desired != "active" {
		t.Fatalf("final restore: %+v", end)
	}
}

func TestScopeResumeChecksSavedLauncherWithoutMachineConnection(t *testing.T) {
	s, f := scopeReviewFixture(t)
	launcher, err := s.SaveLauncher(t.Context(), "owner", Launcher{
		MachineID: f.agent.MachineID, Tool: "codex", Name: "custom", Command: "codex", Containerized: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET tool='codex',variant='custom',launcher_id=? WHERE id=?`, launcher.ID, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	applyScopeReview(t, s, "project", f.project.ID, "pause")
	disabled, needsBind, err := s.ScopeResumeLauncherStatus(t.Context(), "project", f.project.ID)
	if err != nil || !disabled || !needsBind {
		t.Fatalf("disabled=%t needs_bind=%t err=%v", disabled, needsBind, err)
	}
	if _, err := s.db.Exec(`UPDATE machine_launcher SET enabled=1,agent_server_url='http://host.docker.internal:9897' WHERE id=?`, launcher.ID); err != nil {
		t.Fatal(err)
	}
	disabled, needsBind, err = s.ScopeResumeLauncherStatus(t.Context(), "project", f.project.ID)
	if err != nil || disabled || needsBind {
		t.Fatalf("enabled=%t needs_bind=%t err=%v", !disabled, needsBind, err)
	}
}

func TestScopeControlRestoresOnlyBulkChangedAgents(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_owner") }); err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateAgent(ctx, Agent{Slug: "reviewer", ProjectID: f.project.ID, RoleID: f.agent.RoleID,
		Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.agent.MachineID,
		CheckoutID: f.agent.CheckoutID, KeyHash: "second", CreatedBy: "usr_owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=1 WHERE id=?`, second.ID); err != nil {
		t.Fatal(err)
	}
	apply := func(action string, rev int64) ScopeControl {
		t.Helper()
		var result ScopeControl
		err := s.Tx(ctx, func(tx *Tx) error {
			var err error
			result, err = tx.ControlScope("project", f.project.ID, action, "usr_owner", rev)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	paused := apply("pause", 0)
	if paused.Desired != "paused" || paused.Rev != 1 || len(paused.Results) != 1 || paused.Results[0].Agent.ID != f.agent.ID {
		t.Fatalf("pause=%+v", paused)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("project pause allowed dispatch: ready=%t err=%v", ready, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ControlScope("project", f.project.ID, "stop", "usr_owner", 0)
		return err
	}); !errors.Is(err, ErrStaleRev) {
		t.Fatalf("stale rev=%v", err)
	}
	resumed := apply("resume", 1)
	if resumed.Desired != "active" || resumed.Rev != 2 || len(resumed.Results) != 1 || resumed.Results[0].Agent.ID != f.agent.ID {
		t.Fatalf("resume=%+v", resumed)
	}
	var firstDesired, secondDesired, gate string
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, f.agent.ID).Scan(&firstDesired); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, second.ID).Scan(&secondDesired); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT gate FROM project WHERE id=?`, f.project.ID).Scan(&gate); err != nil {
		t.Fatal(err)
	}
	if firstDesired != "active" || secondDesired != "not_accepting" || gate != "open" {
		t.Fatalf("desired=%s,%s gate=%s", firstDesired, secondDesired, gate)
	}
	stopped := apply("stop", 2)
	if stopped.Desired != "stopped" || len(stopped.Results) != 2 || stopped.Results[0].Agent == nil || stopped.Results[1].Agent == nil {
		t.Fatalf("stop=%+v", stopped)
	}
	resumed = apply("resume", 3)
	if resumed.Desired != "active" || len(resumed.Results) != 2 || resumed.Results[0].Agent == nil || resumed.Results[1].Agent == nil {
		t.Fatalf("resume after stop=%+v", resumed)
	}
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, second.ID).Scan(&secondDesired); err != nil {
		t.Fatal(err)
	}
	if secondDesired != "not_accepting" {
		t.Fatalf("individual pause lost: %s", secondDesired)
	}
}
