package store

import (
	"context"
	"database/sql"

	"github.com/goccy/go-json"
)

// v1AgentSession is one row of v1's agent_sessions: where an agent ran and its harness session. v2 has no live agent for an imported sender,
// so these are kept for its read-only profile.
type v1AgentSession struct {
	FullID           string `json:"full_id"`
	Harness          string `json:"harness"`
	Model            string `json:"model"`
	Project          string `json:"project"`
	CWD              string `json:"cwd"`
	HarnessSessionID string `json:"harness_session_id"`
	RegisteredAt     string `json:"registered_at"`
	LastSeen         string `json:"last_seen"`
}

// loadV1AgentSessions reads agent_sessions. A backup without the table, or with a table of another shape, has none (the sessions are a
// convenience, never a reason to refuse the import); a row that is not valid JSON is skipped and costs that one profile. The agent id
// comes from the row's own JSON, so the name of the key column does not matter.
func loadV1AgentSessions(ctx context.Context, db *sql.DB) ([]v1AgentSession, error) {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='agent_sessions'`).Scan(&present); err != nil || present == 0 {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT data FROM agent_sessions`)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var sessions []v1AgentSession
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, nil
		}
		var session v1AgentSession
		if json.Unmarshal([]byte(raw), &session) != nil || session.FullID == "" {
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// importV1AgentSessions keeps each session under its v1 agent id, which is also the author id of that agent's imported messages.
func importV1AgentSessions(ctx context.Context, tx *sql.Tx, data *v1ImportData, report *V1ImportReport) error {
	for _, session := range data.AgentSessions {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES (?,?,?,?,?,?,?,?)`,
			session.FullID, session.Harness, session.Model, session.Project, session.CWD, session.HarnessSessionID, session.RegisteredAt, session.LastSeen)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			report.Imported["agent_sessions"]++
		} else {
			report.AlreadyPresent["agent_sessions"]++
		}
	}
	return nil
}

// importV1AgentRoles keeps the role v1 gave each agent in each room, so that starting the agent in v2 can suggest the same role.
func importV1AgentRoles(ctx context.Context, tx *sql.Tx, data *v1ImportData) error {
	for _, room := range data.Rooms {
		for agentID, role := range room.Roles {
			if agentID == "" || role == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_agent_role(agent_id,v1_room_id,role_key) VALUES (?,?,?)`, agentID, room.ID, role); err != nil {
				return err
			}
		}
	}
	return nil
}
