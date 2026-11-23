package store

import "testing"

// rewindWorkerRecoveryMigration (also drops room.purpose and the login columns, which later migrations add) restores the pre-120 schema for tests that
// construct an older database from a current fixture before replaying upgrades.
func rewindWorkerRecoveryMigration(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE room DROP COLUMN purpose`,
		`ALTER TABLE turn DROP COLUMN login`,
		`ALTER TABLE provider_limit DROP COLUMN login`,
		`DROP INDEX inbox_lead_recovery_due`,
		`ALTER TABLE inbox_item DROP COLUMN lead_due_at`,
		`ALTER TABLE inbox_item DROP COLUMN recovery_lead_id`,
		`ALTER TABLE settings_project DROP COLUMN worker_recovery_to_lead`,
		`ALTER TABLE settings_project DROP COLUMN worker_recovery_escalate_minutes`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

// dropLoginColumns drops the columns migration 130 adds, for a test that replays it by deleting version rows.
func dropLoginColumns(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{`ALTER TABLE turn DROP COLUMN login`, `ALTER TABLE provider_limit DROP COLUMN login`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}
