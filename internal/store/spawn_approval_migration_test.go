package store

import (
	"context"
	"strings"
	"testing"
)

func TestFreshProjectRolesCarryTheApprovalRule(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	bodies := roleBodies(t, s.db)
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
	if bodies["leader"] != currentLeadRoleBody || bodies["dispatcher"] != finalDispatcherRoleBody {
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
	bodies := roleBodies(t, s.db)
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

func TestSpawnApprovalRoleFactsNameOnlyEditedBodies(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	facts := func() []string {
		t.Helper()
		var got []string
		if err := s.Tx(context.Background(), func(tx *Tx) error {
			var err error
			got, err = spawnApprovalRoleFacts(context.Background(), tx.tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// The bodies migrations 134 and 135 rewrite are not the owner's: no notice.
	if _, err := s.db.Exec(`UPDATE role SET body=?,rev=1 WHERE id='role_leader_default'`, currentLeadRoleBody); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE role SET body=?,rev=0 WHERE id='role_dispatcher_default'`, currentDispatcherRoleBody); err != nil {
		t.Fatal(err)
	}
	if got := facts(); len(got) != 0 {
		t.Errorf("unedited defaults drew facts: %v", got)
	}
	// An owner-edited leader body missing the approval sentence gets one notice.
	if _, err := s.db.Exec(`UPDATE role SET body='owner edit',rev=2 WHERE id='role_leader_default'`); err != nil {
		t.Fatal(err)
	}
	got := facts()
	if len(got) != 1 || !strings.Contains(got[0], "role_leader_default") || !strings.Contains(got[0], "missing") {
		t.Errorf("edited leader body: facts %v", got)
	}
	// An owner-edited body carrying the rule gets none.
	owned := currentLeadRoleBody + "\n\nowner wording"
	if _, err := s.db.Exec(`UPDATE role SET body=?,rev=2 WHERE id='role_leader_default'`, owned); err != nil {
		t.Fatal(err)
	}
	if got := facts(); len(got) != 0 {
		t.Errorf("edited body with the sentence drew facts: %v", got)
	}
	// The dispatcher's notice names the researcher rule only when the edited
	// body carries the approval rule but not the researcher rule; one missing
	// everything just names the approval rule.
	if _, err := s.db.Exec(`UPDATE role SET body='owner edit',rev=1 WHERE id='role_dispatcher_default'`); err != nil {
		t.Fatal(err)
	}
	got = facts()
	if len(got) != 1 || !strings.Contains(got[0], "role_dispatcher_default") || !strings.Contains(got[0], "the model-approval rule);") {
		t.Errorf("edited dispatcher body: facts %v", got)
	}
	// A dispatcher body with the approval rule but not the researcher rule
	// still gets its notice.
	half := "owner wording\n\n" + spawnApprovalRuleDispatcher
	if _, err := s.db.Exec(`UPDATE role SET body=?,rev=1 WHERE id='role_dispatcher_default'`, half); err != nil {
		t.Fatal(err)
	}
	got = facts()
	if len(got) != 1 || !strings.Contains(got[0], "role_dispatcher_default") {
		t.Errorf("edited dispatcher body with one sentence: facts %v", got)
	}
}
