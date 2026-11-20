package store

import (
	"context"
	"database/sql"
	"math"
	"testing"
)

// A Claude ping is recorded without a turn and costs the difference from the session's last total, and the total it leaves is what the next real turn
// subtracts from: the turn is not charged for the pings before it.
func TestKeepAliveUsageIsRecordedAndDoesNotChargeTheNextTurn(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	near := func(got sql.NullFloat64, want float64) bool { return got.Valid && math.Abs(got.Float64-want) < 1e-9 }
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "real-1", "usage", "", usageFrame(map[string]any{"cost_estimate": 1.0, "cost_cumulative": true, "tokens_in": 5})); err != nil {
		t.Fatal(err)
	}
	ping := KeepAliveUsage{AgentID: f.agent.ID, At: "2026-11-08T17:00:00Z", TokensIn: 2, TokensOut: 100, CacheRead: 90000, CacheWrite: 150, Cost: 1.045, CostIsTotal: true}
	own, err := s.RecordKeepAlive(ctx, ping)
	if err != nil {
		t.Fatal(err)
	}
	if own == nil || math.Abs(*own-0.045) > 1e-9 {
		t.Fatalf("the ping's own cost is returned for a card (the difference from the session's last total): %v", own)
	}
	var cost, total sql.NullFloat64
	var turnID sql.NullString
	var source, session string
	var read, write int64
	err = s.db.QueryRow(`SELECT turn_id,session_id,source,cost_est,cost_total,tokens_cache_read,tokens_cache_write FROM usage_record WHERE source='keep_alive'`).Scan(&turnID, &session, &source, &cost, &total, &read, &write)
	if err != nil {
		t.Fatal(err)
	}
	if turnID.Valid || session != "ses_test" || source != "keep_alive" || !near(cost, 0.045) || !near(total, 1.045) || read != 90000 || write != 150 {
		t.Fatalf("record = turn %v session %q source %q cost %v total %v read %d write %d", turnID, session, source, cost, total, read, write)
	}
	// the next real turn of the session starts from the ping's total
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "real-2", "usage", "", usageFrame(map[string]any{"cost_estimate": 1.145, "cost_cumulative": true, "tokens_in": 5})); err != nil {
		t.Fatal(err)
	}
	if next, _ := costOf(t, s, "real-2"); !near(next, 0.1) {
		t.Fatalf("the real turn is charged %v, want 0.1 (the ping's 0.045 is its own)", next)
	}
	var sum float64
	if err := s.db.QueryRow(`SELECT SUM(cost_est) FROM usage_record`).Scan(&sum); err != nil || math.Abs(sum-1.145) > 1e-9 {
		t.Fatalf("the spend adds up to the session's own total: %v %v", sum, err)
	}
	var turns int
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT turn_id) FROM usage_record`).Scan(&turns); err != nil || turns != 2 {
		t.Fatalf("a ping is not a turn: %d %v", turns, err)
	}
}

// A Codex ping reports its own cost (no running total), which is recorded as it is; a ping that spent nothing, one of an agent that does not exist
// and one of an agent with no session leave no record.
func TestKeepAliveUsageDirectCostAndNothingToRecord(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if _, err := s.RecordKeepAlive(ctx, KeepAliveUsage{AgentID: f.agent.ID, CacheRead: 5000, Cost: 0.002}); err != nil {
		t.Fatal(err)
	}
	var cost, total sql.NullFloat64
	// The direct cost is recorded as it is, and the row derives the session's running total (the chain had none, so the ping's own
	// cost starts it) for the next cumulative report to be priced against.
	if err := s.db.QueryRow(`SELECT cost_est,cost_total FROM usage_record WHERE source='keep_alive'`).Scan(&cost, &total); err != nil || !cost.Valid || math.Abs(cost.Float64-0.002) > 1e-9 || !total.Valid || total.Float64 != 0.002 {
		t.Fatalf("direct cost: %v %v %v", cost, total, err)
	}
	for name, usage := range map[string]KeepAliveUsage{"spent nothing": {AgentID: f.agent.ID}, "no such agent": {AgentID: "agt_nobody", CacheRead: 1}} {
		before := count(t, s)
		if _, err := s.RecordKeepAlive(ctx, usage); err != nil || count(t, s) != before {
			t.Fatalf("%s: %v rows %d -> %d", name, err, before, count(t, s))
		}
	}
	if _, err := s.db.Exec(`UPDATE session SET ended_at='now' WHERE agent_id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	before := count(t, s)
	if _, err := s.RecordKeepAlive(ctx, KeepAliveUsage{AgentID: f.agent.ID, CacheRead: 1}); err != nil || count(t, s) != before {
		t.Fatalf("no session: %v", err)
	}
}

func count(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_record`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The usage_record migration rebuilds usage_record so that a row may have no turn and the source keep_alive: existing rows come across unchanged, the indexes are
// back, and a turn-less row can be written.
func TestUsageRecordMigrationKeepsRowsAndAllowsPings(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "before", "usage", "", usageFrame(map[string]any{"cost_estimate": 0.5, "cost_cumulative": true, "tokens_in": 7, "tokens_cache_read": 11})); err != nil {
		t.Fatal(err)
	}
	var tokensIn, read int64
	var cost float64
	if err := s.db.QueryRow(`SELECT tokens_in,tokens_cache_read,cost_est FROM usage_record WHERE source_event_id='before'`).Scan(&tokensIn, &read, &cost); err != nil || tokensIn != 7 || read != 11 || cost != 0.5 {
		t.Fatalf("a row written under the new schema: %v %v %v %v", tokensIn, read, cost, err)
	}
	for _, index := range []string{"usage_record_agent_time", "usage_record_room_time", "usage_record_project_time", "usage_record_turn"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d %v", index, n, err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,source,created_at) VALUES ('u','s',NULL,?,?,?,'keep_alive','now')`, f.agent.ID, f.room.ID, f.project.ID); err != nil {
		t.Fatalf("a row with no turn and the source keep_alive: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,source,created_at) VALUES ('u2','s2',NULL,?,?,?,'bogus','now')`, f.agent.ID, f.room.ID, f.project.ID); err == nil {
		t.Fatal("the source check is still there")
	}
}
