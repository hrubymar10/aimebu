package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrSessionAttached is a Claude Code session that a live agent already continues (or that another request attached first).
var ErrSessionAttached = errors.New("session already continued by an agent")

// ResumeSession is a Claude Code session an agent is started from: its first turn continues it (claude --resume) in the folder it was made in.
type ResumeSession struct {
	ID    string
	CWD   string
	Title string
	// Fork starts the agent on a copy of the session (the first turn runs with --fork-session) and leaves the original as it is.
	Fork bool
}

// forkPendingSQL is the SQL that says a session row is a fork whose first turn has not yet reported the copy's id: it still names the original.
const forkPendingSQL = `(se.fork_of IS NOT NULL AND se.native_session_id=se.fork_of)`

// SessionAgent is the agent that continues a session.
type SessionAgent struct {
	AgentID string `json:"agent_id"`
	Slug    string `json:"slug"`
}

// ClaudeSessionAgents says, for each of the given session ids on a machine, which live agent continues it: one started from it
// (imported_session, a continued session; a copy leaves its original free) or one whose own Claude session it is (an agent's session id is
// created by the server and Claude saves it; a fork that has not yet named its copy still names the original and does not count).
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
		WHERE i.machine_id=? AND i.mode='continue' AND i.session_id IN (%s) AND a.revoked_at IS NULL
		UNION SELECT se.native_session_id,a.id,a.slug FROM session se JOIN agent a ON a.id=se.agent_id
		WHERE a.machine_id=? AND a.tool='claude' AND a.revoked_at IS NULL AND se.native_session_id IN (%s) AND NOT `+forkPendingSQL, marks, marks), args...)
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

// ImportedSession is the Claude Code session an agent was started from: the original's id, how it was taken over ("continue" or "fork"),
// and for a copy the id Claude gave it once the agent's first turn reported it.
type ImportedSession struct {
	SessionID       string
	Mode            string
	ForkedSessionID string
	CWD             string
	Title           string
}

// AgentImportedSession is the session an agent was started from; ok is false for an agent that was not started from one.
func (s *Store) AgentImportedSession(ctx context.Context, agentID string) (ImportedSession, bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return ImportedSession{}, false, err
	}
	var item ImportedSession
	err := s.db.QueryRowContext(ctx, `SELECT session_id,mode,COALESCE(forked_session_id,''),cwd,title FROM imported_session WHERE agent_id=?`, agentID).
		Scan(&item.SessionID, &item.Mode, &item.ForkedSessionID, &item.CWD, &item.Title)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportedSession{}, false, nil
	}
	return item, err == nil, err
}

// attachClaudeSession records that the agent continues the session (or works on a copy of it), refusing a continued session a live agent
// already holds. A row left by an agent that was revoked since is replaced: that agent no longer touches the transcript. A copy does not
// hold its original, so any number of agents can fork one session.
func (tx *Tx) attachClaudeSession(in SpawnInput, agentID string) error {
	session := in.ResumeSession
	if !session.Fork {
		var held int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT
			(SELECT COUNT(*) FROM imported_session i JOIN agent a ON a.id=i.agent_id WHERE i.machine_id=? AND i.mode='continue' AND i.session_id=? AND a.revoked_at IS NULL)
			+ (SELECT COUNT(*) FROM session se JOIN agent a ON a.id=se.agent_id WHERE a.machine_id=? AND a.tool='claude' AND a.revoked_at IS NULL
				AND se.native_session_id=? AND a.id<>? AND NOT `+forkPendingSQL+`)`, in.MachineID, session.ID, in.MachineID, session.ID, agentID).Scan(&held); err != nil {
			return err
		}
		if held > 0 {
			return ErrSessionAttached
		}
		if _, err := tx.Exec(`DELETE FROM imported_session WHERE machine_id=? AND session_id=? AND mode='continue'`, in.MachineID, session.ID); err != nil {
			return err
		}
	}
	mode := "continue"
	if session.Fork {
		mode = "fork"
	}
	_, err := tx.Exec(`INSERT INTO imported_session(agent_id,machine_id,session_id,mode,cwd,title,imported_by,imported_at) VALUES (?,?,?,?,?,?,?,?)`,
		agentID, in.MachineID, session.ID, mode, session.CWD, session.Title, in.ActorID, timestamp())
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

