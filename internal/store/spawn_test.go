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

func TestPickAgentSlugNumbersTheNamesOncePoolIsUsedUp(t *testing.T) {
	taken := map[string]bool{}
	name, err := pickAgentSlug(taken)
	if err != nil || !ValidAgentSlug(name) || taken[name] {
		t.Fatalf("first pick %q %v", name, err)
	}
	for _, base := range agentNamePool {
		taken[base] = true
	}
	for i := 0; i < 20; i++ {
		name, err := pickAgentSlug(taken)
		if err != nil || !ValidAgentSlug(name) || !strings.HasSuffix(name, "2") || taken[name] || !taken[strings.TrimSuffix(name, "2")] {
			t.Fatalf("pick with the pool used up: %q %v", name, err)
		}
	}
	// Only some of the pool is taken: the free names are all that comes back.
	taken = map[string]bool{}
	for _, base := range agentNamePool[1:] {
		taken[base] = true
	}
	for i := 0; i < 20; i++ {
		if name, err := pickAgentSlug(taken); err != nil || name != agentNamePool[0] {
			t.Fatalf("pick with one name free: %q %v, want %q", name, err, agentNamePool[0])
		}
	}
}

// A group tag wins over a name in a mention, so no agent can be called one.
func TestValidAgentSlugRefusesGroupTags(t *testing.T) {
	for _, tag := range []string{"all", "here", "channel", "everyone", "ais", "humans"} {
		if ValidAgentSlug(tag) {
			t.Errorf("%q is a group tag and must not be an agent name", tag)
		}
	}
	for _, name := range agentNamePool {
		if !ValidAgentSlug(name) {
			t.Errorf("pool name %q must be a valid agent name", name)
		}
	}
}

func TestSuggestAgentSlugNeverAnswersTheExcludedName(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	fixture := seedExecution(t, s)
	err := s.Tx(context.Background(), func(tx *Tx) error {
		for i, name := range agentNamePool {
			if name == "alice" || name == "bob" {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES (?,?,?,?,'fake','test','test','fake',?,?,?,'owner',?)`, fmt.Sprintf("agt_excl_%d", i), name, fixture.project.ID, fixture.agent.RoleID, fixture.agent.MachineID, fixture.agent.CheckoutID, "hash", timestamp()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := s.SuggestAgentSlug(t.Context(), fixture.project.ID, "Alice")
		if err != nil || got != "bob" {
			t.Fatalf("suggestion excluding alice = %q, %v; want bob", got, err)
		}
	}
	if got, err := s.SuggestAgentSlug(t.Context(), fixture.project.ID, ""); err != nil || (got != "alice" && got != "bob") {
		t.Fatalf("suggestion without exclude = %q, %v", got, err)
	}
}
