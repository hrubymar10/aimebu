package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestQuotaConfigAndStateRoundTripPerTool(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, found, err := s.QuotaConfig(ctx, "claude"); err != nil || found {
		t.Fatalf("a fresh store has no config: found=%v err=%v", found, err)
	}
	if err := s.SaveQuotaConfig(ctx, "claude", `{"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveQuotaConfig(ctx, "claude", `{"enabled":false}`); err != nil { // upsert
		t.Fatal(err)
	}
	if err := s.SaveQuotaConfig(ctx, "codex", `{"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.QuotaConfig(ctx, "claude"); err != nil || !found || got != `{"enabled":false}` {
		t.Fatalf("claude config = %q %v %v", got, found, err)
	}
	if got, _, _ := s.QuotaConfig(ctx, "codex"); got != `{"enabled":true}` {
		t.Fatalf("codex config = %q", got)
	}
	if err := s.SaveQuotaState(ctx, "claude", `{"tripped":true}`); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.QuotaState(ctx, "claude"); err != nil || !found || got != `{"tripped":true}` {
		t.Fatalf("state = %q %v %v", got, found, err)
	}
	if _, found, _ := s.QuotaState(ctx, "codex"); found {
		t.Fatal("state is per tool")
	}
	if err := s.SaveQuotaConfig(ctx, "pi", `{}`); err == nil {
		t.Fatal("an unknown tool must be rejected by the table")
	}
}

func TestQuotaDecisionsKeepTheNewestFiftyPerTool(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	for i := 0; i < QuotaDecisionKeep+7; i++ {
		if err := s.AppendQuotaDecision(ctx, "claude", fmt.Sprintf("2026-10-17T12:00:%02dZ", i%60), fmt.Sprintf(`{"n":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendQuotaDecision(ctx, "codex", "2026-10-17T12:00:00Z", `{"n":"codex"}`); err != nil {
		t.Fatal(err)
	}
	claude, err := s.QuotaDecisions(ctx, "claude", 100)
	if err != nil || len(claude) != QuotaDecisionKeep {
		t.Fatalf("claude kept %d (%v), want %d", len(claude), err, QuotaDecisionKeep)
	}
	if claude[0] != fmt.Sprintf(`{"n":%d}`, QuotaDecisionKeep+6) || claude[len(claude)-1] != `{"n":7}` {
		t.Fatalf("order/trim wrong: first %s last %s", claude[0], claude[len(claude)-1])
	}
	if codex, _ := s.QuotaDecisions(ctx, "codex", 100); len(codex) != 1 {
		t.Fatalf("trimming must be per tool: %v", codex)
	}
	if few, _ := s.QuotaDecisions(ctx, "claude", 3); len(few) != 3 || few[0] != claude[0] {
		t.Fatalf("limit: %v", few)
	}
}

func TestQuotaEventsAreOwnerOnlySystemEvents(t *testing.T) {
	t.Parallel()
	s, _, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordQuotaEvent(ctx, "quota_switch", map[string]string{"tool": "claude", "from": "a", "to": "b", "reason": "the active profile is over its limit or unusable"}); err != nil {
		t.Fatal(err)
	}
	events, err := s.RecentSystemEvents(ctx, owner, 20)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		found = found || (event.Kind == "quota_switch" && strings.Contains(event.PayloadJSON, `"to":"b"`))
	}
	if !found {
		t.Fatalf("the owner does not see the event: %+v", events)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_member','member','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if other, err := s.RecentSystemEvents(ctx, "usr_member", 20); err != nil || len(other) != 0 {
		t.Fatalf("a non-owner sees a project-less quota event: %+v %v", other, err)
	}
}