// AgentForkPending says the agent's next turn forks a Claude Code session: it was started as a copy and its first turn has not yet named the
// copy, so the session row still names the original. Sending that turn to a supervisor that ignores the fork flag would resume, and so
// append to, the original.
func (s *Store) AgentForkPending(ctx context.Context, agentID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var pending sql.NullBool
	err := s.db.QueryRowContext(ctx, `SELECT fork_of IS NOT NULL AND native_session_id=fork_of FROM session WHERE agent_id=? ORDER BY generation DESC LIMIT 1`, agentID).Scan(&pending)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return pending.Valid && pending.Bool, err
}

// AgentLaunchPathPending says the agent's turns must start in a spelling of its folder other than the canonical one: it is a Claude agent that
// continues a Claude Code session made under a link, and Claude finds the session only from the folder as it was spelled then. Sending such a
// turn to a supervisor that ignores the spelling would start Claude where the session cannot be found. An agent switched to another tool needs
// no spelling any more, so it is never held for one.
func (s *Store) AgentLaunchPathPending(ctx context.Context, agentID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var differs sql.NullBool
	err := s.db.QueryRowContext(ctx, `SELECT a.tool='claude' AND a.launch_path<>'' AND a.launch_path<>c.canonical_path FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, agentID).Scan(&differs)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return differs.Valid && differs.Bool, err
}

// AgentLaunchPath is the spelling of its folder the agent's harness is started in (agent.launch_path), empty when it has none.
func (s *Store) AgentLaunchPath(ctx context.Context, agentID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var launch string
	err := s.db.QueryRowContext(ctx, `SELECT launch_path FROM agent WHERE id=?`, agentID).Scan(&launch)
	return launch, err
}

// Why a turn is held back for a supervisor that cannot run it (NoteSessionHeld).
const (
	HeldForFork       = "it works on a copy of a Claude Code session, and the supervisor on %s predates that (it would write into the original)"
	HeldForLaunchPath = "it continues a Claude Code session made under a link to its folder, and the supervisor on %s predates starting Claude in that spelling (it would not find the session)"
)

// NoteSessionHeld tells the room(s) where the agent has queued notes why it is not starting: its machine's supervisor cannot run its turns,
// and nothing was sent to it. why is one of the HeldFor constants (it takes the machine's name). An agent with nothing queued (a recovery
// turn) is told in the first room it joined.
func (s *Store) NoteSessionHeld(ctx context.Context, agentID, why string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var slug, machine string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.slug,mc.name FROM agent a JOIN machine mc ON mc.id=a.machine_id WHERE a.id=?`, agentID).Scan(&slug, &machine); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT room_id FROM note WHERE agent_id=? AND state='queued' GROUP BY room_id ORDER BY MIN(rowid)`, agentID)
		if err != nil {
			return err
		}
		var rooms []string
		for rows.Next() {
			var room string
			if err := rows.Scan(&room); err != nil {
				rows.Close()
				return err
			}
			rooms = append(rooms, room)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(rooms) == 0 {
			var room string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM room_member WHERE member_id=? AND member_kind='agent' AND left_at IS NULL ORDER BY joined_at LIMIT 1`, agentID).Scan(&room)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			rooms = []string{room}
		}
		for _, room := range rooms {
			if _, err := tx.InsertMessage(Message{RoomID: room, AuthorKind: "system", AuthorID: "system",
				Body: fmt.Sprintf("%s is held: "+why+". Update the supervisor; nothing was sent to it.", slug, machine)}); err != nil {
				return err
			}
		}
		return nil
	})
}

// agentLaunchPath is the spelling the agent's harness is started in when it differs from the checkout's canonical path (agent.launch_path):
// the folder a Claude Code session was made under, kept as spelled because Claude keys its sessions by it. Empty for every agent that was not
// started from such a session, for an agent that is not (or no longer) a Claude agent, and when the spelling is the canonical path itself.
func (tx *Tx) agentLaunchPath(agentID, tool, checkoutPath string) (string, error) {
	if tool != "claude" {
		return "", nil
	}
	var launch string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT launch_path FROM agent WHERE id=?`, agentID).Scan(&launch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if launch == "" || launch == checkoutPath || !filepath.IsAbs(launch) {
		return "", nil
	}
	return launch, nil
}
