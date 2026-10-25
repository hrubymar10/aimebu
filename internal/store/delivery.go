package store

import (
	"context"
	"database/sql"
)

type AgentRuntime struct {
	ID            string
	ProjectID     string
	MachineID     string
	Generation    int
	Desired       string
	DesiredRev    int64
	Observed      string
	RecoveryBlock bool
	Revoked       bool
}

func (s *Store) AgentRuntimeByID(ctx context.Context, id string) (AgentRuntime, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentRuntime{}, err
	}
	var agent AgentRuntime
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,project_id,machine_id,generation,desired,desired_rev,observed,recovery_block,revoked_at FROM agent WHERE id=?`, id).Scan(&agent.ID, &agent.ProjectID, &agent.MachineID, &agent.Generation, &agent.Desired, &agent.DesiredRev, &agent.Observed, &agent.RecoveryBlock, &revoked)
	agent.Revoked = revoked.Valid
	return agent, err
}

func (s *Store) AgentsForMachine(ctx context.Context, machineID string) ([]AgentRuntime, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,project_id,machine_id,generation,desired,desired_rev,observed,recovery_block,revoked_at FROM agent WHERE machine_id=? ORDER BY id`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []AgentRuntime{}
	for rows.Next() {
		var agent AgentRuntime
		var revoked sql.NullString
		if err := rows.Scan(&agent.ID, &agent.ProjectID, &agent.MachineID, &agent.Generation, &agent.Desired, &agent.DesiredRev, &agent.Observed, &agent.RecoveryBlock, &revoked); err != nil {
			return nil, err
		}
		agent.Revoked = revoked.Valid
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// PendingStopControlsForMachine includes stops that have not been applied by
// the machine, even when the server already considers the agent quiescent.
// A disconnected supervisor can still have a process running in that case.
func (s *Store) PendingStopControlsForMachine(ctx context.Context, machineID string) ([]AgentRuntime, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.project_id,a.machine_id,a.generation,a.desired,a.desired_rev,a.observed,a.recovery_block,a.revoked_at
		FROM agent a WHERE a.machine_id=? AND a.revoked_at IS NULL AND a.desired='stopped'
		AND (a.observed!='stopped' OR NOT EXISTS (
			SELECT 1 FROM event e WHERE e.agent_id=a.id AND e.kind='agent_control_applied'
			AND CAST(json_extract(e.payload_json,'$.desired_rev') AS INTEGER)=a.desired_rev))
		ORDER BY a.id`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	controls := []AgentRuntime{}
	for rows.Next() {
		var agent AgentRuntime
		var revoked sql.NullString
		if err := rows.Scan(&agent.ID, &agent.ProjectID, &agent.MachineID, &agent.Generation, &agent.Desired, &agent.DesiredRev, &agent.Observed, &agent.RecoveryBlock, &revoked); err != nil {
			return nil, err
		}
		controls = append(controls, agent)
	}
	return controls, rows.Err()
}

func (s *Store) TouchMachine(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE machine SET last_seen=? WHERE id=? AND revoked_at IS NULL`, timestamp(), id)
		return err
	})
}

func (s *Store) RecordMachineHello(ctx context.Context, id, version string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE machine SET last_seen=?,supervisor_version=? WHERE id=? AND revoked_at IS NULL`, timestamp(), version, id)
		return err
	})
}

// TurnBinding verifies journal evidence before an old generation may submit
// its final answer after a supervisor reconnect.
func (s *Store) TurnBinding(ctx context.Context, turnID string) (agentID string, generation int, err error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT agent_id,generation FROM turn WHERE id=?`, turnID).Scan(&agentID, &generation)
	return
}

func (s *Store) TurnMode(ctx context.Context, turnID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT mode FROM turn WHERE id=?`, turnID).Scan(&mode)
	return mode, err
}

func (s *Store) CorrectionTurn(ctx context.Context, parentID string) (Turn, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Turn{}, err
	}
	var turn Turn
	err := s.db.QueryRowContext(ctx, `SELECT id,agent_id,session_id,generation,room_id,parent_turn_id,attempt,mode,exec FROM turn WHERE parent_turn_id=? AND mode='correction'`, parentID).
		Scan(&turn.ID, &turn.AgentID, &turn.SessionID, &turn.Generation, &turn.RoomID, &turn.ParentTurnID, &turn.Attempt, &turn.Mode, &turn.Exec)
	return turn, err
}
