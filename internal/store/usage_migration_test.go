package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindUsageTotalMigration rewinds the schema to before migration 124 the way a database that already ran it looks after the version
// rows are deleted: the total columns and the context-delivery table are dropped, so the migrations run again over them. Deleting the
// version rows from 124 also replays the room purpose migration (126), which adds a column, so that column is dropped too.
func rewindUsageTotalMigration(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE usage_record DROP COLUMN tokens_in_total`,
		`ALTER TABLE usage_record DROP COLUMN tokens_out_total`,
		`ALTER TABLE usage_record DROP COLUMN tokens_cache_read_total`,
		`ALTER TABLE usage_record DROP COLUMN tokens_cache_write_total`,
		`DROP TABLE cost_reset_marker`,
		`DROP TABLE session_context_delivery`,
		`ALTER TABLE room DROP COLUMN purpose`,
		`DELETE FROM schema_version WHERE version>=124`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func insertUsageRow(t *testing.T, s *Store, id, turnID, sessionID, agentID, roomID, projectID string, tokensIn, cacheRead int64, cost, total *float64, source, createdAt string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,session_id,agent_id,room_id,project_id,tokens_in,tokens_cache_read,cost_est,cost_total,model,partial,source,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0,?,?)`, id, id, nullableString(turnID), nullableString(sessionID), agentID, roomID, projectID,
		tokensIn, cacheRead, cost, total, "m", source, createdAt); err != nil {
		t.Fatal(err)
	}
}

// Migration 124 repairs the rows the old code stored the broken way: a resumed session's first report that recorded the session's whole
// lifetime cost as that turn's own, and a keep-alive ping that recorded the harness thread's running totals as its own usage. Rows the
// old code stored correctly stay as they are.
func TestMigrationRepairsLifetimeCostRowsAndTotalPings(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	floatPtr := func(value float64) *float64 { return &value }

	// A session resumed from a v1 legacy thread: its first total row recorded the whole lifetime as the turn's own cost.
	if _, err := s.db.Exec(`UPDATE session SET start_mode='native_resume',native_session_id='legacy-thread' WHERE id='ses_test'`); err != nil {
		t.Fatal(err)
	}
	insertUsageRow(t, s, "broken", f.turn.ID, "ses_test", f.agent.ID, f.room.ID, f.project.ID, 5, 4000, floatPtr(3012.88), floatPtr(3012.88), "measured", "2026-11-19T10:00:00.000000000Z")
	// The chain's next row has an earlier total before it, so it stays whatever it recorded.
	guarded := nextTurn(t, s, f, "ses_test", 0)
	insertUsageRow(t, s, "guarded", guarded.ID, "ses_test", f.agent.ID, f.room.ID, f.project.ID, 5, 4000, floatPtr(3015.0), floatPtr(3015.0), "measured", "2026-11-19T10:01:00.000000000Z")
	// A fresh session's equal cost and total is the turn's own spend and stays.
	fresh := nextTurn(t, s, f, "ses_fresh", 1)
	insertUsageRow(t, s, "own", fresh.ID, "ses_fresh", f.agent.ID, f.room.ID, f.project.ID, 5, 4000, floatPtr(0.5), floatPtr(0.5), "measured", "2026-11-19T10:02:00.000000000Z")
	// A Claude-style ping that recorded the whole running total as its own cost, the first total of its chain.
	nextTurn(t, s, f, "ses_ping", 2)
	if _, err := s.db.Exec(`UPDATE session SET start_mode='native_resume',native_session_id='ping-thread' WHERE id='ses_ping'`); err != nil {
		t.Fatal(err)
	}
	insertUsageRow(t, s, "ping-total", "", "ses_ping", f.agent.ID, f.room.ID, f.project.ID, 2, 90000, floatPtr(3.0), floatPtr(3.0), "keep_alive", "2026-11-19T10:03:00.000000000Z")
	// A Codex-style ping that recorded the thread's running totals as its own tokens, behind a real turn that recorded its own delta.
	codexTurn := nextTurn(t, s, f, "ses_codex", 3)
	insertUsageRow(t, s, "codex-turn", codexTurn.ID, "ses_codex", f.agent.ID, f.room.ID, f.project.ID, 100, 900, nil, nil, "measured", "2026-11-19T10:04:00.000000000Z")
	insertUsageRow(t, s, "ping-codex", "", "ses_codex", f.agent.ID, f.room.ID, f.project.ID, 50000, 13960000, floatPtr(3.50), nil, "keep_alive", "2026-11-19T10:05:00.000000000Z")
	// A ping whose figures are its own small delta stays.
	insertUsageRow(t, s, "ping-small", "", "ses_codex", f.agent.ID, f.room.ID, f.project.ID, 10, 50, floatPtr(0.001), nil, "keep_alive", "2026-11-19T10:06:00.000000000Z")
	// Replaying from 124 also replays the room purpose migration (126), which adds a column.
	if _, err := s.db.Exec(`ALTER TABLE room DROP COLUMN purpose`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version>=124`); err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(ctx, s.db, 0); err != nil {
		t.Fatal(err)
	}
	read := func(id string) (cost, total sql.NullFloat64, source string, in, cacheRead sql.NullInt64) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT cost_est,cost_total,source,tokens_in,tokens_cache_read FROM usage_record WHERE source_event_id=?`, id).
			Scan(&cost, &total, &source, &in, &cacheRead); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return
	}
	cost, total, source, _, _ := read("broken")
	if cost.Valid || !nearCost(total, 3012.88) || source != "estimated" {
		t.Fatalf("the lifetime row: cost %+v total %+v source %s, want no cost, the kept total, estimated", cost, total, source)
	}
	cost, total, source, _, _ = read("guarded")
	if !cost.Valid || cost.Float64 != 3015.0 || source != "measured" {
		t.Fatalf("a row with an earlier total in its chain was rewritten: cost %+v source %s", cost, source)
	}
	cost, total, source, _, _ = read("own")
	if !cost.Valid || cost.Float64 != 0.5 || source != "measured" {
		t.Fatalf("a fresh session's own spend was rewritten: cost %+v source %s", cost, source)
	}
	cost, total, source, in, cacheRead := read("ping-total")
	if cost.Valid || !nearCost(total, 3.0) || source != "keep_alive" || !in.Valid || in.Int64 != 2 || !cacheRead.Valid || cacheRead.Int64 != 90000 {
		t.Fatalf("the Claude-style ping: cost %+v total %+v source %s tokens %v/%v, want the cost cleared and the rest kept", cost, total, source, in, cacheRead)
	}
	cost, total, source, in, cacheRead = read("ping-codex")
	// The Codex-style ping repair is dropped (a heuristic that can erase valid warm-ping spend): the row keeps its figures until a
	// later report re-seeds the chain's baseline.
	if !cost.Valid || cost.Float64 != 3.50 || !in.Valid || in.Int64 != 50000 || !cacheRead.Valid || cacheRead.Int64 != 13960000 || total.Valid {
		t.Fatalf("the Codex-style ping row changed: cost %+v tokens %v/%v total %+v", cost, in, cacheRead, total)
	}
	cost, _, source, in, cacheRead = read("ping-small")
	if !cost.Valid || cost.Float64 != 0.001 || !in.Valid || in.Int64 != 10 {
		t.Fatalf("a ping's own small delta was cleared: cost %+v tokens %v", cost, in)
	}
}

