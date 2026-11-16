package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReviewerAgentsInheritanceAndSpawn(t *testing.T) {
	s, project, room, worker, leader := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO settings_project(project_id) VALUES (?)`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_reviewer_setting','reviewer-setting','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_reviewer_setting','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO role(id,key,role_key,label,emoji,body,cardinality,cap_is_lead,cap_can_spawn,cap_singleton_writer,rev,extends,spawn_defaults_json,spawn_pick_mode)
		SELECT 'role_custom_review','custom-review','custom-review','Custom review',emoji,body,cardinality,0,0,0,0,'reviewer','[]','in_turn' FROM role WHERE role_key='reviewer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO role(id,key,role_key,label,emoji,body,cardinality,cap_is_lead,cap_can_spawn,cap_singleton_writer,rev,extends,spawn_defaults_json,spawn_pick_mode)
		SELECT 'role_deep_review','deep-review','deep-review','Deep review',emoji,body,cardinality,0,0,0,0,'custom-review','[]','in_turn' FROM role WHERE role_key='reviewer'`); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"leader", "dispatcher"} {
		key := "custom-" + parent
		if _, err := s.db.Exec(`INSERT INTO role(id,key,role_key,label,emoji,body,cardinality,cap_is_lead,cap_can_spawn,cap_singleton_writer,rev,extends,spawn_defaults_json,spawn_pick_mode)
			SELECT ?,?,?,?,emoji,body,cardinality,cap_is_lead,cap_can_spawn,cap_singleton_writer,0,?,'[]','in_turn' FROM role WHERE role_key=?`, "role_"+key, key, key, key, parent, parent); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		for role, want := range map[string]bool{"custom-leader": true, "custom-dispatcher": true, "worker": false, "deep-review": false} {
			got, err := tx.roleLineageContains(role, "leader", "dispatcher")
			if err != nil {
				return err
			}
			if got != want {
				t.Errorf("%s has staffing lineage = %t, want %t", role, got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spawn := func(role, key string) error {
		return s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.SpawnAgent(SpawnInput{RoomID: room.ID, ActorKind: "user", ActorID: "usr_reviewer_setting", MachineID: "mch_wake", Path: "/tmp/wake", RoleKey: role, Tool: "fake", Variant: "test", Model: "test", Provider: "test", KeyHash: key})
			return err
		})
	}
	settings, err := s.GetProjectRecoverySettings(ctx, project.ID, "usr_reviewer_setting")
	if err != nil || settings.ReviewerAgents != "inherit" || !settings.ReviewerAgentsEnabled {
		t.Fatalf("defaults: %+v %v", settings, err)
	}
	if err := s.SetGlobalSettingValue(ctx, ReviewerAgentsGlobalKey, "0"); err != nil {
		t.Fatal(err)
	}
	settings, err = s.GetProjectRecoverySettings(ctx, project.ID, "usr_reviewer_setting")
	if err != nil || settings.ReviewerAgentsEnabled {
		t.Fatalf("global off: %+v %v", settings, err)
	}
	for _, role := range []string{"reviewer", "sec-reviewer", "custom-review", "deep-review"} {
		if err := spawn(role, "blocked-"+role); !errors.Is(err, ErrReviewerAgentsDisabled) {
			t.Fatalf("%s: %v", role, err)
		}
	}
	var workerRev int64
	if err := s.db.QueryRow(`SELECT desired_rev FROM agent WHERE id=?`, worker.ID).Scan(&workerRev); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentRoleAsTool(worker.ID, leader.ID, "deep-review", workerRev)
		return err
	}); !errors.Is(err, ErrReviewerAgentsDisabled) {
		t.Fatalf("role assignment to reviewer lineage: %v", err)
	}
	if err := spawn("worker", "worker-allowed"); err != nil {
		t.Fatalf("worker: %v", err)
	}
	on := "on"
	settings, err = s.SetProjectRecoverySettings(ctx, project.ID, "usr_reviewer_setting", ProjectSettingsUpdate{ReviewerAgents: &on})
	if err != nil || !settings.ReviewerAgentsEnabled {
		t.Fatalf("project on: %+v %v", settings, err)
	}
	if err := spawn("reviewer", "reviewer-allowed"); err != nil {
		t.Fatalf("reviewer override: %v", err)
	}
	off := "off"
	settings, err = s.SetProjectRecoverySettings(ctx, project.ID, "usr_reviewer_setting", ProjectSettingsUpdate{ReviewerAgents: &off})
	if err != nil || settings.ReviewerAgentsEnabled {
		t.Fatalf("project off: %+v %v", settings, err)
	}
	if err := spawn("reviewer", "blocked-again"); !errors.Is(err, ErrReviewerAgentsDisabled) {
		t.Fatalf("project off spawn: %v", err)
	}
}

func TestWithoutReviewerStaffing(t *testing.T) {
	if !reviewerEnabled("1", "inherit") || reviewerEnabled("0", "inherit") || !reviewerEnabled("0", "on") || reviewerEnabled("1", "off") {
		t.Fatal("reviewer setting inheritance changed")
	}
	bodies := []string{leadRoleBody, dispatcherRoleBody, flowMigrationNotice("custom", "leader-dispatcher", "dispatcher", true, true)}
	for _, flow := range TeamFlowTemplates() {
		for _, role := range []string{"leader", "dispatcher"} {
			if body := flow.RoleTextOverrides[role]; body != "" {
				bodies = append(bodies, body)
			}
		}
	}
	for _, pair := range reviewerStaffingClauses {
		found := false
		for _, body := range bodies {
			if strings.Contains(body, pair[0]) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("reviewer clause is no longer in role-text source: %q", pair[0])
		}
	}
	for _, body := range bodies {
		filtered := withoutReviewerStaffing(body)
		for _, forbidden := range []string{"independent review", "reviewer"} {
			if strings.Contains(filtered, forbidden) {
				t.Errorf("retained %q in %q", forbidden, filtered)
			}
		}
	}
	for _, flow := range []string{"leader-workers", "leader-dispatcher", "leader-advisor"} {
		for _, role := range []string{"leader", "dispatcher"} {
			notice := flowMigrationNotice("custom", flow, role, false, true)
			if strings.Contains(notice, "reviewer") || strings.Contains(notice, "independent review") {
				t.Errorf("%s/%s notice: %s", flow, role, notice)
			}
		}
	}
	if on := flowMigrationNotice("custom", "leader-dispatcher", "dispatcher", true, true); !strings.Contains(on, "reviewers") {
		t.Fatalf("enabled dispatcher notice lost reviewer staffing: %s", on)
	}
}
