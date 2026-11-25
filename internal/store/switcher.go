package store

import "context"

type SwitcherAgent struct {
	ID       string        `json:"id"`
	Slug     string        `json:"slug"`
	Name     string        `json:"name"`
	Role     string        `json:"role"`
	Room     *SwitcherRoom `json:"room"`
	Tool     string        `json:"tool"`
	Desired  string        `json:"desired"`
	Observed string        `json:"observed"`
	Running  bool          `json:"running"`
}

type SwitcherRoom struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SwitcherAgents lists agents whose tool login would change on the host.
func (s *Store) SwitcherAgents(ctx context.Context, machineID, tool string) ([]SwitcherAgent, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.slug,a.slug,ro.role_key,a.tool,a.desired,a.observed,
		(a.observed IN ('in_turn','interrupting') OR EXISTS(SELECT 1 FROM turn t WHERE t.agent_id=a.id AND t.exec IN ('pending','running'))),
		COALESCE(r.id,''),COALESCE(r.title,'')
		FROM agent a JOIN role ro ON ro.id=a.role_id
		LEFT JOIN room r ON r.id=a.home_room_id
		WHERE a.machine_id=? AND a.tool=?
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
		var room SwitcherRoom
		if err := rows.Scan(&a.ID, &a.Slug, &a.Name, &a.Role, &a.Tool, &a.Desired, &a.Observed, &a.Running, &room.ID, &room.Name); err != nil {
			return nil, err
		}
		if room.ID != "" {
			a.Room = &room
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
