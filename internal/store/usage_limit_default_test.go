package store

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-json"
	_ "modernc.org/sqlite"
)

func limitOwnerFixture(t *testing.T) (*Store, executionFixture) {
	t.Helper()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(context.Background(), func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_owner") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='running',started_at=? WHERE id=?`, timestamp(), f.turn.ID); err != nil {
		t.Fatal(err)
	}
	return s, f
}

func setLimit(t *testing.T, s *Store, project string, in UsageLimitInput, rev int64) (UsageLimit, error) {
	t.Helper()
	var out UsageLimit
	err := s.Tx(context.Background(), func(tx *Tx) error {
		var err error
		out, err = tx.UpdateUsageLimit(project, "usr_owner", in, rev)
		return err
	})
	return out, err
}

func TestProjectLimitsInheritTheGlobalDefaultUntilTheProjectSetsItsOwn(t *testing.T) {
	t.Parallel()
	s, f := limitOwnerFixture(t)
	ctx := context.Background()
	cost, turns := 10.0, int64(5)
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &cost, &turns); err != nil {
		t.Fatal(err)
	}
	// No row: everything is inherited.
	limit, err := s.UsageLimit(ctx, f.project.ID)
	if err != nil || limit.CostMode != "inherit" || limit.TurnMode != "inherit" || limit.DailyCostCap == nil || *limit.DailyCostCap != 10 ||
		limit.DailyTurnCap == nil || *limit.DailyTurnCap != 5 || limit.InheritedCostCap == nil || *limit.InheritedCostCap != 10 {
		t.Fatalf("inherited: %+v %v", limit, err)
	}
	// Cost custom, turns inherit.
	own := 3.0
	limit, err = setLimit(t, s, f.project.ID, UsageLimitInput{CostMode: "custom", Cost: &own, TurnMode: "inherit"}, 0)
	if err != nil || limit.CostMode != "custom" || *limit.DailyCostCap != 3 || limit.TurnMode != "inherit" || *limit.DailyTurnCap != 5 {
		t.Fatalf("custom cost: %+v %v", limit, err)
	}
	// Cost none, turns custom.
	custom := int64(2)
	limit, err = setLimit(t, s, f.project.ID, UsageLimitInput{CostMode: "none", TurnMode: "custom", Turns: &custom}, limit.Rev)
	if err != nil || limit.DailyCostCap != nil || *limit.DailyTurnCap != 2 || limit.InheritedCostCap == nil {
		t.Fatalf("none cost: %+v %v", limit, err)
	}
	// Changing the default moves the inheriting field only.
	newCost, newTurns := 99.0, int64(50)
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &newCost, &newTurns); err != nil {
		t.Fatal(err)
	}
	limit, _ = s.UsageLimit(ctx, f.project.ID)
	if limit.DailyCostCap != nil || *limit.DailyTurnCap != 2 || *limit.InheritedCostCap != 99 {
		t.Fatalf("a project's own settings moved with the default: %+v", limit)
	}
	limit, err = setLimit(t, s, f.project.ID, UsageLimitInput{CostMode: "inherit", TurnMode: "inherit"}, limit.Rev)
	if err != nil || *limit.DailyCostCap != 99 || *limit.DailyTurnCap != 50 {
		t.Fatalf("back to inheriting: %+v %v", limit, err)
	}
	// No default at all: inheriting means no limit.
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", nil, nil); err != nil {
		t.Fatal(err)
	}
	limit, _ = s.UsageLimit(ctx, f.project.ID)
	if limit.DailyCostCap != nil || limit.DailyTurnCap != nil || limit.InheritedCostCap != nil {
		t.Fatalf("no default: %+v", limit)
	}
	// Bad input is refused, and a stale revision too.
	neg := -1.0
	for _, in := range []UsageLimitInput{
		{CostMode: "custom", TurnMode: "none"},                       // custom without a value
		{CostMode: "none", Cost: &own, TurnMode: "none"},             // a value with none
		{CostMode: "custom", Cost: &neg, TurnMode: "none"},           // not positive
		{CostMode: "sometimes", TurnMode: "none"},                    // unknown mode
		{CostMode: "inherit", TurnMode: "custom", Turns: new(int64)}, // zero turns
		{CostMode: "inherit", Cost: &own, TurnMode: "inherit"},       // a value with inherit
	} {
		if _, err := setLimit(t, s, f.project.ID, in, limit.Rev); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%+v: %v", in, err)
		}
	}
	if _, err := setLimit(t, s, f.project.ID, UsageLimitInput{CostMode: "inherit", TurnMode: "inherit"}, 0); !errors.Is(err, ErrStaleRev) {
		t.Fatalf("stale: %v", err)
	}
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &neg, nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a negative default: %v", err)
	}
}

