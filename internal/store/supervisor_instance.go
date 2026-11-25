package store

import (
	"context"
	"database/sql"
	"errors"
)

// SupervisorInstance is one supervisor process of a machine, named by the journal it owns. A machine can run several at once while an older
// build finishes the work it holds and a newer one takes new work. State is active (takes new work), retiring (finishes what it holds) or
// gone (its journal is finished or lost).
type SupervisorInstance struct {
	MachineID, JournalID, Build, Environment, State string
	FirstSeenAt, LastSeenAt, GoneAt                 string
}

// RecordSupervisorInstance notes that an instance said hello: it is created active, or refreshed (and made active again when it was
// retiring or gone, as a hello is proof it runs) with the build and environment it reported.
func (s *Store) RecordSupervisorInstance(ctx context.Context, machineID, journalID, build, environment string) error {
	if machineID == "" || journalID == "" {
		return errors.New("missing supervisor instance identity")
	}
	now := timestamp()
	_, err := s.db.ExecContext(ctx, `INSERT INTO supervisor_instance(machine_id,journal_id,build,environment,state,first_seen_at,last_seen_at)
		VALUES (?,?,?,?,'active',?,?)
		ON CONFLICT(machine_id,journal_id) DO UPDATE SET build=excluded.build,environment=excluded.environment,state='active',
			last_seen_at=excluded.last_seen_at,gone_at=NULL`, machineID, journalID, build, environment, now, now)
	return err
}

// SetSupervisorInstanceState moves an instance between active and retiring. Marking one gone loses its work and is MarkSupervisorInstanceGone.
func (s *Store) SetSupervisorInstanceState(ctx context.Context, machineID, journalID, state string) error {
	if state != "active" && state != "retiring" {
		return errors.New("supervisor instance state must be active or retiring")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE supervisor_instance SET state=? WHERE machine_id=? AND journal_id=? AND state<>'gone'`, state, machineID, journalID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SupervisorInstances lists a machine's instances, oldest first.
func (s *Store) SupervisorInstances(ctx context.Context, machineID string) ([]SupervisorInstance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT machine_id,journal_id,build,environment,state,first_seen_at,last_seen_at,COALESCE(gone_at,'')
		FROM supervisor_instance WHERE machine_id=? ORDER BY first_seen_at,journal_id`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SupervisorInstance
	for rows.Next() {
		var item SupervisorInstance
		if err := rows.Scan(&item.MachineID, &item.JournalID, &item.Build, &item.Environment, &item.State, &item.FirstSeenAt, &item.LastSeenAt, &item.GoneAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// MachineJournalID is the journal the machine last accepted in a hello, "" when none.
func (s *Store) MachineJournalID(ctx context.Context, machineID string) (string, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT journal_id FROM machine WHERE id=?`, machineID).Scan(&id)
	return id.String, err
}

// MarkSupervisorInstanceGone records that an instance is finished for good (its journal is gone or replaced) and loses what it still holds:
// every open turn it owns becomes lost, with the recovery path and the supervisor_journal_changed event a journal change always had, and every
// open command job ends (failed when it never started, lost otherwise). Other instances of the machine are untouched. It returns the number
// of turns and jobs it ended. Marking an instance already gone is a no-op.
func (s *Store) MarkSupervisorInstanceGone(ctx context.Context, machineID, journalID string) (turns, jobs int, err error) {
	err = s.Tx(ctx, func(tx *Tx) error {
		turns, jobs = 0, 0
		result, err := tx.Exec(`UPDATE supervisor_instance SET state='gone',gone_at=? WHERE machine_id=? AND journal_id=? AND state<>'gone'`, timestamp(), machineID, journalID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			// Unknown to the table (it predates it, or never said hello): its work is still lost, but only once.
			var known bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM supervisor_instance WHERE machine_id=? AND journal_id=?)`, machineID, journalID).Scan(&known); err != nil {
				return err
			}
			if known {
				return nil
			}
			now := timestamp()
			if _, err := tx.Exec(`INSERT INTO supervisor_instance(machine_id,journal_id,state,first_seen_at,last_seen_at,gone_at) VALUES (?,?,'gone',?,?,?)`,
				machineID, journalID, now, now, now); err != nil {
				return err
			}
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT t.id,t.agent_id,t.room_id,t.exec,t.mode,a.project_id,a.observed,EXISTS(SELECT 1 FROM receipt r WHERE r.turn_id=t.id)
			FROM turn t JOIN agent a ON a.id=t.agent_id
			WHERE a.machine_id=? AND t.journal_id=? AND t.exec IN ('pending','running')
				AND EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested')`, machineID, journalID)
		if err != nil {
			return err
		}
		var items []openTurnItem
		for rows.Next() {
			item := openTurnItem{owner: journalID}
			if err := rows.Scan(&item.id, &item.agentID, &item.roomID, &item.exec, &item.mode, &item.projectID, &item.observed, &item.hasReceipt); err != nil {
				rows.Close()
				return err
			}
			items = append(items, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := tx.loseOpenTurn(item, machineID, true); err != nil {
				return err
			}
			turns++
		}
		jobRows, err := tx.tx.QueryContext(tx.ctx, `SELECT `+commandColumns+` FROM command_job WHERE machine_id=? AND journal_id=? AND state IN ('queued','running')`, machineID, journalID)
		if err != nil {
			return err
		}
		var open []CommandJob
		for jobRows.Next() {
			job, err := scanCommand(jobRows)
			if err != nil {
				jobRows.Close()
				return err
			}
			open = append(open, job)
		}
		err = jobRows.Err()
		jobRows.Close()
		if err != nil {
			return err
		}
		for _, job := range open {
			result := CommandResult{State: "lost", Error: "the machine's supervisor no longer knows this command, so its end will never be reported"}
			if job.State == "queued" {
				result = CommandResult{State: "failed", Error: commandNeverStarted}
			}
			if _, err := finishCommand(tx, job.ID, result); err != nil {
				return err
			}
			jobs++
		}
		return nil
	})
	return turns, jobs, err
}
