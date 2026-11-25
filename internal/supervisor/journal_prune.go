package supervisor

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"
)

// Journal retention (A14-5). A turn the server has acknowledged and whose answer (if any) has its terminal receipt owes nothing, yet its start
// spec (several KB) and its activity events stayed forever. PruneSettled drops those and keeps the thin turn_start row: HasStart, a replayed
// start, the answer and receipt tombstones and the exit columns all still read it, and no query that decides Owes/Debt/the hello inventory
// looks at the dropped data (they look at exec, exit_acked, answer and receipt only). Deleting the row instead would let a late replay of the
// delivery start the turn a second time.
const (
	// PruneGrace is how long a settled turn or command keeps everything, so a late replay still finds its data.
	PruneGrace = time.Hour
	// PruneInterval is how often the supervisor prunes while it runs (it also prunes once at start).
	PruneInterval = time.Hour
	// PruneBatchSize bounds the turns or commands one transaction handles, so the write lock is never held long.
	PruneBatchSize = 100
	// pruneBatchPause is the pause between two batches, so a turn's own writes get the connection.
	pruneBatchPause = 10 * time.Millisecond
	// VacuumMinFreeBytes and VacuumMinFreeRatio: the journal is rebuilt only when it has more free space than both.
	VacuumMinFreeBytes = 50 << 20
	VacuumMinFreeRatio = 0.25
)

// PruneResult counts what one prune removed.
type PruneResult struct {
	Turns, Events, Commands int
}

// settledTurnWhere selects the turns whose heavy data may go: exited and acknowledged for longer than the grace, with no answer still holding
// its candidate and every answer's turn receipted terminally (the exact complement of the OpenTurns and PendingAnswers conditions), and
// something left to drop.
const settledTurnWhere = `t.exec='exited' AND t.exit_acked=1 AND COALESCE(NULLIF(t.settled_at,''),t.started_at)<?
	AND NOT EXISTS(SELECT 1 FROM answer a WHERE a.turn_id=t.turn_id AND a.candidate IS NOT NULL)
	AND (NOT EXISTS(SELECT 1 FROM answer a WHERE a.turn_id=t.turn_id) OR EXISTS(SELECT 1 FROM receipt r WHERE r.turn_id=t.turn_id AND r.terminal=1))
	AND (length(t.spec_json)>0 OR EXISTS(SELECT 1 FROM journal_event e WHERE e.turn_id=t.turn_id))`

func cutoff(grace time.Duration) string {
	return time.Now().Add(-grace).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// PruneBatch prunes at most limit settled turns and limit settled commands in one short transaction each and says how many it handled.
func (j *Journal) PruneBatch(ctx context.Context, grace time.Duration, limit int) (PruneResult, error) {
	var result PruneResult
	turns, events, err := j.pruneTurns(ctx, cutoff(grace), limit)
	if err != nil {
		return result, err
	}
	result.Turns, result.Events = turns, events
	commands, err := j.pruneCommands(ctx, cutoff(grace), limit)
	result.Commands = commands
	return result, err
}

func (j *Journal) pruneTurns(ctx context.Context, before string, limit int) (turns, events int, err error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT t.turn_id FROM turn_start t WHERE `+settledTurnWhere+` ORDER BY t.started_at,t.turn_id LIMIT ?`, before, limit)
	if err != nil {
		return 0, 0, err
	}
	var ids []any
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ids) == 0 {
		return 0, 0, err
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM journal_event WHERE turn_id IN (`+inList(len(ids))+`)`, ids...)
	if err != nil {
		return 0, 0, err
	}
	count, err := deleted.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turn_start SET spec_json=x'' WHERE turn_id IN (`+inList(len(ids))+`)`, ids...); err != nil {
		return 0, 0, err
	}
	return len(ids), int(count), tx.Commit()
}

// pruneCommands empties the command line and the output tail of commands whose end the server acknowledged; the rows stay, so a replayed start
// or end is still recognised.
func (j *Journal) pruneCommands(ctx context.Context, before string, limit int) (int, error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT e.job_id FROM command_exit e JOIN command_start s ON s.job_id=e.job_id
		WHERE e.exit_acked=1 AND e.ended_at<? AND (e.tail<>'' OR s.cmd<>'') ORDER BY e.ended_at,e.job_id LIMIT ?`, before, limit)
	if err != nil {
		return 0, err
	}
	var ids []any
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE command_exit SET tail='' WHERE job_id IN (`+inList(len(ids))+`)`, ids...); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE command_start SET cmd='' WHERE job_id IN (`+inList(len(ids))+`)`, ids...); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit()
}

// PruneSettled prunes in batches until nothing settled is left, pausing between batches so the journal's writers are never starved.
func (j *Journal) PruneSettled(ctx context.Context, grace time.Duration) (PruneResult, error) {
	return j.pruneSettled(ctx, grace, PruneBatchSize)
}

func (j *Journal) pruneSettled(ctx context.Context, grace time.Duration, limit int) (PruneResult, error) {
	var total PruneResult
	for {
		batch, err := j.PruneBatch(ctx, grace, limit)
		total.Turns += batch.Turns
		total.Events += batch.Events
		total.Commands += batch.Commands
		if err != nil || (batch.Turns < limit && batch.Commands < limit) {
			return total, err
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(pruneBatchPause):
		}
	}
}

// ShouldVacuum says the free pages are worth a rebuild: more than VacuumMinFreeBytes and more than VacuumMinFreeRatio of the file.
func ShouldVacuum(freePages, pageCount, pageSize int64) bool {
	if pageCount <= 0 {
		return false
	}
	free := freePages * pageSize
	return free > VacuumMinFreeBytes && float64(freePages)/float64(pageCount) > VacuumMinFreeRatio
}

// VacuumIfWorthIt rebuilds the file when ShouldVacuum says so and the journal owes nothing (no running turn, no open command, no unacknowledged
// end, no pending answer, no warm process): VACUUM holds the write lock for the whole rebuild and needs the free space of a copy. It reports
// whether it ran. Without auto_vacuum a plain VACUUM is the only way to give pages back; an owing or busy journal just waits for the next tick.
func (j *Journal) VacuumIfWorthIt(ctx context.Context) (bool, error) {
	var free, count, size int64
	if err := j.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return false, err
	}
	if err := j.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&count); err != nil {
		return false, err
	}
	if err := j.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return false, err
	}
	if !ShouldVacuum(free, count, size) || j.leftOwed(ctx) {
		return false, nil
	}
	if _, err := j.db.ExecContext(ctx, `VACUUM`); err != nil {
		return false, err
	}
	var busy, logFrames, checkpointed sql.NullInt64
	_ = j.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
	return true, nil
}

// pruneJournal prunes once at start and then every PruneInterval until ctx ends; it logs what it removed and never fails the supervisor.
func (c *Client) pruneJournal(ctx context.Context) {
	for {
		if result, err := c.Journal.PruneSettled(ctx, PruneGrace); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("supervisor: journal prune failed: %v", err)
		} else if result != (PruneResult{}) {
			log.Printf("supervisor: journal pruned: %d settled turns (%d events), %d settled commands", result.Turns, result.Events, result.Commands)
		}
		if ran, err := c.Journal.VacuumIfWorthIt(ctx); err != nil && ctx.Err() == nil {
			log.Printf("supervisor: journal vacuum failed: %v", err)
		} else if ran {
			log.Printf("supervisor: journal vacuumed")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(PruneInterval):
		}
	}
}
