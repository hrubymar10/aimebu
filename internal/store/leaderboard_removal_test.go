package store

import "testing"

// Migration 56 removes what the leaderboards left in a database that had them, leaves everything else alone, and does nothing on a
// database that never had them (running it twice is the same as running it once).
func TestLeaderboardRemovalMigration(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	var sql string
	for _, step := range migrations {
		if step.version == 56 {
			sql = step.sql
		}
	}
	if sql == "" {
		t.Fatal("no migration 56")
	}
	for _, statement := range []string{
		`CREATE TABLE leaderboard_card (id INTEGER PRIMARY KEY AUTOINCREMENT, subject_model TEXT NOT NULL)`,
		`CREATE INDEX leaderboard_card_created ON leaderboard_card(id)`,
		`CREATE TABLE leaderboard_submission (key_hash TEXT PRIMARY KEY)`,
		`INSERT INTO leaderboard_card(subject_model) VALUES ('m')`,
		`INSERT INTO global_setting(key,value,updated_at) VALUES ('leaderboard_enabled','0','2026-01-01T00:00:00Z'),('keep_me','1','2026-01-01T00:00:00Z')`,
		`INSERT INTO request_key(actor_kind,actor_id,kind,key,request_digest,result_json,created_at) VALUES
			('agent','agt_x','leaderboard_start','k','d','{}','2026-01-01T00:00:00Z'),('agent','agt_x','spawn','k2','d','{}','2026-01-01T00:00:00Z')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for run := 1; run <= 2; run++ {
		if _, err := s.db.Exec(sql); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		var tables, settings, kept, startKeys, otherKeys int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'leaderboard%'`).Scan(&tables); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM global_setting WHERE key='leaderboard_enabled'`).Scan(&settings); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM global_setting WHERE key='keep_me'`).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_key WHERE kind='leaderboard_start'`).Scan(&startKeys); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_key WHERE kind='spawn'`).Scan(&otherKeys); err != nil {
			t.Fatal(err)
		}
		if tables != 0 || settings != 0 || kept != 1 || startKeys != 0 || otherKeys != 1 {
			t.Fatalf("run %d: tables=%d setting=%d kept=%d startKeys=%d otherKeys=%d", run, tables, settings, kept, startKeys, otherKeys)
		}
	}
}
