package store

import (
	"context"
	"database/sql"
	"math"
	"testing"
)

func nearCost(got sql.NullFloat64, want float64) bool {
	return got.Valid && math.Abs(got.Float64-want) < 1e-9
}

// A session resumed from history the store never measured (a v1 legacy session, or a report older than the totals) reports its whole
// lifetime spend on its first turn. The whole lifetime is never one turn's own cost: nothing is recorded, the row is marked estimated,
// and the reported total is remembered so the next turn is priced as the difference from it.
func TestResumedSessionFirstReportRecordsNoLifetimeCost(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE session SET start_mode='native_resume' WHERE id='ses_test'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "first", "usage", "", usageFrame(map[string]any{"cost_estimate": 3012.88, "cost_cumulative": true, "tokens_in": 5})); err != nil {
		t.Fatal(err)
	}
	cost, total := costOf(t, s, "first")
	if cost.Valid || !nearCost(total, 3012.88) {
		t.Fatalf("the first turn of a resumed session: cost %+v total %+v, want no cost and the remembered total", cost, total)
	}
	var source string
	if err := s.db.QueryRow(`SELECT source FROM usage_record WHERE source_event_id='first'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "estimated" {
		t.Fatalf("source = %q, want estimated", source)
	}
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "second", "usage", "", usageFrame(map[string]any{"cost_estimate": 3015.0, "cost_cumulative": true, "tokens_in": 5})); err != nil {
		t.Fatal(err)
	}
	if cost, _ := costOf(t, s, "second"); !nearCost(cost, 2.12) {
		t.Fatalf("the next turn is priced %+v from the remembered total, want 2.12", cost)
	}
}

// A fork works on a copy of another session, so its first report carries the original's whole spend: like a resume, it has no
// baseline of its own and records nothing rather than the lifetime.
func TestForkSessionFirstReportRecordsNoLifetimeCost(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE session SET fork_of='aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa' WHERE id='ses_test'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "forked", "usage", "", usageFrame(map[string]any{"cost_estimate": 585.56, "cost_cumulative": true})); err != nil {
		t.Fatal(err)
	}
	cost, total := costOf(t, s, "forked")
	if cost.Valid || !nearCost(total, 585.56) {
		t.Fatalf("the first turn of a fork: cost %+v total %+v, want no cost and the remembered total", cost, total)
	}
}

// A fresh session's first report is its own spend, measured as the harness said it.
func TestFreshSessionFirstReportIsItsOwnCost(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "fresh", "usage", "", usageFrame(map[string]any{"cost_estimate": 0.5, "cost_cumulative": true})); err != nil {
		t.Fatal(err)
	}
	cost, total := costOf(t, s, "fresh")
	if !nearCost(cost, 0.5) || !nearCost(total, 0.5) {
		t.Fatalf("a fresh session's first report: cost %+v total %+v, want 0.5 of 0.5", cost, total)
	}
}

// A cold Codex turn reports the thread's running token totals (earlier turns included) and a cost priced over them. The store turns
// the totals into the turn's own usage by subtracting the session's previous totals, remembers the new totals either way, and records
// nothing but an estimated mark for a resumed thread whose earlier totals it never measured.
func TestCodexSessionTotalsBecomeTheTurnsOwnTokens(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	// What the Codex adapter emits: the cached share already split out of the input, the totals marked cumulative.
	cumulative := func(in, out, cache any, cost float64) map[string]any {
		return map[string]any{"tokens_in": in, "tokens_out": out, "tokens_cache_read": cache,
			"cost_estimate": cost, "cost_cumulative": true, "tokens_cumulative": true, "cost_source": "estimated"}
	}
	row := func(id string) (in, out, read, totalIn sql.NullInt64, cost sql.NullFloat64, source string) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT tokens_in,tokens_out,tokens_cache_read,tokens_in_total,cost_est,source FROM usage_record WHERE source_event_id=?`, id).
			Scan(&in, &out, &read, &totalIn, &cost, &source); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return
	}
	// The thread's first turn: the totals are its own, and they seed the session's baseline.
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "cold-1", "usage", "", usageFrame(cumulative(300, 300, 900, 0.02))); err != nil {
		t.Fatal(err)
	}
	in, out, read, totalIn, cost, source := row("cold-1")
	if !in.Valid || in.Int64 != 300 || !out.Valid || out.Int64 != 300 || !read.Valid || read.Int64 != 900 || !totalIn.Valid || totalIn.Int64 != 300 {
		t.Fatalf("a fresh thread's totals are its own: in=%v out=%v read=%v totalIn=%v", in, out, read, totalIn)
	}
	if !nearCost(cost, 0.02) || source != "estimated" {
		t.Fatalf("a fresh thread's cost: %+v source %s", cost, source)
	}
	// The second turn of the same thread is a resume: the totals include the first turn, and only the difference is the turn's own.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "cold-2", "usage", "", usageFrame(cumulative(600, 400, 1100, 0.06))); err != nil {
		t.Fatal(err)
	}
	in, out, read, totalIn, cost, source = row("cold-2")
	if !in.Valid || in.Int64 != 300 || !out.Valid || out.Int64 != 100 || !read.Valid || read.Int64 != 200 || !totalIn.Valid || totalIn.Int64 != 600 {
		t.Fatalf("a resumed thread's own tokens: in=%v out=%v read=%v totalIn=%v", in, out, read, totalIn)
	}
	if !nearCost(cost, 0.04) || source != "estimated" {
		t.Fatalf("a resumed thread's own cost: %+v source %s, want 0.04 estimated", cost, source)
	}
	// A session resumed from a v1 legacy thread has no baseline: nothing is recorded, the row is estimated, and the totals seed
	// the baseline the next turn subtracts from.
	legacy := nextTurn(t, s, f, "ses_legacy", 1)
	if _, err := s.db.Exec(`UPDATE session SET start_mode='native_resume',native_session_id='v1-thread' WHERE id='ses_legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, legacy.ID, f.agent.ID, "cold-legacy", "usage", "", usageFrame(cumulative(40000, 4000, 13960000, 3.50))); err != nil {
		t.Fatal(err)
	}
	in, out, read, totalIn, cost, source = row("cold-legacy")
	if in.Valid || out.Valid || read.Valid || cost.Valid {
		t.Fatalf("a legacy resume records nothing of the lifetime: in=%v out=%v read=%v cost=%v", in, out, read, cost)
	}
	if !totalIn.Valid || totalIn.Int64 != 40000 || source != "estimated" {
		t.Fatalf("a legacy resume: totalIn=%v source %s, want the seeded totals and estimated", totalIn, source)
	}
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_legacy", 1).ID, f.agent.ID, "cold-legacy-2", "usage", "", usageFrame(cumulative(50000, 4100, 13970000, 3.52))); err != nil {
		t.Fatal(err)
	}
	in, out, read, totalIn, cost, _ = row("cold-legacy-2")
	if !in.Valid || in.Int64 != 10000 || !out.Valid || out.Int64 != 100 || !read.Valid || read.Int64 != 10000 || !nearCost(cost, 0.02) {
		t.Fatalf("the next turn of a legacy resume: in=%v out=%v read=%v cost=%v, want the difference", in, out, read, cost)
	}
	_ = totalIn
}

// A keep-alive ping that reports the thread's running totals is recorded as its own delta: the difference from the session's previous
// totals, with the new totals remembered for the next real turn.
func TestKeepAlivePingTotalsBecomeThePingsOwnTokens(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "real", "usage", "", usageFrame(map[string]any{
		"tokens_in": 100, "tokens_out": 100, "tokens_cache_read": 900, "cost_estimate": 1.0, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	own, err := s.RecordKeepAlive(ctx, KeepAliveUsage{AgentID: f.agent.ID, At: "2026-11-19T17:00:00Z",
		TokensIn: 150, TokensOut: 130, CacheRead: 940, Cost: 1.02, CostIsTotal: true, TokensAreTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if own == nil || math.Abs(*own-0.02) > 1e-9 {
		t.Fatalf("the ping's own cost is %+v, want 0.02", own)
	}
	var in, out, read, totalIn sql.NullInt64
	var cost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT tokens_in,tokens_out,tokens_cache_read,tokens_in_total,cost_est FROM usage_record WHERE source='keep_alive'`).
		Scan(&in, &out, &read, &totalIn, &cost); err != nil {
		t.Fatal(err)
	}
	if !in.Valid || in.Int64 != 50 || !out.Valid || out.Int64 != 30 || !read.Valid || read.Int64 != 40 {
		t.Fatalf("the ping's own tokens: in=%v out=%v read=%v, want the difference", in, out, read)
	}
	if !totalIn.Valid || totalIn.Int64 != 150 || !nearCost(cost, 0.02) {
		t.Fatalf("the ping row: totalIn=%v cost=%v, want the remembered totals and the own cost", totalIn, cost)
	}
	// The next real turn subtracts from the totals the ping remembered, so the ping is not charged to it.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "real-2", "usage", "", usageFrame(map[string]any{
		"tokens_in": 250, "tokens_out": 200, "tokens_cache_read": 1000, "cost_estimate": 1.10, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var turnIn sql.NullInt64
	var turnCost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT tokens_in,cost_est FROM usage_record WHERE source_event_id='real-2'`).Scan(&turnIn, &turnCost); err != nil {
		t.Fatal(err)
	}
	if !turnIn.Valid || turnIn.Int64 != 100 || !nearCost(turnCost, 0.08) {
		t.Fatalf("the next real turn: in=%v cost=%v, want 100 and 0.08", turnIn, turnCost)
	}
}

// A ping as the first post-upgrade report in a session whose only rows predate the fix: the pre-fix rows hold no totals to subtract
// from, so the ping's reported figures are the thread's lifetime, and neither tokens nor cost are recorded. The reported totals are
// kept as the baseline the next real turn is priced from.
func TestKeepAlivePingWithoutBaselineRecordsNothing(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	// The pre-fix shape: a cold Codex turn row with the thread's totals as its own tokens, priced over them, no cost total.
	if _, err := s.db.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,session_id,agent_id,room_id,project_id,tokens_in,tokens_cache_read,cost_est,cost_total,model,partial,source,created_at)
		VALUES ('use_prefix','prefix',?,?,?,?,?,?,?,3.40,NULL,'m',0,'estimated','2026-11-19T10:00:00.000000000Z')`,
		f.turn.ID, "ses_test", f.agent.ID, f.room.ID, f.project.ID, 50000, 13960000); err != nil {
		t.Fatal(err)
	}
	own, err := s.RecordKeepAlive(ctx, KeepAliveUsage{AgentID: f.agent.ID, At: "2026-11-19T17:00:00Z",
		TokensIn: 60000, CacheRead: 13970000, Cost: 3.50, CostIsTotal: true, TokensAreTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if own != nil {
		t.Fatalf("the ping's own cost is %+v, want none (no baseline)", own)
	}
	var in, read, totalIn sql.NullInt64
	var cost, total sql.NullFloat64
	if err := s.db.QueryRow(`SELECT tokens_in,tokens_cache_read,tokens_in_total,cost_est,cost_total FROM usage_record WHERE source='keep_alive'`).
		Scan(&in, &read, &totalIn, &cost, &total); err != nil {
		t.Fatal(err)
	}
	if in.Valid || read.Valid || cost.Valid {
		t.Fatalf("the ping recorded the lifetime: tokens %v/%v cost %+v", in, read, cost)
	}
	if !totalIn.Valid || totalIn.Int64 != 60000 || !total.Valid || total.Float64 != 3.50 {
		t.Fatalf("the reported totals are not kept as the baseline: tokensInTotal %v costTotal %+v", totalIn, total)
	}
	// The next real turn is priced from the seeded baseline, not from scratch.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "after-ping", "usage", "", usageFrame(map[string]any{
		"tokens_in": 61000, "tokens_cache_read": 13971000, "cost_estimate": 3.52, "cost_cumulative": true, "tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var turnIn sql.NullInt64
	var turnCost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT tokens_in,cost_est FROM usage_record WHERE source_event_id='after-ping'`).Scan(&turnIn, &turnCost); err != nil {
		t.Fatal(err)
	}
	if !turnIn.Valid || turnIn.Int64 != 1000 || !turnCost.Valid || math.Abs(turnCost.Float64-0.02) > 1e-9 {
		t.Fatalf("the next real turn: tokens %v cost %+v, want 1000 and 0.02", turnIn, turnCost)
	}
}

// A ping after warm turns: the warm rows remember the totals they left the session at, so the ping's delta is its own and the cold
// report after it is not charged for the warm turns or the ping.
func TestKeepAlivePingAfterWarmTurnsRecordsItsOwnDelta(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	// Cold turn A reports the thread's totals.
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "cold-A", "usage", "", usageFrame(map[string]any{
		"tokens_in": 1000, "tokens_out": 100, "tokens_cache_read": 900, "cost_estimate": 1.0, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	// Warm turns B and C report their own deltas.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "warm-B", "usage", "", usageFrame(map[string]any{
		"tokens_in": 100, "tokens_out": 20, "tokens_cache_read": 1000, "cost_estimate": 0.01, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "warm-C", "usage", "", usageFrame(map[string]any{
		"tokens_in": 150, "tokens_out": 30, "tokens_cache_read": 1100, "cost_estimate": 0.012, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	own, err := s.RecordKeepAlive(ctx, KeepAliveUsage{AgentID: f.agent.ID, At: "2026-11-19T17:00:00Z",
		TokensIn: 1300, TokensOut: 160, CacheRead: 1200, Cost: 1.024, CostIsTotal: true, TokensAreTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if own == nil || math.Abs(*own-0.002) > 1e-9 {
		t.Fatalf("the ping's own cost is %+v, want 0.002", own)
	}
	// Cold turn D reports the thread's totals; the delta is D alone.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "cold-D", "usage", "", usageFrame(map[string]any{
		"tokens_in": 1400, "tokens_out": 200, "tokens_cache_read": 1300, "cost_estimate": 1.042, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var dIn sql.NullInt64
	var dCost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT tokens_in,cost_est FROM usage_record WHERE source_event_id='cold-D'`).Scan(&dIn, &dCost); err != nil {
		t.Fatal(err)
	}
	if !dIn.Valid || dIn.Int64 != 100 || !dCost.Valid || math.Abs(dCost.Float64-0.018) > 1e-9 {
		t.Fatalf("cold D: tokens %v cost %+v, want 100 and 0.018 (the warm turns and the ping are not counted again)", dIn, dCost)
	}
}

