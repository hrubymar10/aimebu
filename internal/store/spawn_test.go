package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestAgentNamePoolUsesNumericSuffixAfterExhaustion(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	fixture := seedExecution(t, s)
	err := s.Tx(context.Background(), func(tx *Tx) error {
		for i, name := range agentNamePool {
			_, err := tx.Exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES (?,?,?,?,'fake','test','test','fake',?,?,?,'owner',?)`, fmt.Sprintf("agt_pool_%d", i), name, fixture.project.ID, fixture.agent.RoleID, fixture.agent.MachineID, fixture.agent.CheckoutID, "hash", timestamp())
			if err != nil {
				return err
			}
		}
		slug, err := tx.nextAgentSlug(fixture.project.ID)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(slug, "2") {
			t.Fatalf("exhausted pool slug=%q", slug)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentNamePoolAvoidsProjectUserLogin(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	fixture := seedExecution(t, s)
	err := s.Tx(context.Background(), func(tx *Tx) error {
		for i, name := range agentNamePool {
			if name == "alice" {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES (?,?,?,?,'fake','test','test','fake',?,?,?,'owner',?)`, fmt.Sprintf("agt_taken_%d", i), name, fixture.project.ID, fixture.agent.RoleID, fixture.agent.MachineID, fixture.agent.CheckoutID, "hash", timestamp()); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_alice','Alice','hash','{}',?)`, timestamp()); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_alice','viewer')`, fixture.project.ID); err != nil {
			return err
		}
		slug, err := tx.nextAgentSlug(fixture.project.ID)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(slug, "2") {
			t.Fatalf("human login collision: %q", slug)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
