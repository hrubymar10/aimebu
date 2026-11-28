package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshProjectRolesCarryTheApprovalRule(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	bodies := resolvedRoleBodies(t, s)
	for _, want := range []string{spawnApprovalRuleLead} {
		if !strings.Contains(bodies["leader"], want) {
			t.Errorf("fresh leader role lacks %q", want)
		}
	}
	for _, want := range []string{spawnApprovalRuleDispatcher, researcherRuleDispatcher} {
		if !strings.Contains(bodies["dispatcher"], want) {
			t.Errorf("fresh dispatcher role lacks %q", want)
		}
	}
	// The researcher sentence about the dispatcher lives only in the
	// leader-dispatcher flow's leader text: the base leader body is what a
	// leader gets in flows with no dispatcher, so it must not name one.
	if strings.Contains(bodies["leader"], "dispatcher") {
		t.Error("the base leader body mentions the dispatcher")
	}
	if bodies["leader"] != resumeLeadRoleBody || bodies["dispatcher"] != resumeDispatcherRoleBody {
		t.Error("fresh built-in bodies differ from the shipped definitions")
	}
	for _, flow := range TeamFlowTemplates() {
		for _, role := range []string{"leader", "dispatcher"} {
			if flow.Key == "leader-dispatcher" && role == "leader" {
				if strings.Contains(flow.RoleTextOverrides[role], "resume_agent") {
					t.Error("routed leader must not be instructed to resume workers")
				}
				continue
			}
			if body := flow.RoleTextOverrides[role]; body != "" && !strings.Contains(body, resumeWorkerRule) {
				t.Errorf("%s %s role lacks resume instructions", flow.Key, role)
			}
		}
	}
}

func TestRoleResumeMigrationChainsAfterCloseout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, oldLead, oldDispatcher, afterCloseout, wantLead, wantDispatcher string
		leadRev                                                               int
	}{
		{"unchanged", currentLeadRoleBody, priorResumeDispatcherRoleBody, shippedLeadRoleBody, resumeLeadRoleBody, resumeDispatcherRoleBody, 1},
		{"edited leader", currentLeadRoleBody + " owner edit", priorResumeDispatcherRoleBody, currentLeadRoleBody + " owner edit", currentLeadRoleBody + " owner edit", resumeDispatcherRoleBody, 2},
		{"edited dispatcher", currentLeadRoleBody, "owner dispatcher text", shippedLeadRoleBody, resumeLeadRoleBody, "owner dispatcher text", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "before-resume.sqlite")))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := migrateUpTo(context.Background(), db, 147); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE role SET body=?,rev=? WHERE id='role_leader_default'`, tc.oldLead, tc.leadRev); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE role SET body=? WHERE id='role_dispatcher_default'`, tc.oldDispatcher); err != nil {
				t.Fatal(err)
			}
			if err := migrateUpTo(context.Background(), db, 148); err != nil {
				t.Fatal(err)
			}
			var afterCloseout string
			if err := db.QueryRow(`SELECT body FROM role WHERE id='role_leader_default'`).Scan(&afterCloseout); err != nil {
				t.Fatal(err)
			}
			if afterCloseout != tc.afterCloseout {
				t.Fatal("migration 148 changed an edited leader or missed the built-in leader")
			}
			if err := migrate(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			var lead, dispatcher string
			if err := db.QueryRow(`SELECT body FROM role WHERE id='role_leader_default'`).Scan(&lead); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT body FROM role WHERE id='role_dispatcher_default'`).Scan(&dispatcher); err != nil {
				t.Fatal(err)
			}
			if lead != tc.wantLead || dispatcher != tc.wantDispatcher {
				t.Fatalf("after migration leader=%q dispatcher=%q", lead, dispatcher)
			}
		})
	}
}

func TestRoleResumeMigrationPreservesRestoredEditedBodies(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "restored-role.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := migrateUpTo(ctx, db, 148); err != nil {
		t.Fatal(err)
	}
	for _, role := range []struct{ id, body string }{
		{"role_leader_default", priorResumeLeadRoleBody},
		{"role_dispatcher_default", priorResumeDispatcherRoleBody},
	} {
		if _, err := db.Exec(`UPDATE role SET body=?,rev=2 WHERE id=?`, role.body, role.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUpTo(ctx, db, 149); err != nil {
		t.Fatal(err)
	}
	for _, role := range []struct{ id, body string }{
		{"role_leader_default", priorResumeLeadRoleBody},
		{"role_dispatcher_default", priorResumeDispatcherRoleBody},
	} {
		var got string
		if err := db.QueryRow(`SELECT body FROM role WHERE id=?`, role.id).Scan(&got); err != nil || got != role.body {
			t.Fatalf("%s changed after owner restored its body: err=%v", role.id, err)
		}
	}
}

func TestUpgradedProjectRolesCarryTheApprovalRule(t *testing.T) {
	t.Parallel()
	db, path := priorRoleDatabase(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bodies := resolvedRoleBodies(t, s)
	if !strings.Contains(bodies["leader"], spawnApprovalRuleLead) || !strings.Contains(bodies["dispatcher"], spawnApprovalRuleDispatcher) {
		t.Error("an upgraded database lacks the approval rules in the built-in roles")
	}
	if !strings.Contains(bodies["dispatcher"], researcherRuleDispatcher) {
		t.Error("an upgraded database lacks the dispatcher's researcher rule")
	}
	if strings.Contains(bodies["leader"], "dispatcher") {
		t.Error("the upgraded base leader body mentions the dispatcher")
	}
}

func TestSpawnApprovalMigrationRewritesOnlyTheUneditedDefaults(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		leaderBody     string
		leaderRev      int
		dispatcherBody string
		dispatcherRev  int
		wantLeader     string
		wantDispatcher string
	}{
		"unedited":          {leadRoleBody, 1, dispatcherRoleBody, 0, currentLeadRoleBody, currentDispatcherRoleBody},
		"edited leader":     {leadRoleBody + " edit", 2, dispatcherRoleBody, 0, leadRoleBody + " edit", currentDispatcherRoleBody},
		"edited dispatcher": {leadRoleBody, 1, "owner edit", 1, currentLeadRoleBody, "owner edit"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := testStore(t, Options{})
			if _, err := s.db.Exec(`UPDATE role SET body=?,rev=? WHERE id='role_leader_default'`, test.leaderBody, test.leaderRev); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE role SET body=?,rev=? WHERE id='role_dispatcher_default'`, test.dispatcherBody, test.dispatcherRev); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(roleSpawnApprovalMigration()); err != nil {
				t.Fatal(err)
			}
			for id, want := range map[string]string{"role_leader_default": test.wantLeader, "role_dispatcher_default": test.wantDispatcher} {
				var got string
				if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, id).Scan(&got); err != nil || got != want {
					t.Errorf("%s body=%q err=%v", id, got, err)
				}
			}
		})
	}
}

