package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestUsageReadAndCompactionNoticeDedup(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	usageFrame, _ := json.Marshal(map[string]any{"type": "usage", "usage": map[string]any{
		"tokens_in": 5000, "tokens_out": 1200, "context_used": 180000, "context_window": 200000, "cost_estimate": 0.35,
	}})
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "usage-1", "usage", "", usageFrame); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "usage-1", "usage", "", usageFrame); err != nil {
		t.Fatal(err)
	}
	usage, err := s.AgentUsage(ctx, f.agent.ID)
	if err != nil || usage == nil || usage.ContextUsed == nil || *usage.ContextUsed != 180000 || usage.ContextWindow == nil || *usage.ContextWindow != 200000 || usage.CostSoFar == nil || *usage.CostSoFar != 0.35 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_record WHERE agent_id=?`, f.agent.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("usage rows=%d err=%v", count, err)
	}
	compactFrame, _ := json.Marshal(map[string]any{"type": "compaction", "compaction": map[string]any{"pre_tokens": 180000, "post_tokens": 40000}})
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "compact-1", "compaction", "", compactFrame); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "compact-1", "compaction", "", compactFrame); err != nil {
		t.Fatal(err)
	}
	messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].AuthorKind != "system" || !strings.Contains(messages[1].Body, "worker's memory was trimmed (180k → 40k tokens)") {
		t.Fatalf("messages=%+v", messages)
	}
	unknownFrame, _ := json.Marshal(map[string]any{"type": "compaction", "compaction": map[string]any{"pre_tokens": 0, "post_tokens": 0}})
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "compact-2", "compaction", "", unknownFrame); err != nil {
		t.Fatal(err)
	}
	messages, err = s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) != 3 || !strings.Contains(messages[2].Body, "(— → — tokens)") {
		t.Fatalf("unknown compaction messages=%+v err=%v", messages, err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET generation=1 WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	usage, err = s.AgentUsage(ctx, f.agent.ID)
	if err != nil || usage == nil || usage.ContextUsed != nil || usage.CostSoFar == nil || *usage.CostSoFar != 0.35 {
		t.Fatalf("new generation usage=%+v err=%v", usage, err)
	}
}

func TestEstimatedCostIsNotReportedAsKnown(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE turn SET started_at=? WHERE id=?`, timestamp(), f.turn.ID); err != nil {
		t.Fatal(err)
	}
	frame := usageFrame(map[string]any{"tokens_in": 20, "tokens_out": 7, "cost_estimate": 0.00012, "cost_source": "estimated"})
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "estimated-1", "usage", "", frame); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := s.db.QueryRow(`SELECT source FROM usage_record WHERE source_event_id='estimated-1'`).Scan(&source); err != nil || source != "estimated" {
		t.Fatalf("source=%q err=%v", source, err)
	}
	agent, err := s.AgentUsage(ctx, f.agent.ID)
	if err != nil || agent == nil || !agent.CostEstimated {
		t.Fatalf("agent usage=%+v err=%v", agent, err)
	}
	totals, err := s.UsageTotalsFor(ctx, "project", f.project.ID, "all")
	if err != nil || totals.CostCoverage != "estimated" || totals.TurnsWithEstimatedCost != 1 || len(totals.ByModel) != 1 || !totals.ByModel[0].CostEstimated {
		t.Fatalf("totals=%+v err=%v", totals, err)
	}
	limit, err := s.UsageLimit(ctx, f.project.ID)
	if err != nil || limit.CostCoverage != "estimated" || limit.TurnsWithEstimatedCost != 1 {
		t.Fatalf("limit=%+v err=%v", limit, err)
	}
}

// The newest record of the agent's generation is the one with the latest timestamp, and the later insert when two share a timestamp.
func TestAgentUsageReadsTheNewestRecordOfTheGeneration(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	for i, used := range []int{100, 200} {
		frame, _ := json.Marshal(map[string]any{"type": "usage", "usage": map[string]any{"context_used": used, "context_window": 1000}})
		if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, fmt.Sprintf("usage-%d", i), "usage", "", frame); err != nil {
			t.Fatal(err)
		}
	}
	set := func(first, second string) int64 {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE usage_record SET created_at=? WHERE source_event_id='usage-0'`, first); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE usage_record SET created_at=? WHERE source_event_id='usage-1'`, second); err != nil {
			t.Fatal(err)
		}
		usage, err := s.AgentUsage(ctx, f.agent.ID)
		if err != nil || usage == nil || usage.ContextUsed == nil {
			t.Fatalf("usage=%+v err=%v", usage, err)
		}
		return *usage.ContextUsed
	}
	const early, late = "2026-01-01T10:00:00.000Z", "2026-01-01T10:05:00.000Z"
	if got := set(early, late); got != 200 {
		t.Fatalf("the later timestamp should win: %d", got)
	}
	if got := set(late, early); got != 100 {
		t.Fatalf("the order is by timestamp, not by insert order: %d", got)
	}
	if got := set(early, early); got != 200 {
		t.Fatalf("with equal timestamps the later insert should win: %d", got)
	}
	// A record of an older generation is never the newest, however late it is stamped.
	if _, err := s.db.Exec(`UPDATE agent SET generation=generation+1 WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	usage, err := s.AgentUsage(ctx, f.agent.ID)
	if err != nil || (usage != nil && usage.ContextUsed != nil) {
		t.Fatalf("an earlier generation's record was read: %+v err=%v", usage, err)
	}
}

// A partial record (a turn that ended with no report) has no context, so the card keeps showing the last complete report.
func TestAgentUsageSkipsPartialRecords(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	full, _ := json.Marshal(map[string]any{"type": "usage", "usage": map[string]any{"tokens_in": 10, "context_used": 5000, "context_window": 200000}})
	partial, _ := json.Marshal(map[string]any{"type": "usage", "usage": map[string]any{"tokens_in": 3, "partial": true}})
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "full", "usage", "", full); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "partial", "usage", "", partial); err != nil {
		t.Fatal(err)
	}
	usage, err := s.AgentUsage(ctx, f.agent.ID)
	if err != nil || usage == nil || usage.ContextUsed == nil || *usage.ContextUsed != 5000 || usage.TokensIn == nil || *usage.TokensIn != 10 {
		t.Fatalf("the card must show the last complete report: %+v %v", usage, err)
	}
}
