package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLegacyPrefixWarning(t *testing.T) {
	t.Parallel()
	known := map[string]bool{"alice": true, "bob": true}
	for _, tc := range []struct {
		body, want string
	}{
		{"alice: please look", "'alice:' prefix"},
		{"Bob : done", "'bob:' prefix"},
		{"ok. alice, bob — sync up", "'alice, bob —'"},
		{"alice and bob: sync", "'alice, bob —'"},
		{"Note: this is prose", ""},
		{"carol: unknown name", ""},
		{"@alice: properly addressed", ""},
		{"`alice:` in code", ""},
		{"```\nalice: in a fence\n```", ""},
		{"say hi to alice: later", ""},
	} {
		got := legacyPrefixWarning(tc.body, known)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%q => %q, want contains %q", tc.body, got, tc.want)
		}
	}
}

func TestAnswerReceiptWarnsAboutLegacyPrefix(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"fine"},{"room":%q,"body":"worker: hello"}]}`, f.room.ID, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if len(receipt.Warnings) != 1 || !strings.HasPrefix(receipt.Warnings[0], "message 1: 'worker:' prefix detected") {
		t.Fatalf("warnings=%q", receipt.Warnings)
	}
}

func TestRoleKeyRejectedWhenItCollidesWithLiveAgent(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	roleReviewUsers(t, s, f.project.ID)
	ctx := context.Background()
	// The fixture agent's slug is "worker".
	// The check needs a slug no role key already uses, or the duplicate-key check fires first.
	scout, err := s.CreateAgent(ctx, Agent{Slug: "scout", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native",
		Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash_scout", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("scout"), false); !errors.Is(err, ErrRoleNameCollision) {
		t.Fatalf("global role over agent slug: %v", err)
	}
	// A stopped agent does not hold its slug either.
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, scout.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("scout"), false); err != nil {
		t.Fatalf("stopped agent still blocks: %v", err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("distinct"), false); err != nil {
		t.Fatalf("non-colliding key: %v", err)
	}
	// A revoked agent no longer holds the slug.
	ranger, err := s.CreateAgent(ctx, Agent{Slug: "ranger", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native",
		Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash_ranger", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("ranger"), false); !errors.Is(err, ErrRoleNameCollision) {
		t.Fatalf("live agent should block: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET revoked_at='now' WHERE id=?`, ranger.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("ranger"), false); err != nil {
		t.Fatalf("revoked agent still blocks: %v", err)
	}
}

func TestAgentSlugAvoidsRoleKeys(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	for _, name := range agentNamePool {
		if name == "yael" {
			continue
		}
		if _, err := s.db.Exec(`INSERT INTO role(id,key,label,body,role_key) VALUES (?,?,?,?,?)`, "role_k_"+name, "k_"+name, name, "b", name); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Tx(context.Background(), func(tx *Tx) error {
		slug, err := tx.nextAgentSlug(f.project.ID)
		if slug != "yael" {
			t.Fatalf("slug=%q err=%v", slug, err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRevokedAgentDoesNotHideRoleMention(t *testing.T) {
	t.Parallel()
	members := []roomRecipient{
		{id: "agt_old", slug: "reviewer", kind: "agent", role: "worker", revoked: true},
		{id: "agt_live", slug: "kai", kind: "agent", role: "reviewer"},
	}
	got := resolveMentionTargets("@reviewer please look", "usr_x", members, "prj", "proj")
	if got["agt_live"] != "role" || len(got) != 1 {
		t.Fatalf("live role holder must win over a revoked slug: %v", got)
	}
	// With no live holder the revoked agent is still addressed, so the wake
	// is recorded as skipped instead of vanishing.
	got = resolveMentionTargets("@reviewer please look", "usr_x", members[:1], "prj", "proj")
	if got["agt_old"] != "mention" {
		t.Fatalf("revoked-only slug dropped: %v", got)
	}
}

func TestRoleKeyRejectedWhenItCollidesWithHumanLogin(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	roleReviewUsers(t, s, f.project.ID)
	ctx := context.Background()
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("usr_operator"), false); !errors.Is(err, ErrRoleNameCollision) {
		t.Fatalf("role over a human: %v", err)
	}
	// Roles are global, so a human who is in no project blocks the key as well.
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_elsewhere','elsewhere','hash','{}','3')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("elsewhere"), false); !errors.Is(err, ErrRoleNameCollision) {
		t.Fatalf("role over a human outside every project: %v", err)
	}
	// A disabled human no longer holds the name.
	if _, err := s.db.Exec(`UPDATE user SET disabled_at='now' WHERE id='usr_operator'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", reviewRole("usr_operator"), false); err != nil {
		t.Fatalf("disabled human still blocks: %v", err)
	}
}
