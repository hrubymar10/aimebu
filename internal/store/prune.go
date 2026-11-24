package store

import (
	"context"
	"fmt"
	"time"
)

// PruneRuntime clears live agent execution state. V2 retains agent, session,
// turn, and usage history because usage limits and cost reports need it.
func (s *Store) PruneRuntime(ctx context.Context) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	cutoff := time.Now().UTC().Add(-2 * time.Minute).Format(timestampLayout)
	var remoteCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM machine WHERE is_host=0 AND approved=1 AND revoked_at IS NULL AND last_seen>=?`, cutoff).Scan(&remoteCount); err != nil {
		return err
	}
	if remoteCount != 0 {
		return fmt.Errorf("remote supervisor was active within the last two minutes; stop it and wait before pruning")
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE checkout SET writer_agent_id=NULL,writer_state='none' WHERE writer_agent_id IS NOT NULL`, nil},
		{`UPDATE agent SET desired='stopped',observed='stopped',desired_rev=desired_rev+1,recovery_block=0,key_hash='',stopped_at=?`, []any{now}},
		{`UPDATE session SET ended_at=? WHERE ended_at IS NULL`, []any{now}},
		{`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=COALESCE(ended_at,?) WHERE exec IN ('pending','running','lost')`, []any{now}},
		{`UPDATE turn SET settle='settled' WHERE settle='pending'`, nil},
		{`UPDATE note SET state='aborted' WHERE state IN ('queued','dispatched','in_turn','unknown')`, nil},
		{`UPDATE outbox SET state='superseded' WHERE state IN ('proposed','validated')`, nil},
		{`UPDATE inbox_item SET state='withdrawn',rev=rev+1 WHERE state='open' AND (agent_id IS NOT NULL OR turn_id IS NOT NULL)`, nil},
		{`UPDATE schedule SET enabled=0,rev=rev+1,updated_at=? WHERE target_agent_id IS NOT NULL AND enabled=1`, []any{now}},
		{`DELETE FROM request_key WHERE actor_kind='agent'`, nil},
		{`DELETE FROM scope_control_agent`, nil},
		{`DELETE FROM scope_control_failure`, nil},
		{`DELETE FROM agent_switch`, nil},
		{`DELETE FROM agent_boundary_grant`, nil},
		{`DELETE FROM boundary_pending`, nil},
		{`DELETE FROM provider_limit_retry`, nil},
		{`DELETE FROM provider_limit`, nil},
	}
	return s.Tx(ctx, func(tx *Tx) error {
		for _, statement := range statements {
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				return fmt.Errorf("prune runtime: %s: %w", statement.query, err)
			}
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id FROM project WHERE system=0`)
		if err != nil {
			return err
		}
		var projects []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			projects = append(projects, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range projects {
			if _, err := tx.AppendEvent(Event{Kind: "snapshot_required", ProjectID: id}); err != nil {
				return err
			}
		}
		return nil
	})
}
