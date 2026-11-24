package store

import (
	"context"
	"strings"
	"testing"
)

// resetToPreApprovalBodies puts the built-in leader and dispatcher rows back to
// the state migration 132 left them in, so the migration's SQL can be run
// against it again.
func resetToPreApprovalBodies(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE role SET body=?,rev=1 WHERE id='role_leader_default'`, leadRoleBody); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE role SET body=?,rev=0 WHERE id='role_dispatcher_default'`, dispatcherRoleBody); err != nil {
		t.Fatal(err)
	}
}

func TestFreshProjectRolesCarryTheApprovalRule(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	bodies := roleBodies(t, s.db)
	for key, want := range map[string]string{
		"leader":     spawnApprovalRuleLead,
		"dispatcher": spawnApprovalRuleDispatcher,
	} {
		if !strings.Contains(bodies[key], want) {
			t.Errorf("fresh %s role lacks the approval rule", key)
		}
	}
	if bodies["leader"] != currentLeadRoleBody || bodies["dispatcher"] != currentDispatcherRoleBody {
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
	resetToPreApprovalBodies(t, s)
	if got := facts(); len(got) != 0 {
		t.Errorf("unedited defaults drew facts: %v", got)
	}
	if _, err := s.db.Exec(`UPDATE role SET body='owner edit',rev=2 WHERE id='role_leader_default'`); err != nil {
		t.Fatal(err)
	}
	got := facts()
	if len(got) != 1 || !strings.Contains(got[0], "role_leader_default") || !strings.Contains(got[0], "missing") {
		t.Errorf("edited leader body: facts %v", got)
	}
	if _, err := s.db.Exec(`UPDATE role SET body=?,rev=1 WHERE id='role_leader_default'`, currentLeadRoleBody); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE role SET body='owner edit',rev=1 WHERE id='role_dispatcher_default'`); err != nil {
		t.Fatal(err)
	}
	got = facts()
	if len(got) != 1 || !strings.Contains(got[0], "role_dispatcher_default") {
		t.Errorf("one edited body at a time: facts %v", got)
	}
}
