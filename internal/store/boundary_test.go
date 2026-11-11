package store

import (
	"context"
	"errors"
	"testing"
)

func TestWithinFolderUsesPathComponents(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		root, path string
		want       bool
	}{
		{"/work/proj", "/work/proj", true},
		{"/work/proj", "/work/proj/task", true},
		{"/work/proj", "/work/proj2", false},
		{"/work/proj", "/work/proj/../outside", false},
		{"/work/proj/", "/work/proj/task", false},
		{"work/proj", "/work/proj", false},
	} {
		if got := WithinFolder(test.root, test.path); got != test.want {
			t.Errorf("WithinFolder(%q,%q)=%t, want %t", test.root, test.path, got, test.want)
		}
	}
}

func TestRemoteBoundaryApprovalDelaysSpawnAndIsAgentSpecific(t *testing.T) {
	t.Parallel()
	s, project, room, _, _ := wakeFixture(t)
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_boundary','owner','hash','{}','now')`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_operator','operator','hash','{}','now')`,
		`INSERT INTO machine(id,name,key_hash,approved) VALUES ('mch_boundary','remote','hash',1)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_boundary','owner'),(?,'usr_operator','operator')`, project.ID, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,'mch_boundary','/remote/project')`, project.ID); err != nil {
		t.Fatal(err)
	}
	spawn := func() AgentCard {
		t.Helper()
		var card AgentCard
		if err := s.Tx(ctx, func(tx *Tx) error {
			var err error
			card, err = tx.SpawnAgent(SpawnInput{RoomID: room.ID, ActorID: "usr_boundary", ActorKind: "user", MachineID: "mch_boundary", Path: "/remote/project", RoleKey: "worker", Tool: "fake", Variant: "test", Model: "test", KeyHash: "hash"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return card
	}
	first := spawn()
	if first.BoundaryPending == "" || first.Desired != "not_accepting" {
		t.Fatalf("pending card: %+v", first)
	}
	if _, err := s.db.Exec(`UPDATE room SET kind='dm' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_peer','peer','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_peer','viewer')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "user", "usr_peer"); err != nil {
		t.Fatal(err)
	}
	if peerItems, err := s.InboxForUser(ctx, "usr_peer"); err != nil || len(peerItems) != 0 {
		t.Fatalf("DM peer saw boundary approval: %+v, %v", peerItems, err)
	}
	items, err := s.InboxForUser(ctx, "usr_boundary")
	if err != nil || len(items) != 1 || !items[0].BoundaryPending || items[0].BoundaryPath != "/remote/project" {
		t.Fatalf("boundary inbox: %+v %v", items, err)
	}
	if _, _, err := s.AnswerInbox(ctx, first.BoundaryPending, "usr_boundary", "wrong-endpoint", "Approve", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("chat answer on boundary item: %v", err)
	}
	if _, ready, err := s.PendingSpawnByID(ctx, first.ID); err != nil || ready {
		t.Fatalf("spawn before approval: ready=%t err=%v", ready, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(first.ID, "usr_boundary", "active", first.DesiredRev)
		return err
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("held spawn resumed without approval: %v", err)
	}
	if _, _, err := s.DecideBoundary(ctx, first.BoundaryPending, "usr_operator", "operator", "approve", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator decision: %v", err)
	}
	decision, reused, err := s.DecideBoundary(ctx, first.BoundaryPending, "usr_boundary", "approve-key", "approve", 0)
	if err != nil || reused || decision.Choice != "approve" {
		t.Fatalf("owner approval: %+v reused=%t err=%v", decision, reused, err)
	}
	if _, reused, err := s.DecideBoundary(ctx, first.BoundaryPending, "usr_boundary", "approve-key", "approve", 0); err != nil || !reused {
		t.Fatalf("approval replay: reused=%t err=%v", reused, err)
	}
	if _, ready, err := s.PendingSpawnByID(ctx, first.ID); err != nil || !ready {
		t.Fatalf("spawn after approval: ready=%t err=%v", ready, err)
	}
	second := spawn()
	if second.BoundaryPending != "" {
		t.Fatalf("project machine admission should cover mapped root: %+v", second)
	}
	var outside AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		outside, err = tx.SpawnAgent(SpawnInput{RoomID: room.ID, ActorID: "usr_boundary", ActorKind: "user", MachineID: "mch_boundary", Path: "/other/work", RoleKey: "worker", Tool: "fake", Variant: "test", Model: "test", KeyHash: "hash"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if outside.BoundaryPending == "" {
		t.Fatalf("outside mapped root should wait: %+v", outside)
	}
	if _, _, err := s.DecideBoundary(ctx, outside.BoundaryPending, "usr_boundary", "deny-outside", "deny", 0); err != nil {
		t.Fatal(err)
	}
	outside, err = s.AgentCardByID(ctx, outside.ID)
	if err != nil || outside.Desired != "stopped" || outside.BoundaryPending != "" {
		t.Fatalf("denied spawn: %+v %v", outside, err)
	}
	if _, ready, err := s.PendingSpawnByID(ctx, outside.ID); err != nil || ready {
		t.Fatalf("denied spawn ready=%t err=%v", ready, err)
	}
}

func TestOutsideCheckoutReactivationWaitsForOwner(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_reactivate','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_reactivate','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE checkout SET canonical_path='/other/worktree' WHERE id=?`, agent.CheckoutID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	var card AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		card, err = tx.SetAgentDesired(agent.ID, "usr_reactivate", "active", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if card.BoundaryPending == "" || card.Desired != "stopped" {
		t.Fatalf("reactivation should wait: %+v", card)
	}
	if _, _, err := s.DecideBoundary(ctx, card.BoundaryPending, "usr_reactivate", "approve-reactivate", "approve", 0); err != nil {
		t.Fatal(err)
	}
	card, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil || card.Desired != "active" || card.BoundaryPending != "" || card.RoomID != room.ID {
		t.Fatalf("reactivated card: %+v %v", card, err)
	}
}

func TestFreshStartChoiceSurvivesBoundaryApproval(t *testing.T) {
	t.Parallel()
	s, project, _, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_fresh','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_fresh','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude',desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa',native_session_started_at=? WHERE agent_id=?`, timestamp(), agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE checkout SET canonical_path='/outside/folder' WHERE id=?`, agent.CheckoutID); err != nil {
		t.Fatal(err)
	}
	var pending AgentCard
	if err := s.Tx(ctx, func(tx *Tx) (err error) {
		pending, err = tx.StartAgentFresh(agent.ID, "usr_fresh", 0)
		return err
	}); err != nil || pending.BoundaryPending == "" {
		t.Fatalf("pending fresh start: %+v %v", pending, err)
	}
	if _, _, err := s.DecideBoundary(ctx, pending.BoundaryPending, "usr_fresh", "approve-fresh", "approve", 0); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.db.QueryRow(`SELECT start_mode FROM session WHERE agent_id=? AND generation=1`, agent.ID).Scan(&mode); err != nil || mode != "fresh_with_history" {
		t.Fatalf("approved fresh start: %q %v", mode, err)
	}
}

func TestOutsideCheckoutSwitchWaitsForOwner(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_boundary_switch','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_boundary_switch','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE checkout SET canonical_path='/other/worktree' WHERE id=?`, agent.CheckoutID); err != nil {
		t.Fatal(err)
	}
	var card AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		card, err = tx.BeginAgentSwitch(agent.ID, "usr_boundary_switch", room.ID, "claude", "native", "haiku", "anthropic", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if card.BoundaryPending == "" || card.Desired != "not_accepting" {
		t.Fatalf("switch pending: %+v", card)
	}
	if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 0 {
		t.Fatalf("switch advanced without approval: %+v %v", controls, err)
	}
	oldPending := card.BoundaryPending
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.withdrawBoundaryPending(agent.ID) }); err != nil {
		t.Fatal(err)
	}
	if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 0 {
		t.Fatalf("withdrawn switch bypassed recheck: %+v %v", controls, err)
	}
	card, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil || card.BoundaryPending == "" || card.BoundaryPending == oldPending {
		t.Fatalf("switch approval was not recreated: %+v %v", card, err)
	}
	if _, _, err := s.DecideBoundary(ctx, card.BoundaryPending, "usr_boundary_switch", "approve-switch", "approve", 0); err != nil {
		t.Fatal(err)
	}
	if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 1 {
		t.Fatalf("switch after approval: %+v %v", controls, err)
	}
}

func TestLegacySpawnApprovalAndDenial(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"approve", "deny"} {
		t.Run(decision, func(t *testing.T) {
			s, project, _, agent, _ := wakeFixture(t)
			ctx := context.Background()
			if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_writer','owner','hash','{}','now')`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_writer','owner')`, project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE role SET cap_singleton_writer=1 WHERE id='role_worker'`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE checkout SET canonical_path='/outside/work',writer_agent_id=?,writer_state='reserved' WHERE id=?`, agent.ID, agent.CheckoutID); err != nil {
				t.Fatal(err)
			}
			allowed, err := s.EnsureSpawnBoundary(ctx, agent.ID)
			if err != nil || allowed {
				t.Fatalf("held legacy spawn allowed=%t err=%v", allowed, err)
			}
			card, err := s.AgentCardByID(ctx, agent.ID)
			if err != nil || card.BoundaryPending == "" {
				t.Fatalf("pending card=%+v err=%v", card, err)
			}
			if _, _, err := s.DecideBoundary(ctx, card.BoundaryPending, "usr_writer", "writer-"+decision, decision, 0); err != nil {
				t.Fatal(err)
			}
			var desired string
			if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, agent.ID).Scan(&desired); err != nil {
				t.Fatal(err)
			}
			if decision == "approve" && desired != "active" {
				t.Fatalf("approved desired=%s", desired)
			}
			if decision == "deny" && desired != "stopped" {
				t.Fatalf("denied desired=%s", desired)
			}

		})
	}
}

