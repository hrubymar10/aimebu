package store

import (
	"strings"
	"testing"
)

func TestEventBoundsPlanAndResult(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := t.Context()
	rows, err := s.readDB.QueryContext(ctx, "EXPLAIN QUERY PLAN "+eventBoundsSQL)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SCAN event") {
			t.Errorf("full scan: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	check := func(label string) {
		t.Helper()
		var wantMin, wantMax int64
		if err := s.db.QueryRow(`SELECT COALESCE(MIN(seq),0),COALESCE(MAX(seq),0) FROM event`).Scan(&wantMin, &wantMax); err != nil {
			t.Fatal(err)
		}
		min, max, err := s.EventBounds(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if min != wantMin || max != wantMax {
			t.Errorf("%s: got %d,%d want %d,%d", label, min, max, wantMin, wantMax)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM event`); err != nil {
		t.Fatal(err)
	}
	check("empty")
	if min, max, _ := s.EventBounds(ctx); min != 0 || max != 0 {
		t.Errorf("empty table: got %d,%d want 0,0", min, max)
	}
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<50)
		INSERT INTO event(id,seq,kind,payload_json,created_at) SELECT 'evt_b_'||i,100+i,'supervisor_activity','{}','2026-10-01T00:00:00.000000000Z' FROM n`); err != nil {
		t.Fatal(err)
	}
	check("filled")
	if min, max, _ := s.EventBounds(ctx); min != 101 || max != 150 {
		t.Errorf("filled table: got %d,%d want 101,150", min, max)
	}
}
