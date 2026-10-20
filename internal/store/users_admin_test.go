package store

import (
	"context"
	"testing"
	"time"
)

// Links an account issued under authority it has since lost are spent, not just refused when used: the row says so.
func TestLosingAdminSpendsTheAccountsUnusedInvitesAndNobodyElses(t *testing.T) {
	for name, end := range map[string]func(s *Store, ctx context.Context) error{
		"the flag is taken away":     func(s *Store, ctx context.Context) error { return s.SetUserAdmin(ctx, "a_owner", "b_admin", false) },
		"the account is deactivated": func(s *Store, ctx context.Context) error { return s.DeactivateUser(ctx, "a_owner", "b_admin") },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := testStore(t, Options{})
			ctx := context.Background()
			for _, login := range []string{"a_owner", "b_admin", "c_user"} {
				if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, login, login); err != nil {
					t.Fatal(err)
				}
			}
			project, err := s.CreateProject(ctx, Project{Name: "p", Folder: "/tmp/p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, project.ID, "a_owner"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetUserAdmin(ctx, "a_owner", "b_admin", true); err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Hour)
			issue := func(actor, kind, target, hash string) {
				t.Helper()
				projectID, role := "", ""
				if kind == "join" {
					projectID, role = project.ID, "viewer"
				}
				if _, err := s.CreateUserInvite(ctx, actor, kind, projectID, role, target, hash, expires); err != nil {
					t.Fatalf("%s issues %s: %v", actor, kind, err)
				}
			}
			issue("b_admin", "reset", "c_user", "h1")
			issue("b_admin", "join", "", "h2")
			issue("a_owner", "reset", "c_user", "h3")
			unused := func(creator string) int {
				var n int
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_invite WHERE created_by=? AND used_at IS NULL`, creator).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			if unused("b_admin") != 2 || unused("a_owner") != 1 {
				t.Fatalf("setup: admin %d, owner %d unused", unused("b_admin"), unused("a_owner"))
			}
			if err := end(s, ctx); err != nil {
				t.Fatal(err)
			}
			if got := unused("b_admin"); got != 0 {
				t.Errorf("%d of the admin's links are still unused after %s", got, name)
			}
			if got := unused("a_owner"); got != 1 {
				t.Errorf("the owner's link was touched: %d unused, want 1", got)
			}
			var spent int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_invite WHERE created_by='b_admin' AND used_at IS NOT NULL`).Scan(&spent); err != nil || spent != 2 {
				t.Errorf("the admin's two links should be marked spent: %d %v", spent, err)
			}
		})
	}
}

func TestRevokingInvitesLeavesAnAlreadyUsedOneAlone(t *testing.T) {
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	for _, login := range []string{"a_owner", "b_admin"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, login, login); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetUserAdmin(ctx, "a_owner", "b_admin", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUserInvite(ctx, "b_admin", "reset", "", "", "a_owner", "h1", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("an admin must not be able to issue a reset for the owner")
	}
	if _, err := s.db.Exec(`INSERT INTO user_invite(id,token_hash,kind,target_user_id,created_by,expires_at,used_at,created_at) VALUES ('inv_used','h2','reset','b_admin','b_admin','2999-01-01T00:00:00.000Z','2026-01-01T00:00:00.000Z','2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserAdmin(ctx, "a_owner", "b_admin", false); err != nil {
		t.Fatal(err)
	}
	var usedAt string
	if err := s.db.QueryRow(`SELECT used_at FROM user_invite WHERE id='inv_used'`).Scan(&usedAt); err != nil || usedAt != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("an invite that was already used had its mark rewritten: %q %v", usedAt, err)
	}
}
