package store

import "testing"

// dropPoolProvenance undoes migration 143's tables and columns (the inbox_item rebuild replays as it is), for a test that replays it.
func dropPoolProvenance(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{`DROP TABLE pool_respawn`, `ALTER TABLE agent DROP COLUMN pool_origin`, `ALTER TABLE agent DROP COLUMN pool_launcher_id`,
		`ALTER TABLE agent DROP COLUMN pool_model`, `ALTER TABLE agent DROP COLUMN pool_kept_sig`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

// dropSupervisorInstances undoes migration 140 (the instance table, the owner columns and their indexes), for a test that replays it.
func dropSupervisorInstances(t *testing.T, s *Store) {
	t.Helper()
	dropPoolProvenance(t, s)
	for _, statement := range []string{`DROP INDEX turn_open_journal`, `DROP INDEX command_job_open_journal`,
		`ALTER TABLE turn DROP COLUMN journal_id`, `ALTER TABLE command_job DROP COLUMN journal_id`, `DROP TABLE supervisor_instance`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

// rewindWorkerRecoveryMigration (also drops room.purpose, the login columns, message.outcome and turn.handoff_outcome_required, which later migrations add) restores the pre-120 schema for tests that
// construct an older database from a current fixture before replaying upgrades.
func rewindWorkerRecoveryMigration(t *testing.T, s *Store) {
	t.Helper()
	dropSupervisorInstances(t, s)
	for _, statement := range []string{
		`ALTER TABLE room DROP COLUMN purpose`,
		`ALTER TABLE turn DROP COLUMN login`,
		`ALTER TABLE provider_limit DROP COLUMN login`,
		`ALTER TABLE message DROP COLUMN outcome`,
		`ALTER TABLE turn DROP COLUMN handoff_outcome_required`,
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

// dropColumnsSince130 drops the columns migrations 130 (the login columns), 131 (message.outcome) and 132 (turn.handoff_outcome_required) add, for a test that replays them by
// deleting version rows.
func dropColumnsSince130(t *testing.T, s *Store) {
	t.Helper()
	dropSupervisorInstances(t, s)
	for _, statement := range []string{`ALTER TABLE turn DROP COLUMN login`, `ALTER TABLE provider_limit DROP COLUMN login`, `ALTER TABLE message DROP COLUMN outcome`, `ALTER TABLE turn DROP COLUMN handoff_outcome_required`, `ALTER TABLE agent DROP COLUMN approved_by`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}