// Raising the default lifts the pause it caused, and lowering it takes effect at once, for the projects that inherit it.
func TestChangingTheGlobalDefaultReleasesAndAppliesPauses(t *testing.T) {
	t.Parallel()
	s, f := limitOwnerFixture(t)
	ctx := context.Background()
	five := 5.0
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &five, nil); err != nil {
		t.Fatal(err)
	}
	frame, _ := json.Marshal(map[string]any{"usage": map[string]any{"cost_estimate": 6.0}})
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "use_a", "usage", "", frame); err != nil {
		t.Fatal(err)
	}
	gate := func() string {
		var g string
		if err := s.db.QueryRow(`SELECT gate FROM project WHERE id=?`, f.project.ID).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	if gate() != "paused" {
		t.Fatalf("an inheriting project over the default was not paused: %q", gate())
	}
	twenty := 20.0
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &twenty, nil); err != nil {
		t.Fatal(err)
	}
	if gate() != "open" {
		t.Fatalf("raising the default did not release the pause: %q", gate())
	}
	if items, _ := s.InboxForUser(ctx, "usr_owner"); len(items) != 0 && items[0].Kind == "usage_limit" {
		t.Fatalf("the limit notice stayed open: %+v", items)
	}
	three := 3.0
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &three, nil); err != nil {
		t.Fatal(err)
	}
	if gate() != "paused" {
		t.Fatalf("lowering the default did not apply: %q", gate())
	}
	// A project with its own value is not touched by the default.
	own := 100.0
	limit, _ := s.UsageLimit(ctx, f.project.ID)
	if _, err := setLimit(t, s, f.project.ID, UsageLimitInput{CostMode: "custom", Cost: &own, TurnMode: "none"}, limit.Rev); err != nil {
		t.Fatal(err)
	}
	if gate() != "open" {
		t.Fatalf("a project's own higher cap did not release it: %q", gate())
	}
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &three, nil); err != nil {
		t.Fatal(err)
	}
	if gate() != "open" {
		t.Fatalf("the default moved a project that has its own value: %q", gate())
	}
}

func migrationByMarker(t *testing.T, marker string) int {
	t.Helper()
	for i, m := range migrations {
		if strings.Contains(m.sql, marker) {
			return i
		}
	}
	t.Fatalf("no migration contains %q", marker)
	return -1
}

// The migration keeps project limits' meaning (a value is custom, a blank was no limit), drops room limits, releases a room one of
// them had paused and resolves its notices.
func TestMigrationMakesLimitsPerProjectAndDropsRoomLimits(t *testing.T) {
	t.Parallel()
	db, index := prepareBeforeLimitsMigration(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO usage_limit(scope_kind,scope_id,daily_cost_cap,daily_turn_cap,rev) VALUES
		('project','prj_a',5.5,NULL,1),('project','prj_b',NULL,7,1),('project','prj_c',NULL,NULL,2),
		('room','room_x',1.0,NULL,1),('room','room_y',NULL,3,1)`)
	exec(`INSERT INTO usage_limit_day(scope_kind,scope_id,day_start,tz,paused) VALUES ('room','room_x','2030-01-01T00:00:00.000000000Z','UTC',1),('project','prj_a','2030-01-01T00:00:00.000000000Z','UTC',0)`)
	if _, err := db.Exec(migrations[index].sql); err != nil {
		t.Fatalf("the migration: %v", err)
	}
	type row struct {
		cost, turns string
	}
	got := map[string]row{}
	rows, err := db.Query(`SELECT scope_id,cost_mode,turn_mode FROM usage_limit WHERE scope_kind='project'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var r row
		if err := rows.Scan(&id, &r.cost, &r.turns); err != nil {
			t.Fatal(err)
		}
		got[id] = r
	}
	rows.Close()
	want := map[string]row{"prj_a": {"custom", "none"}, "prj_b": {"none", "custom"}, "prj_c": {"none", "none"}}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s: %+v, want %+v", id, got[id], w)
		}
	}
	var rooms, days int
	db.QueryRow(`SELECT COUNT(*) FROM usage_limit WHERE scope_kind='room'`).Scan(&rooms)
	db.QueryRow(`SELECT COUNT(*) FROM usage_limit_day WHERE scope_kind='room'`).Scan(&days)
	if rooms != 0 || days != 0 {
		t.Fatalf("room limits remain: %d limits, %d day states", rooms, days)
	}
	var projectDays int
	db.QueryRow(`SELECT COUNT(*) FROM usage_limit_day WHERE scope_kind='project'`).Scan(&projectDays)
	if projectDays != 1 {
		t.Fatalf("project day states were touched: %d", projectDays)
	}
}

