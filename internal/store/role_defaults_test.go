package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func roleBodies(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT role_key,body FROM role WHERE project_id IS NULL AND deleted_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var key, body string
		if err := rows.Scan(&key, &body); err != nil {
			t.Fatal(err)
		}
		result[key] = body
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func priorRoleDatabase(t *testing.T) (*sql.DB, string) {
	return roleDatabaseBefore(t, 32)
}

func roleDatabaseBefore(t *testing.T, stop int) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, step := range migrations {
		if step.version >= stop {
			continue
		}
		if _, err := db.Exec(step.sql); err != nil {
			t.Fatalf("migration %d: %v", step.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version(version,applied_at) VALUES (?,?)`, step.version, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func TestRoleFollowupMigrationPreservesEditsAndRevision(t *testing.T) {
	db, path := roleDatabaseBefore(t, 35)
	prior := func(body string) string { return strings.Replace(body, roleSafety, priorRoleSafety, 1) }
	oldReviewer := prior(strings.Replace(reviewerRoleBody, reviewerStart, priorReviewerStart, 1))
	oldBodies := map[string]string{
		"role_lead_default":     strings.Replace(priorLeadRoleBody, frozenRoleSafety, priorRoleSafety, 1),
		"role_worker_default":   prior(strings.Replace(workerRoleBody, workerStart, priorWorkerStart, 1)),
		"role_reviewer_default": oldReviewer,
	}
	for _, focus := range reviewerFocus {
		oldBodies["role_"+focus.key+"_default"] = oldReviewer + "\n\nAdditional focus: " + focus.body
	}
	for id, body := range oldBodies {
		if _, err := db.Exec(`UPDATE role SET body=? WHERE id=?`, body, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []struct {
		id, body string
		rev      int
	}{
		{"role_lead_default", "owner lead edit", 1},
		{"role_reviewer_default", "owner reviewer edit", 1},
		{"role_sec-reviewer_default", oldBodies["role_sec-reviewer_default"], 1},
	} {
		if _, err := db.Exec(`UPDATE role SET body=?,rev=? WHERE id=?`, change.body, change.rev, change.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('proj_role','role','/tmp/role',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO role(id,key,label,body,project_id,role_key) VALUES ('role_scoped','reviewer@proj_role','Reviewer','scoped edit','proj_role','reviewer')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(roleFollowupMigration()); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"role_leader_default": "owner lead edit", "role_reviewer_default": "owner reviewer edit",
		"role_worker_default":       currentWorkerRoleBody,
		"role_sec-reviewer_default": oldBodies["role_sec-reviewer_default"],
		"role_scoped":               "scoped edit",
	} {
		var got string
		if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("%s body=%q err=%v", id, got, err)
		}
	}
	for _, focus := range reviewerFocus {
		id := "role_" + focus.key + "_default"
		if focus.key == "sec-reviewer" {
			continue
		}
		var got string
		// Migration 54 then turns the unedited specialist into a role that extends reviewer, holding only its focus.
		if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, id).Scan(&got); err != nil || got != specialistFocus(focus.body) {
			t.Errorf("%s body=%q err=%v", id, got, err)
		}
	}
}

func TestRoleDefaultsFreshAndUpgradeMatch(t *testing.T) {
	fresh, _ := testStore(t, Options{})
	want := roleBodies(t, fresh.db)
	if len(want) != 8 {
		t.Fatalf("fresh global role count = %d, want 8", len(want))
	}
	for key, body := range want {
		for _, local := range []string{"AIMEBU2_CONFIG_DIR", "flock", "prohibited substring"} {
			if strings.Contains(body, local) {
				t.Errorf("global %s role contains local rule %q", key, local)
			}
		}
	}
	for key, needle := range map[string]string{
		"leader": "frame the problem", "worker": "parent SHA", "reviewer": "ordered by severity",
		"sec-reviewer": "authorization boundaries", "test-reviewer": "regression coverage", "ux-reviewer": "accessibility",
	} {
		if !strings.Contains(want[key], needle) {
			t.Errorf("%s missing %q", key, needle)
		}
	}
	db, path := priorRoleDatabase(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	got := roleBodies(t, upgraded.db)
	for key, body := range want {
		if got[key] != body {
			t.Errorf("upgraded %s differs from fresh default", key)
		}
	}
}

func TestRoleDefaultsMigrationPreservesEdits(t *testing.T) {
	db, path := priorRoleDatabase(t)
	if _, err := db.Exec(`UPDATE role SET body='owner edit',rev=rev+1 WHERE id='role_worker_default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('proj_role','role','/tmp/role',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO role(id,key,label,body,project_id,role_key) VALUES ('role_scoped','reviewer@proj_role','Reviewer','scoped edit','proj_role','reviewer')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(roleDefaultsMigration()); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"role_worker_default": "owner edit", "role_scoped": "scoped edit"} {
		var got string
		if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("%s body=%q err=%v", id, got, err)
		}
	}
	if got := len(roleBodies(t, s.db)); got != 8 {
		t.Errorf("idempotent migration role count = %d, want 8", got)
	}
}

func TestTeamlessLeadMigrationRewritesOnlyTheUneditedDefault(t *testing.T) {
	t.Parallel()
	s := &Store{db: leaderRoleDatabaseBefore(t)}
	defer s.db.Close()
	if strings.Contains(priorLeadRoleBody, "list_teams") || strings.Contains(leadRoleBody, "list_teams") {
		t.Fatal("the default lead body still sends the lead through saved teams")
	}
	prior := strings.Replace(priorLeadRoleBody, frozenLeadSpawnParagraph, priorLeadSpawnParagraph, 1)
	for name, test := range map[string]struct {
		body string
		rev  int
		want string
	}{"unedited": {prior, 0, priorLeadRoleBody}, "edited body": {prior + " edit", 0, prior + " edit"}, "bumped rev": {prior, 1, prior}} {
		if _, err := s.db.Exec(`UPDATE role SET body=?,rev=? WHERE id='role_lead_default'`, test.body, test.rev); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(roleTeamlessLeadMigration()); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_lead_default'`).Scan(&got); err != nil || got != test.want {
			t.Errorf("%s: body=%q err=%v", name, got, err)
		}
	}
}