// TestSpawnApprovalMigrationAddsTheResearcherRule moves the dispatcher body
// migration 134 wrote to the one that also carries the who-spawns-what rule;
// the leader body is left as migration 134 wrote it.
func TestSpawnApprovalMigrationAddsTheResearcherRule(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		leaderBody     string
		leaderRev      int
		dispatcherBody string
		dispatcherRev  int
		wantLeader     string
		wantDispatcher string
	}{
		"unedited":          {currentLeadRoleBody, 1, currentDispatcherRoleBody, 0, currentLeadRoleBody, finalDispatcherRoleBody},
		"edited leader":     {currentLeadRoleBody + " edit", 2, currentDispatcherRoleBody, 0, currentLeadRoleBody + " edit", finalDispatcherRoleBody},
		"edited dispatcher": {currentLeadRoleBody, 1, "owner edit", 1, currentLeadRoleBody, "owner edit"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := testStore(t, Options{})
			if _, err := s.db.Exec(`UPDATE role SET body=?,rev=? WHERE id='role_leader_default'`, test.leaderBody, test.leaderRev); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE role SET body=?,rev=? WHERE id='role_dispatcher_default'`, test.dispatcherBody, test.dispatcherRev); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(roleResearcherMigration()); err != nil {
				t.Fatal(err)
			}
			for id, want := range map[string]string{"role_leader_default": test.wantLeader, "role_dispatcher_default": test.wantDispatcher} {
				var got string
				if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, id).Scan(&got); err != nil || got != want {
					t.Errorf("%s body=%q err=%v", id, got, err)
				}
			}
		})
	}
}

// A14-9: the dispatcher's duties are part of the shipped default (which an unedited built-in role resolves to, see
// TestOldRoleBodiesBecomeCurrentDefaults) and of the leader-dispatcher flow's dispatcher text, which replaces the body on delivery.
func TestDispatcherDutiesAreInTheDefaultAndTheFlowText(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	bodies := resolvedRoleBodies(t, s)
	flow, ok := teamFlowTemplate("leader-dispatcher")
	if !ok {
		t.Fatal("no leader-dispatcher flow")
	}
	for name, body := range map[string]string{"default body": bodies["dispatcher"], "flow text": flow.RoleTextOverrides["dispatcher"]} {
		for _, want := range []string{
			"goes to the leader as a proposal first", "never ask people through a worker",
			"list_recoveries and resolve_recovery", "more than about 30 minutes", "replace it right away",
			"through its own shell tool", "run_command only for long jobs",
			"as file:line", "check those citations before you pass it on", "when a citation does not hold",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	if strings.Contains(bodies["leader"], "list_recoveries") {
		t.Error("the leader body gained the dispatcher's duties")
	}
}
