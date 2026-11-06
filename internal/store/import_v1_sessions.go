package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"

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

// importV1AgentRoles keeps the role v1 gave each agent in each room, so that starting the agent in v2 can suggest the same role. A room's own
// record has them only while the agent is registered: v1 vacates an agent's roles when it deregisters, and a backup is taken after the agents
// left, so the roles are also rebuilt from the system messages v1 wrote in the room. A role the record still has wins.
func importV1AgentRoles(ctx context.Context, tx *sql.Tx, data *v1ImportData) error {
	derived := v1RolesFromSystemMessages(data.Messages)
	for _, room := range data.Rooms {
		roles := map[string]string{}
		for agentID, role := range derived[room.ID] {
			roles[agentID] = role
		}
		for agentID, role := range room.Roles {
			if agentID != "" && role != "" {
				roles[agentID] = role
			}
		}
		for agentID, role := range roles {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_agent_role(agent_id,v1_room_id,role_key) VALUES (?,?,?)`, agentID, room.ID, role); err != nil {
				return err
			}
		}
	}
	return nil
}

// v1RolesFromSystemMessages replays v1's role system messages of every room, in the order they were written, and returns the role each agent
// holds at the end, by room and agent. v1 wrote "<agent> was assigned as <role>" when a role was assigned (a role's label is its key in v1), and
// "<agent> role cleared" when a person took it away, which counts. It wrote `role "<role>" vacated: <agent> deregistered` when the agent left,
// which is not a decision about the role: it is ignored, so an agent that deregistered still carries the role it last had.
func v1RolesFromSystemMessages(messages []v1ImportMessage) map[string]map[string]string {
	ordered := append([]v1ImportMessage(nil), messages...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	out := map[string]map[string]string{}
	for _, message := range ordered {
		if !isV1SystemAuthor(message) || message.RoomID == "" {
			continue
		}
		body := strings.TrimSpace(message.Body)
		if agent, role, ok := strings.Cut(body, " was assigned as "); ok && agent != "" && role != "" && !strings.ContainsAny(agent, " \t\n") {
			if out[message.RoomID] == nil {
				out[message.RoomID] = map[string]string{}
			}
			out[message.RoomID][agent] = strings.TrimSpace(role)
		} else if agent, ok := strings.CutSuffix(body, " role cleared"); ok && agent != "" && !strings.ContainsAny(agent, " \t\n") {
			delete(out[message.RoomID], agent)
		}
	}
	return out
}

// v1AgentSessionHistory is one row of v1's agent_session_history: a harness session an agent name ran in. v1 keeps only the latest in
// agent_sessions; the history (and the session each message was sent from) came with a later v1 build, so older backups have neither.
type v1AgentSessionHistory struct {
	FullID           string
	HarnessSessionID string
	Harness          string
	Model            string
	Project          string
	CWD              string
	Origin           string
	RegisteredAt     string
	LastSeen         string
}

// loadV1AgentSessionHistory reads agent_session_history. A backup without the table has none, and a table of another shape is ignored: like
// the sessions, it is a convenience, never a reason to refuse the import.
func loadV1AgentSessionHistory(ctx context.Context, db *sql.DB) ([]v1AgentSessionHistory, error) {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='agent_session_history'`).Scan(&present); err != nil || present == 0 {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT full_id,harness_session_id,COALESCE(harness,''),COALESCE(model,''),COALESCE(project,''),COALESCE(cwd,''),COALESCE(origin,''),COALESCE(registered_at,''),COALESCE(last_seen,'') FROM agent_session_history ORDER BY id`)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var history []v1AgentSessionHistory
	for rows.Next() {
		var row v1AgentSessionHistory
		if err := rows.Scan(&row.FullID, &row.HarnessSessionID, &row.Harness, &row.Model, &row.Project, &row.CWD, &row.Origin, &row.RegisteredAt, &row.LastSeen); err != nil {
			return nil, nil
		}
		if row.FullID != "" && row.HarnessSessionID != "" {
			history = append(history, row)
		}
	}
	return history, rows.Err()
}

// importV1AgentSessionHistory keeps each session under its v1 agent id, like the latest-session rows.
func importV1AgentSessionHistory(ctx context.Context, tx *sql.Tx, data *v1ImportData, report *V1ImportReport) error {
	for _, row := range data.AgentSessionHistory {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_agent_session_history(agent_id,harness_session_id,harness,model,project,cwd,origin,registered_at,last_seen) VALUES (?,?,?,?,?,?,?,?,?)`,
			row.FullID, row.HarnessSessionID, row.Harness, row.Model, row.Project, row.CWD, row.Origin, row.RegisteredAt, row.LastSeen)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			report.Imported["agent_session_history"]++
		} else {
			report.AlreadyPresent["agent_session_history"]++
		}
	}
	return nil
}