// The migration replays cleanly over a database that already ran it: repaired rows do not match again, and the total columns it adds
// once are not added twice.
func TestUsageTotalMigrationReplays(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`UPDATE session SET start_mode='native_resume' WHERE id='ses_test'`); err != nil {
		t.Fatal(err)
	}
	insertUsageRow(t, s, "broken", f.turn.ID, "ses_test", f.agent.ID, f.room.ID, f.project.ID, 5, 4000, func() *float64 { v := 3012.88; return &v }(), func() *float64 { v := 3012.88; return &v }(), "measured", "2026-11-19T10:00:00.000000000Z")
	if err := migrateUpTo(context.Background(), s.db, 0); err != nil {
		t.Fatal(err)
	}
	rewindUsageTotalMigration(t, s)
	if err := migrateUpTo(context.Background(), s.db, 0); err != nil {
		t.Fatal(err)
	}
	var cost sql.NullFloat64
	var source string
	if err := s.db.QueryRow(`SELECT cost_est,source FROM usage_record WHERE source_event_id='broken'`).Scan(&cost, &source); err != nil {
		t.Fatal(err)
	}
	_, total := costOf(t, s, "broken")
	if cost.Valid || !nearCost(total, 3012.88) || source != "estimated" {
		t.Fatalf("the replayed migration changed its repair: cost %+v total %+v source %s", cost, total, source)
	}
}