func TestLeadDefaultAllowsExplicitSmallTasks(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"For ordinary requests, delegate implementation and independent review through a task room (create_room) and spawn_agent.",
		"When a human explicitly asks you to do a small, bounded task yourself, do it directly, including file changes.",
		"Read the project's AGENTS.md and relevant docs before editing.",
		"If no worker can be spawned",
		"ask the human how to proceed",
	} {
		if !strings.Contains(leadRoleBody, want) {
			t.Errorf("the lead body lacks %q", want)
		}
	}
	for _, body := range []string{workerRoleBody, reviewerRoleBody} {
		if !strings.Contains(body, "before editing") {
			t.Error("the worker and reviewer bodies must keep the read-before-editing sentence")
		}
	}
}

func TestLeadNoImplementMigrationRewritesOnlyTheUneditedDefault(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		body string
		rev  int
		want string
	}{
		"unedited":    {priorLeadRoleBody, 0, priorNoDirectLeadRoleBody},
		"edited body": {priorLeadRoleBody + " edit", 0, priorLeadRoleBody + " edit"},
		"bumped rev":  {priorLeadRoleBody, 1, priorLeadRoleBody},
		"already new": {priorNoDirectLeadRoleBody, 0, priorNoDirectLeadRoleBody},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Store{db: leaderRoleDatabaseBefore(t)}
			defer s.db.Close()
			if _, err := s.db.Exec(`UPDATE role SET body=?,rev=? WHERE id='role_lead_default'`, test.body, test.rev); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(roleLeadNoImplementMigration()); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_lead_default'`).Scan(&got); err != nil || got != test.want {
				t.Errorf("body=%q err=%v", got, err)
			}
		})
	}
}

