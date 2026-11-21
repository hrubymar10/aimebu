package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/prompt"
)

func promptFixture(t *testing.T) (*Store, Project, Room, Agent, Agent) {
	t.Helper()
	s, project, room, worker, lead := wakeFixture(t)
	roleReviewUsers(t, s, project.ID)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_viewer','viewer','hash','{}','3')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, project.ID, "usr_viewer", "viewer"); err != nil {
		t.Fatal(err)
	}
	return s, project, room, worker, lead
}

func slotByKey(t *testing.T, slots []PromptSlot, key string) PromptSlot {
	t.Helper()
	for _, slot := range slots {
		if slot.Key == key {
			return slot
		}
	}
	t.Fatalf("no slot %q", key)
	return PromptSlot{}
}

func TestPromptOverridesResolveProjectOverGlobalOverDefault(t *testing.T) {
	t.Parallel()
	s, project, _, _, _ := promptFixture(t)
	ctx := context.Background()
	shipped := slotByKey(t, mustPrompts(t, s, ""), "mentions").Default
	if got := slotByKey(t, mustPrompts(t, s, project.ID), "mentions"); got.Source != "default" || got.Effective != shipped {
		t.Fatalf("nothing saved: %+v", got)
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", "", "mentions", "  global mention text  ", 0); err != nil {
		t.Fatal(err)
	}
	if got := slotByKey(t, mustPrompts(t, s, project.ID), "mentions"); got.Source != "global" || got.Effective != "global mention text" || got.Global == nil || got.Global.Rev != 1 {
		t.Fatalf("global: %+v", got)
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", project.ID, "mentions", "project mention text", 0); err != nil {
		t.Fatal(err)
	}
	got := slotByKey(t, mustPrompts(t, s, project.ID), "mentions")
	if got.Source != "project" || got.Effective != "project mention text" || got.Global == nil || got.Project == nil {
		t.Fatalf("project: %+v", got)
	}
	// The global view does not see the project's text.
	if got := slotByKey(t, mustPrompts(t, s, ""), "mentions"); got.Source != "global" || got.Project != nil {
		t.Fatalf("global view: %+v", got)
	}
	// Reset peels one layer at a time.
	if err := s.ResetPromptOverride(ctx, "usr_owner", project.ID, "mentions", 1); err != nil {
		t.Fatal(err)
	}
	if got := slotByKey(t, mustPrompts(t, s, project.ID), "mentions"); got.Source != "global" {
		t.Fatalf("after project reset: %+v", got)
	}
	if err := s.ResetPromptOverride(ctx, "usr_owner", "", "mentions", 1); err != nil {
		t.Fatal(err)
	}
	if got := slotByKey(t, mustPrompts(t, s, project.ID), "mentions"); got.Source != "default" || got.Effective != shipped {
		t.Fatalf("after global reset: %+v", got)
	}
}