// Migration 124 leaves the turn rows alone: the pre-fix cold Codex rows carry the identical stored shape a correct warm row
// carries (source estimated, no cost total, per-turn deltas for the warm ones and thread totals for the cold ones), so no query over
// the stored data can prove which rows were cold, and a repair keyed on growing figures would wipe correct warm rows. Warm turns and
// pings, a resumed session's first warm row, and the cold turn rows all survive; only the provably broken shapes are repaired.
func TestMigrationLeavesWarmAndColdTurnRowsIntact(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	// A warm chain: each row is a correct per-turn delta whose figures grow past the previous row's.
	warm := nextTurn(t, s, f, "ses_warm", 1)
	insertUsageRow(t, s, "warm-1", warm.ID, "ses_warm", f.agent.ID, f.room.ID, f.project.ID, 100, 0, func() *float64 { v := 0.010; return &v }(), nil, "estimated", "2026-11-19T10:00:00.000000000Z")
	warm2 := nextTurn(t, s, f, "ses_warm", 1)
	insertUsageRow(t, s, "warm-2", warm2.ID, "ses_warm", f.agent.ID, f.room.ID, f.project.ID, 150, 1100, func() *float64 { v := 0.012; return &v }(), nil, "estimated", "2026-11-19T10:01:00.000000000Z")
	warm3 := nextTurn(t, s, f, "ses_warm", 1)
	insertUsageRow(t, s, "warm-3", warm3.ID, "ses_warm", f.agent.ID, f.room.ID, f.project.ID, 250, 2200, func() *float64 { v := 0.014; return &v }(), nil, "estimated", "2026-11-19T10:02:00.000000000Z")
	// A warm ping behind them: its own delta never exceeds the real turn before it in tokens or cost.
	insertUsageRow(t, s, "warm-ping", "", "ses_warm", f.agent.ID, f.room.ID, f.project.ID, 150, 2300, func() *float64 { v := 0.002; return &v }(), nil, "keep_alive", "2026-11-19T10:03:00.000000000Z")
	// A resumed session's first warm row: its own deltas are correct (the warm path baselined them itself).
	resumed := nextTurn(t, s, f, "ses_resumed", 2)
	if _, err := s.db.Exec(`UPDATE session SET start_mode='native_resume',native_session_id='resumed-thread' WHERE id='ses_resumed'`); err != nil {
		t.Fatal(err)
	}
	insertUsageRow(t, s, "resumed-warm", resumed.ID, "ses_resumed", f.agent.ID, f.room.ID, f.project.ID, 300, 3300, func() *float64 { v := 0.015; return &v }(), nil, "estimated", "2026-11-19T10:04:00.000000000Z")
	// A pre-fix cold turn row keeps its figures too: the repair that would have cleared it is gone, because it could not be
	// told apart from a correct warm row.
	cold := nextTurn(t, s, f, "ses_cold", 3)
	insertUsageRow(t, s, "cold-turn", cold.ID, "ses_cold", f.agent.ID, f.room.ID, f.project.ID, 900000, 13960000, func() *float64 { v := 3.50; return &v }(), nil, "estimated", "2026-11-19T10:05:00.000000000Z")
	// The provably broken shapes are still repaired: a cold ping (totals priced over them, above every earlier row in both
	// tokens and cost) and a lifetime cost recorded as one turn's own.
	insertUsageRow(t, s, "cold-ping", "", "ses_cold", f.agent.ID, f.room.ID, f.project.ID, 1000000, 14000000, func() *float64 { v := 3.55; return &v }(), nil, "keep_alive", "2026-11-19T10:06:00.000000000Z")
	lifetime := nextTurn(t, s, f, "ses_lifetime", 4)
	if _, err := s.db.Exec(`UPDATE session SET start_mode='native_resume',native_session_id='lifetime-thread' WHERE id='ses_lifetime'`); err != nil {
		t.Fatal(err)
	}
	insertUsageRow(t, s, "lifetime-turn", lifetime.ID, "ses_lifetime", f.agent.ID, f.room.ID, f.project.ID, 5, 4000, func() *float64 { v := 3012.88; return &v }(), func() *float64 { v := 3012.88; return &v }(), "measured", "2026-11-19T10:07:00.000000000Z")
	// Replaying from 124 also replays the room purpose migration (126), which adds a column.
	if _, err := s.db.Exec(`ALTER TABLE room DROP COLUMN purpose`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version>=124`); err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(context.Background(), s.db, 0); err != nil {
		t.Fatal(err)
	}
	read := func(id string) (in, cacheRead sql.NullInt64, cost sql.NullFloat64, source string) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT tokens_in,tokens_cache_read,cost_est,source FROM usage_record WHERE source_event_id=?`, id).
			Scan(&in, &cacheRead, &cost, &source); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return
	}
	for _, id := range []string{"warm-1", "warm-2", "warm-3", "warm-ping", "resumed-warm", "cold-turn"} {
		in, cacheRead, cost, source := read(id)
		if !in.Valid || !cacheRead.Valid || !cost.Valid || (source != "estimated" && source != "keep_alive") {
			t.Fatalf("%s was cleared: tokens %v/%v cost %+v source %s", id, in, cacheRead, cost, source)
		}
	}
	in, cacheRead, cost, _ := read("cold-ping")
	// The ping repair is dropped (a heuristic that can erase valid warm-ping spend): the overstated cold ping stays until a later
	// report re-seeds the chain's baseline.
	if !cost.Valid || cost.Float64 != 3.55 || !in.Valid || in.Int64 != 1000000 || !cacheRead.Valid || cacheRead.Int64 != 14000000 {
		t.Fatalf("the cold ping row changed: tokens %v/%v cost %+v", in, cacheRead, cost)
	}
	in, _, cost, source := read("lifetime-turn")
	if cost.Valid || source != "estimated" {
		t.Fatalf("the lifetime row kept its cost: cost %+v source %s (its per-turn tokens stay)", cost, source)
	}
	_ = in
}