func TestLeadDirectTaskMigrationPreservesEditedRoles(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		body string
		rev  int
		want string
	}{
		"unedited":    {priorNoDirectLeadRoleBody, 0, priorLeadDirectRoleBody},
		"edited body": {priorNoDirectLeadRoleBody + " edit", 0, priorNoDirectLeadRoleBody + " edit"},
		"bumped rev":  {priorNoDirectLeadRoleBody, 1, priorNoDirectLeadRoleBody},
		"already new": {priorLeadDirectRoleBody, 0, priorLeadDirectRoleBody},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Store{db: leaderRoleDatabaseBefore(t)}
			defer s.db.Close()
			if _, err := s.db.Exec(`UPDATE role SET body=?,rev=? WHERE id='role_lead_default'`, test.body, test.rev); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(roleLeadDirectTaskMigration()); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_lead_default'`).Scan(&got); err != nil || got != test.want {
				t.Errorf("body=%q err=%v", got, err)
			}
		})
	}
}

func TestRoomMentionMigrationUpdatesOnlyUneditedBuiltins(t *testing.T) {
	s := &Store{db: leaderRoleDatabaseBefore(t)}
	defer s.db.Close()
	for _, item := range []struct {
		id, body string
		rev      int
	}{
		{"role_lead_default", priorLeadDirectRoleBody, 0},
		{"role_worker_default", workerRoleBody, 0},
		{"role_reviewer_default", reviewerRoleBody + " owner edit", 0},
	} {
		if _, err := s.db.Exec(`UPDATE role SET body=?,rev=? WHERE id=?`, item.body, item.rev, item.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(roleRoomMentionMigration()); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, want string }{
		{"role_lead_default", priorHandbookLeadRoleBody},
		{"role_worker_default", currentWorkerRoleBody},
		{"role_reviewer_default", reviewerRoleBody + " owner edit"},
	} {
		var got string
		if err := s.db.QueryRow(`SELECT body FROM role WHERE id=?`, item.id).Scan(&got); err != nil || got != item.want {
			t.Fatalf("%s body changed unexpectedly: %v", item.id, err)
		}
	}
}

// goldenLeadBodySHA256 is the SHA-256 of the lead body as it shipped through migration 78 (the output migration 62 left, until migration 79
// moved it on). It is recorded once, from the body those migrations built, so an edit to priorLeadRoleBody, or to anything it was once built
// from, cannot go unnoticed.
const goldenLeadBodySHA256 = "32f1e33a3ed607db94b3c1df2ca751fc138e78351ff8a0db1e2b47e75f5e4d7b"

func TestFrozenLeadBody(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte(priorLeadRoleBody))
	if got := hex.EncodeToString(sum[:]); got != goldenLeadBodySHA256 {
		t.Fatalf("priorLeadRoleBody changed (sha256 %s, golden %s): it is what migrations 32, 35, 62 and 79 look for on an old database and must stay as it shipped", got, goldenLeadBodySHA256)
	}
	for name, piece := range map[string]string{"frozenRoleSafety": frozenRoleSafety, "frozenLeadSpawnParagraph": frozenLeadSpawnParagraph} {
		if strings.Count(priorLeadRoleBody, piece) != 1 {
			t.Errorf("%s is not found exactly once in priorLeadRoleBody", name)
		}
	}
}

// The steps that matched and wrote the lead body still find the frozen literal: the body each of them expects to find (built from the frozen
// pieces) is rewritten to it, and migration 79 then moves it on.
func TestFrozenLeadBodyIsWhatTheEarlierMigrationsWrite(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		before, migration string
	}{
		"migration 35": {strings.Replace(priorLeadRoleBody, frozenRoleSafety, priorRoleSafety, 1), roleFollowupMigration()},
		"migration 62": {strings.Replace(priorLeadRoleBody, frozenLeadSpawnParagraph, priorLeadSpawnParagraph, 1), roleTeamlessLeadMigration()},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Store{db: leaderRoleDatabaseBefore(t)}
			defer s.db.Close()
			if _, err := s.db.Exec(`UPDATE role SET body=?,rev=0 WHERE id='role_lead_default'`, test.before); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(test.migration); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_lead_default'`).Scan(&got); err != nil || got != priorLeadRoleBody {
				t.Errorf("body=%q err=%v", got, err)
			}
			if _, err := s.db.Exec(roleLeadNoImplementMigration()); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_lead_default'`).Scan(&got); err != nil || got != priorNoDirectLeadRoleBody {
				t.Errorf("after migration 79: body=%q err=%v", got, err)
			}
		})
	}
}
