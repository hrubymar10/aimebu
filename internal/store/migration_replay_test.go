package store

import "testing"

// rewindWorkerRecoveryMigration (also drops room.purpose, the login columns and message.outcome, which later migrations add) restores the pre-120 schema for tests that
// construct an older database from a current fixture before replaying upgrades.
func rewindWorkerRecoveryMigration(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE room DROP COLUMN purpose`,
		`ALTER TABLE turn DROP COLUMN login`,
		`ALTER TABLE provider_limit DROP COLUMN login`,
		`ALTER TABLE message DROP COLUMN outcome`,
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

// dropColumnsSince130 drops the columns migrations 130 (the login columns) and 131 (message.outcome) add, for a test that replays them by
// deleting version rows.
func dropColumnsSince130(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{`ALTER TABLE turn DROP COLUMN login`, `ALTER TABLE provider_limit DROP COLUMN login`, `ALTER TABLE message DROP COLUMN outcome`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}
