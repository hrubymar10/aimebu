package store

import "context"

// AgentPlacement is where an agent runs and which project it belongs to, whether or not it is a member of any room now: an agent that
// left its room, or was stopped there, still has what it wrote on its machine. sql.ErrNoRows for an agent that does not exist.
func (s *Store) AgentPlacement(ctx context.Context, agentID string) (machineID, projectID string, err error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", "", err
	}
	err = s.db.QueryRowContext(ctx, `SELECT machine_id,project_id FROM agent WHERE id=?`, agentID).Scan(&machineID, &projectID)
	return machineID, projectID, err
}