// A fresh session whose first report carries tokens but no cost estimate (the Codex adapter skips the price estimate when the token
// shares are inconsistent) keeps its token totals but no cost total. The next priced cold report must not record the whole lifetime as
// its own cost: with no cost baseline but earlier rows in the chain, the baseline is unknown, so no cost is recorded and the reported
// total seeds the baseline the following turn is priced from.
func TestPricedReportAfterUnpriceableReportRecordsNoLifetimeCost(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	// The fresh chain's first report: tokens recorded (and their totals remembered), no cost.
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "unpriced", "usage", "", usageFrame(map[string]any{
		"tokens_in": 1000, "tokens_out": 100, "tokens_cache_read": 900, "cost_estimate": 1.0, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE usage_record SET cost_est=NULL WHERE source_event_id='unpriced'`); err != nil {
		t.Fatal(err)
	}
	var totalIn sql.NullInt64
	var total sql.NullFloat64
	// The pre-fix shape the finding describes: the Codex adapter priced nothing, so the row keeps its totals but no cost total.
	if _, err := s.db.Exec(`UPDATE usage_record SET cost_est=NULL,cost_total=NULL WHERE source_event_id='unpriced'`); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT tokens_in_total,cost_total FROM usage_record WHERE source_event_id='unpriced'`).Scan(&totalIn, &total); err != nil {
		t.Fatal(err)
	}
	if !totalIn.Valid || totalIn.Int64 != 1000 || total.Valid {
		t.Fatalf("the unpriced row: tokensInTotal %v costTotal %+v, want the totals kept and no cost total", totalIn, total)
	}
	// The next priced cold report covers the whole thread so far: the cost has no baseline, but no earlier row ever recorded a cost,
	// so the uncharged cumulative total is real spend and is recorded as the fresh baseline (F007).
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "priced", "usage", "", usageFrame(map[string]any{
		"tokens_in": 2000, "tokens_out": 300, "tokens_cache_read": 1800, "cost_estimate": 1.02, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var cost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT cost_est FROM usage_record WHERE source_event_id='priced'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if !cost.Valid || math.Abs(cost.Float64-1.02) > 1e-9 {
		t.Fatalf("the priced report recorded %+v, want the uncharged 1.02 as the fresh baseline", cost)
	}
	// The reported total is seeded, so the turn after is priced as the difference from it.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "priced-2", "usage", "", usageFrame(map[string]any{
		"tokens_in": 2100, "tokens_out": 320, "tokens_cache_read": 1900, "cost_estimate": 1.05, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT cost_est FROM usage_record WHERE source_event_id='priced-2'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if !cost.Valid || math.Abs(cost.Float64-0.03) > 1e-9 {
		t.Fatalf("the turn after: cost %+v, want 0.03 from the seeded baseline (1.05 less 1.02)", cost)
	}
}

// The first report after a Codex thread reset is the fresh thread's own spend: its totals started at zero. The reset marker tells the
// baseline checks to read the session as fresh even though the earlier rows remain, so the turn's spend is recorded, not dropped.
func TestFirstReportAfterThreadResetIsItsOwnSpend(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	// The thread carries measured history: a cumulative turn and its totals.
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='resumed-thread',native_session_started_at='2026-11-19T09:00:00.000000000Z' WHERE id='ses_test'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "before-reset", "usage", "", usageFrame(map[string]any{
		"tokens_in": 1000, "tokens_out": 100, "tokens_cache_read": 900, "cost_estimate": 1.0, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	// The reset: the totals are forgotten and the marker is written.
	if err := s.Tx(ctx, func(tx *Tx) error {
		return tx.recordNativeSessionReset(f.turn.ID, f.agent.ID, f.project.ID, f.room.ID, "codex")
	}); err != nil {
		t.Fatal(err)
	}
	var marker int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM cost_reset_marker WHERE session_id='ses_test'`).Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("the reset marker: %d rows, %v", marker, err)
	}
	var costReset string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.cost_reset') FROM event WHERE kind='native_session_reset' AND turn_id=?`, f.turn.ID).Scan(&costReset); err != nil || costReset != "1" {
		t.Fatalf("the reset event carries cost_reset=%q, %v", costReset, err)
	}
	// The next report: the fresh thread's totals include only itself, so the whole report is its own spend.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "after-reset", "usage", "", usageFrame(map[string]any{
		"tokens_in": 800, "tokens_out": 80, "tokens_cache_read": 700, "cost_estimate": 0.9, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var in sql.NullInt64
	var cost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT tokens_in,cost_est FROM usage_record WHERE source_event_id='after-reset'`).Scan(&in, &cost); err != nil {
		t.Fatal(err)
	}
	if !in.Valid || in.Int64 != 800 || !cost.Valid || math.Abs(cost.Float64-0.9) > 1e-9 {
		t.Fatalf("the first report after a reset: tokens %v cost %+v, want the whole report recorded as its own", in, cost)
	}
	// The marker is consumed: the next cumulative report is a resumed thread again, priced as the difference.
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM cost_reset_marker WHERE session_id='ses_test'`).Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("the reset marker was not consumed: %d rows, %v", marker, err)
	}
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "after-reset-2", "usage", "", usageFrame(map[string]any{
		"tokens_in": 900, "tokens_out": 100, "tokens_cache_read": 800, "cost_estimate": 0.92, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT tokens_in,cost_est FROM usage_record WHERE source_event_id='after-reset-2'`).Scan(&in, &cost); err != nil {
		t.Fatal(err)
	}
	if !in.Valid || in.Int64 != 100 || !cost.Valid || math.Abs(cost.Float64-0.02) > 1e-9 {
		t.Fatalf("the report after: tokens %v cost %+v, want the difference 100/0.02", in, cost)
	}
}

// A reset followed by a warm turn followed by a cold turn: the warm turn's own deltas seed the totals (the reset marker reads the
// session as fresh), the marker is consumed, and the cold turn's cumulative report is priced as the difference. Without the marker
// check the warm turn would be rejected as a baseline and the cold turn's spend would be lost.
func TestResetThenWarmThenColdSeedsAndPrices(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	// The thread carries measured history.
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='resumed-thread',native_session_started_at='2026-11-19T09:00:00.000000000Z' WHERE id='ses_test'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "before-reset", "usage", "", usageFrame(map[string]any{
		"tokens_in": 1000, "tokens_out": 100, "tokens_cache_read": 900, "cost_estimate": 1.0, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	// The reset: the totals are forgotten and the marker is written.
	if err := s.Tx(ctx, func(tx *Tx) error {
		return tx.recordNativeSessionReset(f.turn.ID, f.agent.ID, f.project.ID, f.room.ID, "codex")
	}); err != nil {
		t.Fatal(err)
	}
	// A warm turn on the fresh thread reports own deltas: they are the fresh totals, not a rejected baseline.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "warm-after-reset", "usage", "", usageFrame(map[string]any{
		"tokens_in": 200, "tokens_out": 40, "tokens_cache_read": 150, "cost_estimate": 0.02, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var warmIn sql.NullInt64
	var warmCost, warmTotalIn sql.NullFloat64
	if err := s.db.QueryRow(`SELECT tokens_in,cost_est,cost_total FROM usage_record WHERE source_event_id='warm-after-reset'`).Scan(&warmIn, &warmCost, &warmTotalIn); err != nil {
		t.Fatal(err)
	}
	if !warmIn.Valid || warmIn.Int64 != 200 || !warmCost.Valid || math.Abs(warmCost.Float64-0.02) > 1e-9 {
		t.Fatalf("the warm turn after a reset: tokens %v cost %+v, want the deltas recorded", warmIn, warmCost)
	}
	if !warmTotalIn.Valid || warmTotalIn.Float64 != 0.02 {
		t.Fatalf("the warm turn's derived cost total is %+v, want 0.02 (the fresh thread's own)", warmTotalIn)
	}
	var warmTotalTokens sql.NullInt64
	if err := s.db.QueryRow(`SELECT tokens_in_total FROM usage_record WHERE source_event_id='warm-after-reset'`).Scan(&warmTotalTokens); err != nil {
		t.Fatal(err)
	}
	if !warmTotalTokens.Valid || warmTotalTokens.Int64 != 200 {
		t.Fatalf("the warm turn's derived token total is %v, want 200", warmTotalTokens)
	}
	// The marker is consumed by the recorded report.
	var marker int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM cost_reset_marker WHERE session_id='ses_test'`).Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("the reset marker was not consumed: %d rows, %v", marker, err)
	}
	// The cold turn's cumulative report covers the fresh thread so far: priced as the difference from the warm turn's derived total.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "cold-after-warm", "usage", "", usageFrame(map[string]any{
		"tokens_in": 400, "tokens_out": 60, "tokens_cache_read": 350, "cost_estimate": 0.05, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var coldIn sql.NullInt64
	var coldCost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT tokens_in,cost_est FROM usage_record WHERE source_event_id='cold-after-warm'`).Scan(&coldIn, &coldCost); err != nil {
		t.Fatal(err)
	}
	if !coldIn.Valid || coldIn.Int64 != 200 || !coldCost.Valid || math.Abs(coldCost.Float64-0.03) > 1e-9 {
		t.Fatalf("the cold turn: tokens %v cost %+v, want the difference 200/0.03", coldIn, coldCost)
	}
}

// A chain where no row ever recorded a cost (a report the harness could not price, then nothing) leaves the next cumulative total
// uncharged spend: it is recorded as the fresh baseline rather than dropped, while a chain with a recorded cost keeps the unknown-baseline
// treatment.
func TestCostBaselineUnknownOnlyWhenACostWasRecorded(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	// Earlier rows exist, but none ever recorded a cost.
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "no-cost-row", "usage", "", usageFrame(map[string]any{
		"tokens_in": 500, "partial": true})); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "first-priced", "usage", "", usageFrame(map[string]any{
		"cost_estimate": 2.0, "cost_cumulative": true})); err != nil {
		t.Fatal(err)
	}
	var cost sql.NullFloat64
	if err := s.db.QueryRow(`SELECT cost_est FROM usage_record WHERE source_event_id='first-priced'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if !cost.Valid || math.Abs(cost.Float64-2.0) > 1e-9 {
		t.Fatalf("the first priced report over an unpriced chain recorded %+v, want the uncharged 2.0 as the fresh baseline", cost)
	}
	// A chain with a recorded cost (and no total) keeps the unknown-baseline treatment: the rows are wiped and the session is
	// re-seeded, then a priced row stores its cumulative cost without a total (the pre-fix loss shape).
	if _, err := s.db.Exec(`DELETE FROM usage_record WHERE agent_id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "priced-no-total", "usage", "", usageFrame(map[string]any{
		"tokens_in": 500, "cost_estimate": 1.0, "cost_cumulative": true, "tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE usage_record SET cost_total=NULL WHERE source_event_id='priced-no-total'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "priced-after-loss", "usage", "", usageFrame(map[string]any{
		"cost_estimate": 2.0, "cost_cumulative": true, "tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT cost_est FROM usage_record WHERE source_event_id='priced-after-loss'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost.Valid {
		t.Fatalf("the report over a chain with a recorded cost recorded %+v, want no cost (the earlier spend's total is unknown)", cost)
	}
}

// A token baseline is taken from the latest row with any non-null token total, and a category the baseline row never recorded is handled
// per category: the rest of the report's deltas are recorded, not dropped.
func TestTokenBaselineKeepsTheCategoriesTheLatestRowRecorded(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	// The first report stores totals for every category it reported.
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "base", "usage", "", usageFrame(map[string]any{
		"tokens_in": 1000, "tokens_out": 100, "tokens_cache_read": 900, "cost_estimate": 1.0, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	// The second report omits input (the adapter cannot split the cached share): its stored totals carry only the other categories.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "no-input", "usage", "", usageFrame(map[string]any{
		"tokens_out": 130, "tokens_cache_read": 950, "cost_estimate": 1.01, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var noInputOut sql.NullInt64
	if err := s.db.QueryRow(`SELECT tokens_out FROM usage_record WHERE source_event_id='no-input'`).Scan(&noInputOut); err != nil {
		t.Fatal(err)
	}
	if !noInputOut.Valid || noInputOut.Int64 != 30 {
		t.Fatalf("the input-less report's output delta is %v, want 30", noInputOut)
	}
	// The third report has input again: its delta is taken from the baseline it can find, and the other categories still subtract
	// from the remembered totals.
	if err := s.RecordTurnEvent(ctx, nextTurn(t, s, f, "ses_test", 0).ID, f.agent.ID, "input-again", "usage", "", usageFrame(map[string]any{
		"tokens_in": 1400, "tokens_out": 150, "tokens_cache_read": 1000, "cost_estimate": 1.03, "cost_cumulative": true,
		"tokens_cumulative": true, "cost_source": "estimated"})); err != nil {
		t.Fatal(err)
	}
	var in, out, read sql.NullInt64
	if err := s.db.QueryRow(`SELECT tokens_in,tokens_out,tokens_cache_read FROM usage_record WHERE source_event_id='input-again'`).Scan(&in, &out, &read); err != nil {
		t.Fatal(err)
	}
	if !out.Valid || out.Int64 != 20 || !read.Valid || read.Int64 != 50 {
		t.Fatalf("the report after an input-less one: out %v read %v, want 20/50 from the remembered totals", out, read)
	}
}