func TestLegacyPausedSpawnStaysPausedAfterApproval(t *testing.T) {
	t.Parallel()
	s, project, _, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_pause','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_pause','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE checkout SET canonical_path='/outside/work' WHERE id=?`, agent.CheckoutID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='not_accepting' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.EnsureSpawnBoundary(ctx, agent.ID); err != nil || allowed {
		t.Fatalf("held paused spawn allowed=%t err=%v", allowed, err)
	}
	card, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideBoundary(ctx, card.BoundaryPending, "usr_pause", "approve-paused", "approve", 0); err != nil {
		t.Fatal(err)
	}
	card, err = s.AgentCardByID(ctx, agent.ID)
	if err != nil || card.Desired != "not_accepting" {
		t.Fatalf("approval lifted pause: %+v %v", card, err)
	}
}

func TestRemapWithdrawsOldBoundaryApproval(t *testing.T) {
	t.Parallel()
	s, project, room, _, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_remap','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_remap','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO machine(id,name,key_hash,approved) VALUES ('mch_remap','remote','hash',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProjectMachineFolder(ctx, project.ID, "mch_remap", "usr_remap", "/remote/old"); err != nil {
		t.Fatal(err)
	}
	var card AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		card, err = tx.SpawnAgent(SpawnInput{RoomID: room.ID, ActorID: "usr_remap", ActorKind: "user", MachineID: "mch_remap", Path: "/remote/old", RoleKey: "worker", Tool: "fake", Variant: "test", Model: "test", KeyHash: "hash"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if card.BoundaryPending == "" {
		t.Fatalf("no approval for old mapping: %+v", card)
	}
	if _, err := s.db.Exec(`UPDATE project_machine_folder SET folder='/remote/new' WHERE project_id=? AND machine_id='mch_remap'`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideBoundary(ctx, card.BoundaryPending, "usr_remap", "unwithdrawn-remap", "approve", 0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approval accepted changed mapping: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE project_machine_folder SET folder='/remote/old' WHERE project_id=? AND machine_id='mch_remap'`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProjectMachineFolder(ctx, project.ID, "mch_remap", "usr_remap", "/remote/new"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideBoundary(ctx, card.BoundaryPending, "usr_remap", "stale-remap", "approve", 1); !errors.Is(err, ErrAlreadyWithdrawn) {
		t.Fatalf("stale approval remained usable: %v", err)
	}
	updated, err := s.AgentCardByID(ctx, card.ID)
	if err != nil || updated.BoundaryPending == "" || updated.BoundaryPending == card.BoundaryPending {
		t.Fatalf("new mapping did not request a fresh approval: %+v %v", updated, err)
	}
	if allowed, err := s.ProjectMachineAllowed(ctx, project.ID, "mch_remap"); err != nil || allowed {
		t.Fatalf("new mapping admitted: %t %v", allowed, err)
	}
}
