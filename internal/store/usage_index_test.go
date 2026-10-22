package store

import (
	"strings"
	"testing"
)

// Every read of usage_record, and the turn count beside it, must be an index search: with no index each limit check, agent card and
// profile read the whole table (pi adds a row per model call), and the history only grows. A plan step "SCAN <table>" is that read.
func TestUsageQueriesNeverScanATable(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	queries := map[string]string{
		"room turns":    usageLimitTurnsSQL("t.room_id=?"),
		"project turns": usageLimitTurnsSQL("r.project_id=?"),
		"room cost":     usageLimitCostSQL("room_id=?"),
		"project cost":  usageLimitCostSQL("project_id=?"),
		"latest usage":  latestUsageSQL,
		"agent cost":    agentCostSQL,
		"agent profile": agentTodaySQL,
	}
	for name, query := range queries {
		args := make([]any, strings.Count(query, "?"))
		for i := range args {
			args[i] = "x"
		}
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN ") {
				t.Errorf("%s reads a whole table: %q\nplan: %q", name, step, plan)
			}
		}
		if name == "latest usage" {
			for _, step := range plan {
				if strings.Contains(step, "TEMP B-TREE") {
					t.Errorf("latest usage sorts the agent's whole history: %q", plan)
				}
			}
		}
	}
}