func mustPrompts(t *testing.T, s *Store, projectID string) []PromptSlot {
	t.Helper()
	slots, err := s.ListPrompts(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	return slots
}

func TestPromptOverrideValidationRevisionsAndPermissions(t *testing.T) {
	t.Parallel()
	s, project, _, _, _ := promptFixture(t)
	ctx := context.Background()
	for _, c := range []struct{ key, value string }{
		{"output_contract", "x"}, {"mentions", "   "}, {"mentions", strings.Repeat("é", prompt.MaxOverrideLen+1)},
	} {
		if err := s.SetPromptOverride(ctx, "usr_owner", "", c.key, c.value, 0); !errors.Is(err, ErrPromptOverride) {
			t.Errorf("%q / %d chars should be rejected: %v", c.key, len(c.value), err)
		}
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", "", "tail", strings.Repeat("é", prompt.MaxOverrideLen), 0); err != nil {
		t.Fatalf("the cap counts characters, not bytes: %v", err)
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", "", "tail", "again", 0); !errors.Is(err, ErrStaleRev) {
		t.Errorf("creating over an existing override: %v", err)
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", "", "tail", "changed", 9); !errors.Is(err, ErrStaleRev) {
		t.Errorf("a stale revision: %v", err)
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", "", "tail", "changed", 1); err != nil {
		t.Fatal(err)
	}
	if got := slotByKey(t, mustPrompts(t, s, ""), "tail"); got.Global.Rev != 2 || got.Effective != "changed" {
		t.Errorf("revision moves on: %+v", got)
	}
	if err := s.ResetPromptOverride(ctx, "usr_owner", "", "tail", 1); !errors.Is(err, ErrStaleRev) {
		t.Errorf("resetting a stale revision: %v", err)
	}
	// Only the instance owner edits the global scope; a project's owner and operators edit their project; viewers and outsiders cannot.
	if err := s.SetPromptOverride(ctx, "usr_operator", "", "tail", "x", 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("an operator on the global scope: %v", err)
	}
	if err := s.SetPromptOverride(ctx, "usr_operator", project.ID, "tail", "operator text", 0); err != nil {
		t.Errorf("an operator on their project: %v", err)
	}
	if err := s.SetPromptOverride(ctx, "usr_viewer", project.ID, "identity", "x", 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("a viewer: %v", err)
	}
	if err := s.SetPromptOverride(ctx, "usr_nobody", project.ID, "identity", "x", 0); err == nil {
		t.Error("a user outside the project must not edit it")
	}
}

// A turn's context carries the resolved overrides, and a lead's instructions carry the inline-plan sentence only when asked for.
func TestDispatchCarriesPromptOverridesAndInlinePlans(t *testing.T) {
	t.Parallel()
	s, project, room, worker, lead := promptFixture(t)
	ctx := context.Background()
	read := func(agent Agent) (map[string]string, string) {
		t.Helper()
		if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@" + agent.Slug + " go"}); err != nil {
			t.Fatal(err)
		}
		delivery, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
		if err != nil || !ready {
			t.Fatalf("prepare ready=%t err=%v", ready, err)
		}
		var data struct {
			RoleBody  string            `json:"role_body"`
			Overrides map[string]string `json:"prompt_overrides"`
		}
		if err := json.Unmarshal(delivery.Context, &data); err != nil {
			t.Fatal(err)
		}
		if err := s.CancelPreparedDispatch(ctx, delivery.TurnID); err != nil {
			t.Fatal(err)
		}
		return data.Overrides, data.RoleBody
	}
	if overrides, _ := read(worker); overrides != nil {
		t.Fatalf("nothing saved, so the context carries no overrides: %v", overrides)
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", "", "tail", "GLOBAL TAIL", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", "", "mentions", "GLOBAL MENTIONS", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPromptOverride(ctx, "usr_owner", project.ID, "tail", "PROJECT TAIL", 0); err != nil {
		t.Fatal(err)
	}
	overrides, _ := read(worker)
	if len(overrides) != 2 || overrides["tail"] != "PROJECT TAIL" || overrides["mentions"] != "GLOBAL MENTIONS" {
		t.Fatalf("the project's text wins and the rest is global: %v", overrides)
	}

	if got, _ := s.InlinePlanAppendix(ctx); got != InlinePlanOptional {
		t.Fatalf("the default leaves the plan appendix optional: %q", got)
	}
	_, leadBody := read(lead)
	if strings.Contains(leadBody, "always attach the full prose as appendix_pages") {
		t.Fatal("optional adds nothing to the lead")
	}
	if err := s.SetInlinePlanAppendix(ctx, "usr_operator", InlinePlanAlways); !errors.Is(err, ErrForbidden) {
		t.Fatalf("only the owner changes it: %v", err)
	}
	if err := s.SetInlinePlanAppendix(ctx, "usr_owner", "sometimes"); !errors.Is(err, ErrPromptOverride) {
		t.Fatalf("an unknown value: %v", err)
	}
	if err := s.SetInlinePlanAppendix(ctx, "usr_owner", InlinePlanAlways); err != nil {
		t.Fatal(err)
	}
	_, leadBody = read(lead)
	if !strings.Contains(leadBody, "always attach the full prose as appendix_pages") {
		t.Fatalf("always adds the sentence to a lead: %q", leadBody)
	}
	if _, workerBody := read(worker); strings.Contains(workerBody, "always attach the full prose as appendix_pages") {
		t.Fatal("a worker is not asked for the appendix")
	}
}

// Migration 57 leaves a database as it found it apart from the new table, and a scope holds one row per slot.
func TestPromptOverrideTableKeepsOneRowPerScopeAndSlot(t *testing.T) {
	t.Parallel()
	s, project, _, _, _ := promptFixture(t)
	if _, err := s.db.Exec(`INSERT INTO prompt_override(id,project_id,key,value,updated_at) VALUES ('a',NULL,'tail','x','t'),('b',?,'tail','y','t')`, project.ID); err != nil {
		t.Fatalf("a global and a project row for one slot coexist: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO prompt_override(id,project_id,key,value,updated_at) VALUES ('c',NULL,'tail','z','t')`); err == nil {
		t.Fatal("two global rows for one slot must be refused")
	}
}
