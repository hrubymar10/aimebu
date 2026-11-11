package store

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// usageFrame is the frame an adapter's usage event becomes.
func usageFrame(fields map[string]any) []byte {
	data, _ := json.Marshal(map[string]any{"type": "usage", "usage": fields})
	return data
}

// nextTurn ends the agent's active turn and starts another in the given session.
func nextTurn(t *testing.T, s *Store, f executionFixture, session string, generation int) Turn {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited' WHERE agent_id=? AND exec IN ('pending','running')`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?,?,?,'fresh_with_history','now')`, session, f.agent.ID, generation); err != nil {
		t.Fatal(err)
	}
	turn, err := s.RecordTurn(context.Background(), Turn{AgentID: f.agent.ID, SessionID: session, Generation: generation, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func costOf(t *testing.T, s *Store, sourceID string) (cost, total sql.NullFloat64) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT cost_est,cost_total FROM usage_record WHERE source_event_id=?`, sourceID).Scan(&cost, &total); err != nil {
		t.Fatalf("%s: %v", sourceID, err)
	}
	return
}

// Claude reports a resumed session's whole spend in every result, so a turn costs the difference from the session's previous total. This is the
// sequence a real three-turn run produced (the totals grew while each turn's own tokens were small).
func TestClaudeCostIsPricedAsTheDifferenceFromTheSessionTotal(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	record := func(turn Turn, id string, fields map[string]any) {
		t.Helper()
		if err := s.RecordTurnEvent(ctx, turn.ID, f.agent.ID, id, "usage", "", usageFrame(fields)); err != nil {
			t.Fatal(err)
		}
	}
	near := func(got sql.NullFloat64, want float64) bool { return got.Valid && math.Abs(got.Float64-want) < 1e-9 }
	cumulative := func(total float64) map[string]any {
		return map[string]any{"cost_estimate": total, "cost_cumulative": true, "tokens_in": 5}
	}

	record(f.turn, "a", cumulative(0.001333))
	record(nextTurn(t, s, f, "ses_test", 0), "b", cumulative(0.002172))
	record(nextTurn(t, s, f, "ses_test", 0), "c", cumulative(0.002811))
	for id, want := range map[string]float64{"a": 0.001333, "b": 0.000839, "c": 0.000639} {
		if cost, _ := costOf(t, s, id); !near(cost, want) {
			t.Errorf("turn %s costs %+v, want %v", id, cost, want)
		}
	}
	var sum float64
	if err := s.db.QueryRow(`SELECT SUM(cost_est) FROM usage_record`).Scan(&sum); err != nil || math.Abs(sum-0.002811) > 1e-9 {
		t.Fatalf("what the limits sum is %v (%v), want the session's own total 0.002811", sum, err)
	}
	if _, total := costOf(t, s, "c"); !near(total, 0.002811) {
		t.Errorf("the running total is not remembered: %+v", total)
	}

	// A new session (a later generation) starts from nothing: its first report is its own cost, not a difference from the old one.
	record(nextTurn(t, s, f, "ses_next", 1), "d", cumulative(0.0005))
	if cost, _ := costOf(t, s, "d"); !near(cost, 0.0005) {
		t.Errorf("the first report of a new session is %+v, want 0.0005", cost)
	}
	// A total that went down is unknown, never zero, but it is remembered: the next turn subtracts from it.
	record(nextTurn(t, s, f, "ses_next", 1), "e", cumulative(0.0004))
	if cost, total := costOf(t, s, "e"); cost.Valid || !near(total, 0.0004) {
		t.Errorf("a falling total: cost %+v total %+v, want unknown cost and a remembered 0.0004", cost, total)
	}
	record(nextTurn(t, s, f, "ses_next", 1), "g", cumulative(0.0009))
	if cost, _ := costOf(t, s, "g"); !near(cost, 0.0005) {
		t.Errorf("after the fall: %+v, want 0.0005", cost)
	}
	// A harness that reports each turn's own cost (not a running total) is recorded as it is.
	record(nextTurn(t, s, f, "ses_next", 1), "h", map[string]any{"cost_estimate": 0.01})
	if cost, total := costOf(t, s, "h"); !near(cost, 0.01) || total.Valid {
		t.Errorf("a per-turn cost: %+v %+v", cost, total)
	}
	// A turn with no cost leaves the chain alone: no row becomes the previous total.
	record(nextTurn(t, s, f, "ses_next", 1), "i", map[string]any{"tokens_in": 3, "partial": true})
	if cost, total := costOf(t, s, "i"); cost.Valid || total.Valid {
		t.Errorf("a turn with no cost: %+v %+v", cost, total)
	}
	record(nextTurn(t, s, f, "ses_next", 1), "j", cumulative(0.0012))
	if cost, _ := costOf(t, s, "j"); !near(cost, 0.0003) {
		t.Errorf("after a turn with no cost: %+v, want 0.0003", cost)
	}
}

func TestResumedGenerationKeepsNativeSessionCostBaseline(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	const handle = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	if _, err := s.db.Exec(`UPDATE session SET native_session_id=? WHERE id='ses_test'`, handle); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "before-stop", "usage", "", usageFrame(map[string]any{"cost_estimate": 1.25, "cost_cumulative": true})); err != nil {
		t.Fatal(err)
	}
	continued := nextTurn(t, s, f, "ses_continued", 1)
	if _, err := s.db.Exec(`UPDATE session SET native_session_id=?,start_mode='native_resume' WHERE id='ses_continued'`, handle); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, continued.ID, f.agent.ID, "after-start", "usage", "", usageFrame(map[string]any{"cost_estimate": 1.40, "cost_cumulative": true})); err != nil {
		t.Fatal(err)
	}
	cost, _ := costOf(t, s, "after-start")
	if !cost.Valid || math.Abs(cost.Float64-0.15) > 1e-9 {
		t.Fatalf("continued native session charged %+v rather than 0.15", cost)
	}
}

// A Claude session that spans the upgrade carries on from the total its last pre-upgrade turn stored, so its next turn is priced as the
// difference and not as the whole session over again. Only Claude's stored cost was a running total.
func TestMigrationStartsTheCostChainFromTheLastStoredClaudeCost(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	var backfill string
	for _, m := range migrations {
		if i := strings.Index(m.sql, "UPDATE usage_record SET cost_total"); i >= 0 {
			backfill = m.sql[i:]
		}
	}
	if backfill == "" {
		t.Fatal("no migration carries the cost_total backfill")
	}
	// What the previous code stored: the running total as the turn's cost, and no cost_total.
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "old", "usage", "", usageFrame(map[string]any{"cost_estimate": 0.5})); err != nil {
		t.Fatal(err)
	}
	// An agent of another tool: its stored cost was its own, so it is not a total.
	if _, err := s.db.Exec(backfill); err != nil {
		t.Fatal(err)
	}
	if _, total := costOf(t, s, "old"); total.Valid {
		t.Fatalf("a codex agent's cost became a running total: %+v", total)
	}
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(backfill); err != nil {
		t.Fatal(err)
	}
	if cost, total := costOf(t, s, "old"); !total.Valid || total.Float64 != 0.5 || cost.Float64 != 0.5 {
		t.Fatalf("a Claude agent's stored cost: cost %+v total %+v", cost, total)
	}
	// The session's next turn reports the new running total; it is priced as the difference.
	next := nextTurn(t, s, f, "ses_test", 0)
	if err := s.RecordTurnEvent(ctx, next.ID, f.agent.ID, "new", "usage", "", usageFrame(map[string]any{"cost_estimate": 0.7, "cost_cumulative": true})); err != nil {
		t.Fatal(err)
	}
	if cost, total := costOf(t, s, "new"); !cost.Valid || math.Abs(cost.Float64-0.2) > 1e-9 || total.Float64 != 0.7 {
		t.Fatalf("the first turn after the upgrade: cost %+v total %+v, want 0.2 of 0.7", cost, total)
	}
	if _, err := s.db.Exec(backfill); err != nil { // idempotent: it never rewrites a total that is already remembered
		t.Fatal(err)
	}
	if _, total := costOf(t, s, "new"); total.Float64 != 0.7 {
		t.Fatalf("running the backfill again rewrote a remembered total: %+v", total)
	}
}
