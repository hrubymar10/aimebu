package store

import (
	"context"
	"errors"
	"testing"
)

func roleReviewUsers(t *testing.T, s *Store, projectID string) {
	t.Helper()
	for _, user := range []struct{ id, role, created string }{{"usr_owner", "owner", "1"}, {"usr_operator", "operator", "2"}} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}',?)`, user.id, user.id, user.created); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, projectID, user.id, user.role); err != nil {
			t.Fatal(err)
		}
	}
}

func reviewRole(key string) RoleDefinition {
	return RoleDefinition{Key: key, Label: key, Body: "role", Cardinality: "multi"}
}

// Roles are global, so only the instance owner defines them: a project operator may not create, edit or delete one.
func TestRolesAreDefinedByTheInstanceOwnerOnly(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	project, _, _ := fixture(t, s)
	roleReviewUsers(t, s, project.ID)
	ctx := context.Background()
	if _, err := s.ManageRole(ctx, "usr_operator", reviewRole("analyst"), false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator created a role: %v", err)
	}
	analyst, err := s.ManageRole(ctx, "usr_owner", reviewRole("analyst"), false)
	if err != nil {
		t.Fatalf("owner created a role: %v", err)
	}
	analyst.Body = "operator edit"
	if _, err := s.ManageRole(ctx, "usr_operator", analyst, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator edited a role: %v", err)
	}
	if _, err := s.ManageRole(ctx, "usr_operator", analyst, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator deleted a role: %v", err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", analyst, true); err != nil {
		t.Fatalf("owner deleted a role: %v", err)
	}
}

func TestDeletedRoleKeepsStoppedAgentHistory(t *testing.T) {
	t.Parallel()
	s, project, room, _, _ := wakeFixture(t)
	roleReviewUsers(t, s, project.ID)
	ctx := context.Background()
	role, err := s.ManageRole(ctx, "usr_owner", reviewRole("historian"), false)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreateAgent(ctx, Agent{Slug: "historian", ProjectID: project.ID, RoleID: role.ID, Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_wake", KeyHash: "key_historian", CreatedBy: "usr_owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", role, true); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	card, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil || !card.RoleDeleted || card.RoleKey != "historian" {
		t.Fatalf("historical card=%+v %v", card, err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("historian"), false); err != nil {
		t.Fatalf("recreate key: %v", err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(agent.ID, "usr_owner", "active", card.DesiredRev)
		return err
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reactivated deleted role: %v", err)
	}
}
