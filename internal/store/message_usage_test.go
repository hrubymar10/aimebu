package store

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// Every message of a turn shows the turn's figures with its place among the turn's messages; a turn's several usage events are summed once, and
// a figure nobody reported stays unknown.
func TestUsageForMessagesCarriesTheTurnFiguresOnEachMessage(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"first"},{"room":%q,"body":"second"}]}`, f.room.ID, f.room.ID)
	if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw); err != nil || receipt.Result != "committed" {
		t.Fatalf("commit: %+v %v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) != 3 {
		t.Fatalf("messages: %+v %v", messages, err)
	}
	// Nothing reported yet: no entry, however the turn went.
	if got, err := s.UsageForMessages(ctx, messages); err != nil || len(got) != 0 {
		t.Fatalf("before any usage: %+v %v", got, err)
	}
	record := func(id string, fields map[string]any) {
		t.Helper()
		if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, id, "usage", "", usageFrame(fields)); err != nil {
			t.Fatal(err)
		}
	}
	record("u1", map[string]any{"tokens_in": 10, "tokens_out": 5, "tokens_cache_read": 100, "cost_estimate": 0.01})
	record("u2", map[string]any{"tokens_in": 4, "tokens_out": 1, "cost_estimate": 0.005})
	got, err := s.UsageForMessages(ctx, messages)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[messages[0].ID]; ok {
		t.Fatal("the human's message has a usage")
	}
	for i, want := range []int{1, 2} {
		usage, ok := got[messages[i+1].ID]
		if !ok || usage.Index != want || usage.Count != 2 || usage.TurnID != f.turn.ID {
			t.Fatalf("message %d: %+v", i+1, usage)
		}
		if usage.TokensIn == nil || *usage.TokensIn != 14 || usage.TokensOut == nil || *usage.TokensOut != 6 || usage.CacheRead == nil || *usage.CacheRead != 100 ||
			usage.Cost == nil || math.Abs(*usage.Cost-0.015) > 1e-12 || usage.Model != "test" || usage.Partial {
			t.Fatalf("the turn's figures on message %d: %+v", i+1, usage)
		}
		if usage.CacheWrite != nil {
			t.Fatalf("a figure nobody reported must stay unknown, not zero: %+v", usage)
		}
	}
	// Partial spend and an unknown cost come through as they are.
	record("u3", map[string]any{"tokens_in": 3, "partial": true})
	if _, err := s.db.Exec(`UPDATE usage_record SET cost_est=NULL`); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UsageForMessages(ctx, messages)
	usage := got[messages[1].ID]
	if !usage.Partial || usage.Cost != nil || usage.TokensIn == nil || *usage.TokensIn != 17 {
		t.Fatalf("partial and unpriced: %+v", usage)
	}
}

// The three queries serve a page of any size: no query is made per message.
func TestUsageForMessagesReadsByKeyNotByScan(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	for _, query := range []string{
		`SELECT id,turn_id FROM outbox WHERE id IN (?,?)`,
		`SELECT turn_id,id FROM outbox WHERE state='committed' AND turn_id IN (?,?) ORDER BY turn_id,answer_rev,idx`,
		`SELECT turn_id,SUM(tokens_in),SUM(tokens_out),SUM(tokens_cache_read),SUM(tokens_cache_write),SUM(cost_est),MAX(partial),COALESCE(MAX(model),'') FROM usage_record WHERE turn_id IN (?,?) GROUP BY turn_id`,
	} {
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, "a", "b")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if len(detail) >= 4 && detail[:4] == "SCAN" {
				t.Errorf("%s: %s", query, detail)
			}
		}
		rows.Close()
	}
}
