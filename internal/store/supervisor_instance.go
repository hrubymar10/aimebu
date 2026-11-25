package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"time"
)

// InstanceQuietFor is how long an instance must have been silent before a person may write it off: a hello within it proves the instance runs.
const InstanceQuietFor = time.Minute

// ErrInstanceRecentlySeen means a write-off was refused because the instance said hello within the quiet period.
var ErrInstanceRecentlySeen = errors.New("the supervisor instance was seen recently, so it is not lost")

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

// InstanceGone is what marking an instance gone did: the open turns and command jobs it ended, and whether the instance was already gone
// (then nothing was ended and nothing was recorded).
type InstanceGone struct {
	Turns, Jobs int
	AlreadyGone bool
}

// MarkSupervisorInstanceGone records that an instance is finished for good (its journal is gone or replaced) and loses what it still holds:
// every open turn it owns becomes lost, with the recovery path and the supervisor_journal_changed event a journal change always had, and every
// open command job ends (failed when it never started, lost otherwise). Other instances of the machine are untouched. It returns the number
// of turns and jobs it ended. Marking an instance already gone is a no-op.
func (s *Store) MarkSupervisorInstanceGone(ctx context.Context, machineID, journalID string) (turns, jobs int, err error) {
	result, err := s.MarkSupervisorInstanceGoneBy(ctx, machineID, journalID, "", 0)
	return result.Turns, result.Jobs, err
}

// MarkSupervisorInstanceGoneBy is MarkSupervisorInstanceGone for a person's decision: when actorID is not empty the write-off is recorded
// as a supervisor_instance_gone event naming who did it and what it ended, in the same transaction. A second call finds the instance gone,
// ends nothing and records nothing (AlreadyGone). With quiet above zero the instance is written off only when its last hello is older than
// that, decided in the same transaction as the write-off (a hello updates last_seen_at in its own, and SQLite serializes the two): a live
// instance that connected after the caller's own check is refused with ErrInstanceRecentlySeen and loses nothing.
func (s *Store) MarkSupervisorInstanceGoneBy(ctx context.Context, machineID, journalID, actorID string, quiet time.Duration) (out InstanceGone, err error) {
	err = s.Tx(ctx, func(tx *Tx) error {
		out = InstanceGone{}
		cutoff := "9999-12-31T23:59:59.999999999Z"
		if quiet > 0 {
			cutoff = time.Now().UTC().Add(-quiet).Format(timestampLayout)
		}
		result, err := tx.Exec(`UPDATE supervisor_instance SET state='gone',gone_at=? WHERE machine_id=? AND journal_id=? AND state<>'gone' AND last_seen_at<?`, timestamp(), machineID, journalID, cutoff)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			// Unknown to the table (it predates it, or never said hello): its work is still lost, but only once.
			var state string
			known := true
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT state FROM supervisor_instance WHERE machine_id=? AND journal_id=?`, machineID, journalID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
				known = false
			} else if err != nil {
				return err
			}
			if known && state == "gone" {
				out.AlreadyGone = true
				return nil
			}
			if known {
				return ErrInstanceRecentlySeen
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
			out.Turns++
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
			out.Jobs++
		}
		if actorID == "" {
			return nil
		}
		_, err = tx.AppendEvent(Event{Kind: "supervisor_instance_gone", PayloadJSON: payloadJSON(map[string]string{"machine_id": machineID, "journal_id": journalID, "actor_id": actorID, "turns": strconv.Itoa(out.Turns), "jobs": strconv.Itoa(out.Jobs)})})
		return err
	})
	return out, err
}

// InstanceLoad is what an instance still owns: its open turns and command jobs, and the agents they belong to. It is what marking the instance
// gone would lose.
type InstanceLoad struct {
	Turns, Commands int
	Agents          []InstanceAgent
}

// InstanceAgent names an agent that owns work on an instance.
type InstanceAgent struct{ ID, Slug string }

// SupervisorInstanceLoad counts the open turns and command jobs an instance owns, by the same rule MarkSupervisorInstanceGone uses to end them,
// with the agents they belong to (each once, in slug order).
func (s *Store) SupervisorInstanceLoad(ctx context.Context, machineID, journalID string) (InstanceLoad, error) {
	var load InstanceLoad
	agents := map[string]string{}
	turns, err := s.db.QueryContext(ctx, `SELECT a.id,a.slug FROM turn t JOIN agent a ON a.id=t.agent_id
		WHERE a.machine_id=? AND t.journal_id=? AND t.exec IN ('pending','running')
			AND EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested')`, machineID, journalID)
	if err != nil {
		return load, err
	}
	for turns.Next() {
		var id, slug string
		if err := turns.Scan(&id, &slug); err != nil {
			turns.Close()
			return load, err
		}
		agents[id] = slug
		load.Turns++
	}
	err = turns.Err()
	turns.Close()
	if err != nil {
		return load, err
	}
	jobs, err := s.db.QueryContext(ctx, `SELECT a.id,a.slug FROM command_job j JOIN agent a ON a.id=j.agent_id
		WHERE j.machine_id=? AND j.journal_id=? AND j.state IN ('queued','running')`, machineID, journalID)
	if err != nil {
		return load, err
	}
	for jobs.Next() {
		var id, slug string
		if err := jobs.Scan(&id, &slug); err != nil {
			jobs.Close()
			return load, err
		}
		agents[id] = slug
		load.Commands++
	}
	err = jobs.Err()
	jobs.Close()
	if err != nil {
		return load, err
	}
	for id, slug := range agents {
		load.Agents = append(load.Agents, InstanceAgent{ID: id, Slug: slug})
	}
	sort.Slice(load.Agents, func(i, j int) bool {
		if load.Agents[i].Slug != load.Agents[j].Slug {
			return load.Agents[i].Slug < load.Agents[j].Slug
		}
		return load.Agents[i].ID < load.Agents[j].ID
	})
	return load, nil
}
