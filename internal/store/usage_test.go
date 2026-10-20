package store

import (
	"context"
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
