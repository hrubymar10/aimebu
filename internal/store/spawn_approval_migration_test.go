package store

import (
	"context"
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
	if bodies["leader"] != finalLeadRoleBody+"\n\n"+spawnApprovalRuleLead || bodies["dispatcher"] != finalDispatcherRoleBody {
		t.Error("fresh built-in bodies differ from the shipped definitions")
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
