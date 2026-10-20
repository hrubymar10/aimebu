package store

import "context"

type SwitcherAgent struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Tool     string `json:"tool"`
	Desired  string `json:"desired"`
	Observed string `json:"observed"`
	Running  bool   `json:"running"`
}

// SwitcherAgents lists agents whose tool login would change on the host.
func (s *Store) SwitcherAgents(ctx context.Context, machineID, tool string) ([]SwitcherAgent, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.slug,a.tool,a.desired,a.observed,
		(a.observed IN ('in_turn','interrupting') OR EXISTS(SELECT 1 FROM turn t WHERE t.agent_id=a.id AND t.exec IN ('pending','running')))
		FROM agent a WHERE a.machine_id=? AND a.tool=?
		AND ((a.revoked_at IS NULL AND a.desired!='stopped') OR a.observed IN ('in_turn','interrupting')
		OR EXISTS(SELECT 1 FROM turn t WHERE t.agent_id=a.id AND t.exec IN ('pending','running')))
		ORDER BY a.slug,a.id`, machineID, tool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SwitcherAgent{}
	for rows.Next() {
		var a SwitcherAgent
		if err := rows.Scan(&a.ID, &a.Slug, &a.Tool, &a.Desired, &a.Observed, &a.Running); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