// "Continue" raises today's cap for the project. A new default is a new cap, so the raise that shadowed the old one is cleared, and a
// project paused again after its Continue is released when the default goes above what it has spent.
func TestRaisingTheDefaultClearsTodaysContinueOverrides(t *testing.T) {
	t.Parallel()
	s, f := limitOwnerFixture(t)
	ctx := context.Background()
	five := 5.0
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &five, nil); err != nil {
		t.Fatal(err)
	}
	addCost := func(id string, amount float64) {
		t.Helper()
		frame, _ := json.Marshal(map[string]any{"usage": map[string]any{"cost_estimate": amount}})
		if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, id, "usage", "", frame); err != nil {
			t.Fatal(err)
		}
	}
	gate := func() string {
		var g string
		if err := s.db.QueryRow(`SELECT gate FROM project WHERE id=?`, f.project.ID).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	addCost("use_a", 6) // over 5: paused
	items, err := s.InboxForUser(ctx, "usr_owner")
	if err != nil || len(items) != 1 || items[0].Kind != "usage_limit" {
		t.Fatalf("limit item: %+v %v", items, err)
	}
	if _, _, err := s.DecideUsageLimit(ctx, items[0].ID, "usr_owner", "continue-1", "continue", int(items[0].Rev)); err != nil {
		t.Fatal(err)
	}
	limit, _ := s.UsageLimit(ctx, f.project.ID)
	if limit.DailyCostCap == nil || *limit.DailyCostCap != 12 || gate() != "open" {
		t.Fatalf("after Continue: cap=%v gate=%q", limit.DailyCostCap, gate())
	}
	addCost("use_b", 7) // 13: over the raised 12, paused again
	if gate() != "paused" {
		t.Fatalf("not paused again: %q", gate())
	}
	// The default goes to 20. The raise to 12 must not shadow it: the cap is 20 and 13 is under it.
	twenty := 20.0
	if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &twenty, nil); err != nil {
		t.Fatal(err)
	}
	limit, _ = s.UsageLimit(ctx, f.project.ID)
	if limit.DailyCostCap == nil || *limit.DailyCostCap != 20 {
		t.Fatalf("the Continue override still shadows the new default: %v", limit.DailyCostCap)
	}
	if gate() != "open" {
		t.Fatalf("raising the default did not release the project: %q", gate())
	}
}

