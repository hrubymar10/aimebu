package store

import "testing"

// rewindWorkerRecoveryMigration restores the pre-120 schema for tests that
// construct an older database from a current fixture before replaying upgrades.
func rewindWorkerRecoveryMigration(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
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
