package store

import "context"

// MachineRunningTurnIDs names turns that may already be in the local
// supervisor when an owner requests an immediate replacement.
func (s *Store) MachineRunningTurnIDs(ctx context.Context, machineID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id FROM turn t JOIN agent a ON a.id=t.agent_id
		WHERE a.machine_id=? AND (t.exec='running' OR (t.exec='pending' AND EXISTS(
			SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested')))`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MachineActiveWork is the conservative server-side idle check for an older
// local supervisor that does not support retire_at_idle. The current
// supervisor reports its own process state instead.
func (s *Store) MachineActiveWork(ctx context.Context, machineID string) (bool, error) {
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM turn t JOIN agent a ON a.id=t.agent_id
		WHERE a.machine_id=? AND (t.exec='running' OR (t.exec='pending' AND EXISTS(
			SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested')))
		UNION ALL
		SELECT 1 FROM command_job WHERE machine_id=? AND state='running'
	)`, machineID, machineID).Scan(&active)
	return active, err
}

// MachineRunningCommand says whether a command outside a turn is running on the machine.
func (s *Store) MachineRunningCommand(ctx context.Context, machineID string) (bool, error) {
	var running bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM command_job WHERE machine_id=? AND state='running')`, machineID).Scan(&running)
	return running, err
}
