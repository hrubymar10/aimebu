package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// A room's or project's totals sum what its turns spent, each turn once, by range; a figure nobody reported stays unknown, and the cost
// coverage says whether every turn had a cost.
func TestUsageTotalsSumEachTurnOnceByRange(t *testing.T) {
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
	// Turn one reports twice (as pi does, once per model call): its figures are summed, and it is one turn.
	record(f.turn, "a1", map[string]any{"tokens_in": 10, "tokens_out": 2, "tokens_cache_read": 100, "cost_estimate": 0.01, "model": "m1"})
	record(f.turn, "a2", map[string]any{"tokens_in": 5, "tokens_out": 1, "cost_estimate": 0.005, "model": "m1"})
	// Turn two has no cost (Codex) and used another model.
	two := nextTurn(t, s, f, "ses_test", 0)
	record(two, "b1", map[string]any{"tokens_in": 7, "tokens_out": 3, "model": "m2", "partial": true})
	// Turn three is old: ten days ago.
	three := nextTurn(t, s, f, "ses_test", 0)
	record(three, "c1", map[string]any{"tokens_in": 1000, "tokens_out": 1000, "cost_estimate": 1.0, "model": "m1"})
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(timestampLayout)
	if _, err := s.db.Exec(`UPDATE usage_record SET created_at=? WHERE source_event_id='c1'`, old); err != nil {
		t.Fatal(err)
	}

	near := func(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-9 }
	for _, scope := range []struct{ kind, id string }{{"room", f.room.ID}, {"project", f.project.ID}} {
		week, err := s.UsageTotalsFor(ctx, scope.kind, scope.id, "7d")
		if err != nil {
			t.Fatal(err)
		}
		if week.Turns != 2 || week.TurnsWithCost != 1 || week.CostCoverage != "partial" || !week.Partial {
			t.Fatalf("%s 7d: %+v", scope.kind, week)
		}
		if week.TokensIn == nil || *week.TokensIn != 22 || week.TokensOut == nil || *week.TokensOut != 6 || week.CacheRead == nil || *week.CacheRead != 100 || !near(week.Cost, 0.015) || week.CacheWrite != nil {
			t.Fatalf("%s 7d figures: in=%v out=%v read=%v cost=%v write=%v", scope.kind, week.TokensIn, week.TokensOut, week.CacheRead, week.Cost, week.CacheWrite)
		}
		all, err := s.UsageTotalsFor(ctx, scope.kind, scope.id, "all")
		if err != nil || all.Turns != 3 || all.TurnsWithCost != 2 || !near(all.Cost, 1.015) || all.Since != "" {
			t.Fatalf("%s all: %+v %v", scope.kind, all, err)
		}
		today, err := s.UsageTotalsFor(ctx, scope.kind, scope.id, "today")
		if err != nil || today.Turns != 2 {
			t.Fatalf("%s today: %+v %v", scope.kind, today, err)
		}
	}
	// By role and by model: the dearest first, a group with no cost after, turns counted per group.
	week, _ := s.UsageTotalsFor(ctx, "room", f.room.ID, "7d")
	if len(week.ByRole) != 1 || week.ByRole[0].Label != "worker" || week.ByRole[0].Agents != 1 || week.ByRole[0].Turns != 2 {
		t.Fatalf("by role: %+v", week.ByRole)
	}
	if len(week.ByModel) != 2 || week.ByModel[0].Key != "m1" || week.ByModel[0].Turns != 1 || !near(week.ByModel[0].Cost, 0.015) || week.ByModel[1].Key != "m2" || week.ByModel[1].Cost != nil {
		t.Fatalf("by model: %+v", week.ByModel)
	}
	// Nothing recorded in a scope: zero turns, unknown cost, empty lists (never null).
	none, err := s.UsageTotalsFor(ctx, "room", "room_nothing", "all")
	if err != nil || none.Turns != 0 || none.CostCoverage != "unknown" || none.ByRole == nil || none.ByModel == nil || none.Cost != nil || none.TokensIn != nil {
		t.Fatalf("empty: %+v %v", none, err)
	}
	if _, err := s.UsageTotalsFor(ctx, "room", f.room.ID, "year"); err == nil {
		t.Fatal("an unknown range was accepted")
	}
	if _, err := s.UsageTotalsFor(ctx, "agent", f.room.ID, "all"); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
}

func TestUsageTotalsGroupsAgentsByCurrentRole(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "role-first", "usage", "", usageFrame(map[string]any{"tokens_in": 5, "tokens_cache_read": 15})); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
		 SELECT 'agt_second','second',project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at FROM agent WHERE id='` + f.agent.ID + `'`,
		`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_second','agt_second',0,'fresh_with_history','now')`,
		`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec) VALUES ('turn_second','agt_second','ses_second',0,'` + f.room.ID + `',1,'normal','exited')`,
		`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,tokens_in,tokens_out,source,partial,created_at)
		 VALUES ('role-second','role-second','turn_second','agt_second','` + f.room.ID + `','` + f.project.ID + `',7,2,'measured',0,'` + timestamp() + `')`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := s.UsageTotalsFor(ctx, "room", f.room.ID, "all")
	if err != nil || len(usage.ByRole) != 1 || usage.ByRole[0].Agents != 2 || usage.ByRole[0].Turns != 2 || usage.ByRole[0].CacheRead == nil || *usage.ByRole[0].CacheRead != 15 {
		t.Fatalf("role totals: %+v %v", usage.ByRole, err)
	}
	if _, err := s.db.Exec(`INSERT INTO role(id,key,label,body,role_key) VALUES ('role_researcher_test','field-researcher-test','Researcher','body','field-researcher-test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET role_id='role_researcher_test' WHERE id='agt_second'`); err != nil {
		t.Fatal(err)
	}
	usage, err = s.UsageTotalsFor(ctx, "room", f.room.ID, "all")
	if err != nil || len(usage.ByRole) != 2 || usage.ByRole[0].Agents != 1 || usage.ByRole[1].Agents != 1 {
		t.Fatalf("current roles: %+v %v", usage.ByRole, err)
	}
}

// The totals read by scope and time through the indexes, never by scanning the table.
func TestUsageTotalsQueriesNeverScan(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	for _, column := range []string{"room_id", "project_id"} {
		for name, query := range map[string]string{
			"sums":     `SELECT COUNT(DISTINCT u.turn_id),SUM(u.tokens_in),SUM(u.cost_est) FROM usage_record u WHERE u.` + column + `=? AND u.created_at>=?`,
			"by role":  `SELECT COALESCE(NULLIF(r.role_key,''),'no role'),COUNT(DISTINCT u.turn_id),SUM(u.cost_est) FROM usage_record u LEFT JOIN agent a ON a.id=u.agent_id LEFT JOIN role r ON r.id=a.role_id WHERE u.` + column + `=? AND u.created_at>=? GROUP BY COALESCE(NULLIF(r.role_key,''),'no role')`,
			"by model": `SELECT COALESCE(u.model,''),COUNT(DISTINCT u.turn_id),SUM(u.cost_est) FROM usage_record u WHERE u.` + column + `=? AND u.created_at>=? GROUP BY COALESCE(u.model,'')`,
		} {
			rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, "x", "y")
			if err != nil {
				t.Fatalf("%s %s: %v", column, name, err)
			}
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				if len(detail) >= 4 && detail[:4] == "SCAN" && detail != "SCAN a" {
					t.Errorf("%s %s: %s", column, name, detail)
				}
			}
			rows.Close()
		}
	}
}