// prepareBeforeLimitsMigration builds a database as it was before the per-project limits migration: every earlier migration applied and
// recorded, so that migrate runs only the one under test.
func prepareBeforeLimitsMigration(t *testing.T) (*sql.DB, int) {
	t.Helper()
	index := migrationByMarker(t, roomLimitsMarker)
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/m.sqlite?_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:index] {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if m.version == 54 {
			if err := ensureRoleExtendsColumn(context.Background(), tx); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(m.sql); err != nil {
			t.Fatalf("migration %d: %v", m.version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version,applied_at) VALUES (?,?)`, m.version, timestamp()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return db, index
}

func TestRoomLimitLinesDescribeEachDroppedLimitInFull(t *testing.T) {
	t.Parallel()
	db, _ := prepareBeforeLimitsMigration(t)
	if _, err := db.Exec(`INSERT INTO usage_limit(scope_kind,scope_id,daily_cost_cap,daily_turn_cap,rev) VALUES ('room','room_log_me',2.5,40,1),('room','room_log_too',NULL,9,1),('room','room_precise',0.125,NULL,1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	lines, err := roomLimitLines(context.Background(), tx, 60)
	tx.Rollback()
	want := []string{
		"migration 60: dropped the daily limit of room room_log_me (cost $2.5, turns 40 turns)",
		"migration 60: dropped the daily limit of room room_log_too (cost none, turns 9 turns)",
		"migration 60: dropped the daily limit of room room_precise (cost $0.125, turns none)",
	}
	if err != nil || strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines=%q err=%v", lines, err)
	}
}

// The log lines come out when the migration has committed: migrate writes them, with the count, and the limits are gone.
func TestMigrateLogsTheDroppedRoomLimitsOnceCommitted(t *testing.T) {
	db, _ := prepareBeforeLimitsMigration(t)
	if _, err := db.Exec(`INSERT INTO usage_limit(scope_kind,scope_id,daily_cost_cap,daily_turn_cap,rev) VALUES ('room','room_one',2.5,NULL,1),('room','room_two',NULL,9,1)`); err != nil {
		t.Fatal(err)
	}
	var buffer strings.Builder
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buffer.Write(p) }))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if err := migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	text := buffer.String()
	mu.Unlock()
	for _, want := range []string{"dropped the daily limit of room room_one (cost $2.5, turns none)", "dropped the daily limit of room room_two (cost none, turns 9 turns)", "2 room daily limits dropped"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the log lacks %q:\n%s", want, text)
		}
	}
	var left int
	db.QueryRow(`SELECT COUNT(*) FROM usage_limit WHERE scope_kind='room'`).Scan(&left)
	if left != 0 {
		t.Fatalf("room limits remain: %d", left)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Saving the default clears a Continue raise only for a field the project inherits and only when that field's default changed.
func TestDefaultChangesClearOnlyTheInheritedFieldThatChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spend := func(s *Store, f executionFixture, id string, amount float64) {
		t.Helper()
		frame, _ := json.Marshal(map[string]any{"usage": map[string]any{"cost_estimate": amount}})
		if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, id, "usage", "", frame); err != nil {
			t.Fatal(err)
		}
	}
	gate := func(s *Store, f executionFixture) string {
		var g string
		if err := s.db.QueryRow(`SELECT gate FROM project WHERE id=?`, f.project.ID).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	continueOnce := func(s *Store) {
		t.Helper()
		items, err := s.InboxForUser(ctx, "usr_owner")
		if err != nil || len(items) != 1 || items[0].Kind != "usage_limit" {
			t.Fatalf("limit item: %+v %v", items, err)
		}
		if _, _, err := s.DecideUsageLimit(ctx, items[0].ID, "usr_owner", "continue-"+items[0].ID, "continue", int(items[0].Rev)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a custom cost keeps its Continue when only the turns default changes", func(t *testing.T) {
		s, f := limitOwnerFixture(t)
		own := 5.0
		if _, err := setLimit(t, s, f.project.ID, UsageLimitInput{CostMode: "custom", Cost: &own, TurnMode: "inherit"}, 0); err != nil {
			t.Fatal(err)
		}
		spend(s, f, "use_a", 6)
		continueOnce(s)
		limit, _ := s.UsageLimit(ctx, f.project.ID)
		if *limit.DailyCostCap != 12 || gate(s, f) != "open" {
			t.Fatalf("after Continue: %+v gate=%q", limit.DailyCostCap, gate(s, f))
		}
		turns := int64(500)
		if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", nil, &turns); err != nil {
			t.Fatal(err)
		}
		limit, _ = s.UsageLimit(ctx, f.project.ID)
		if limit.DailyCostCap == nil || *limit.DailyCostCap != 12 || gate(s, f) != "open" {
			t.Fatalf("editing the turns default reset the project's own cost: cap=%v gate=%q", limit.DailyCostCap, gate(s, f))
		}
	})

	t.Run("an unchanged default clears nothing", func(t *testing.T) {
		s, f := limitOwnerFixture(t)
		five := 5.0
		if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &five, nil); err != nil {
			t.Fatal(err)
		}
		spend(s, f, "use_a", 6)
		continueOnce(s)
		if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &five, nil); err != nil {
			t.Fatal(err)
		}
		limit, _ := s.UsageLimit(ctx, f.project.ID)
		if *limit.DailyCostCap != 12 || gate(s, f) != "open" {
			t.Fatalf("saving the same default cleared the Continue: cap=%v gate=%q", *limit.DailyCostCap, gate(s, f))
		}
	})

	t.Run("an inherited cost keeps its Continue when only the turns default changes", func(t *testing.T) {
		s, f := limitOwnerFixture(t)
		five := 5.0
		if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &five, nil); err != nil {
			t.Fatal(err)
		}
		spend(s, f, "use_a", 6)
		continueOnce(s)
		turns := int64(500)
		if _, err := s.SetUsageLimitDefault(ctx, "usr_owner", &five, &turns); err != nil {
			t.Fatal(err)
		}
		limit, _ := s.UsageLimit(ctx, f.project.ID)
		if *limit.DailyCostCap != 12 || gate(s, f) != "open" {
			t.Fatalf("the unchanged cost default lost the Continue: cap=%v gate=%q", *limit.DailyCostCap, gate(s, f))
		}
	})
}
