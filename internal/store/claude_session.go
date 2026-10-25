package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrSessionAttached is a Claude Code session that a live agent already continues (or that another request attached first).
var ErrSessionAttached = errors.New("session already continued by an agent")

// ResumeSession is a Claude Code session an agent is started from: its first turn continues it (claude --resume) in the folder it was made in.
type ResumeSession struct {
	ID    string
	CWD   string
	Title string
}

// SessionAgent is the agent that continues a session.
type SessionAgent struct {
	AgentID string `json:"agent_id"`
	Slug    string `json:"slug"`
}

// ClaudeSessionAgents says, for each of the given session ids on a machine, which live agent continues it: one started from it
// (imported_session) or one whose own Claude session it is (an agent's session id is created by the server and Claude saves it).
func (s *Store) ClaudeSessionAgents(ctx context.Context, machineID string, ids []string) (map[string]SessionAgent, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	out := map[string]SessionAgent{}
	if len(ids) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := []any{machineID}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, machineID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT i.session_id,a.id,a.slug FROM imported_session i JOIN agent a ON a.id=i.agent_id
		WHERE i.machine_id=? AND i.session_id IN (%s) AND a.revoked_at IS NULL
		UNION SELECT se.native_session_id,a.id,a.slug FROM session se JOIN agent a ON a.id=se.agent_id
		WHERE a.machine_id=? AND a.tool='claude' AND a.revoked_at IS NULL AND se.native_session_id IN (%s)`, marks, marks), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var agent SessionAgent
		if err := rows.Scan(&id, &agent.AgentID, &agent.Slug); err != nil {
			return nil, err
		}
		out[id] = agent
	}
	return out, rows.Err()
}

// attachClaudeSession records that the agent continues the session, refusing a session a live agent already holds. A row left by an agent
// that was revoked since is replaced: that agent no longer touches the transcript.
func (tx *Tx) attachClaudeSession(in SpawnInput, agentID string) error {
	session := in.ResumeSession
	var held int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT
		(SELECT COUNT(*) FROM imported_session i JOIN agent a ON a.id=i.agent_id WHERE i.machine_id=? AND i.session_id=? AND a.revoked_at IS NULL)
		+ (SELECT COUNT(*) FROM session se JOIN agent a ON a.id=se.agent_id WHERE a.machine_id=? AND a.tool='claude' AND a.revoked_at IS NULL
			AND se.native_session_id=? AND a.id<>?)`, in.MachineID, session.ID, in.MachineID, session.ID, agentID).Scan(&held); err != nil {
		return err
	}
	if held > 0 {
		return ErrSessionAttached
	}
	if _, err := tx.Exec(`DELETE FROM imported_session WHERE machine_id=? AND session_id=?`, in.MachineID, session.ID); err != nil {
		return err
	}
	actor := in.ActorID
	_, err := tx.Exec(`INSERT INTO imported_session(machine_id,session_id,agent_id,cwd,title,imported_by,imported_at) VALUES (?,?,?,?,?,?,?)`,
		in.MachineID, session.ID, agentID, session.CWD, session.Title, actor, timestamp())
	return err
}

// roomForSession makes the task room a continued session gets of its own, under the project's home room, in the spawn's transaction: a spawn
// that fails leaves no room behind.
func (tx *Tx) roomForSession(anchorRoomID, title, actorID string) (Room, error) {
	var projectID, homeID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, anchorRoomID).Scan(&projectID); err != nil {
		return Room{}, err
	}
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home'`, projectID).Scan(&homeID); err != nil {
		return Room{}, err
	}
	return tx.CreateRoom(Room{ProjectID: projectID, ParentID: homeID, Kind: "task", Title: title, CreatedBy: actorID})
}
