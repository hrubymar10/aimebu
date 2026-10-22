package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// AgentProfile is the read-only detail of one agent for the web's Agent profile panel: what the
// card does not carry (the native session, where it runs, who started it, today's use). Every
// field is optional; a value the server does not know is left empty rather than guessed.
type AgentProfile struct {
	AgentID          string `json:"agent_id"`
	NativeSessionID  string `json:"native_session_id,omitempty"`
	SessionStartedAt string `json:"session_started_at,omitempty"`
	StartMode        string `json:"start_mode,omitempty"`
	ResumeCommand    string `json:"resume_command,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	Worktree         bool   `json:"worktree,omitempty"`
	Branch           string `json:"branch,omitempty"`
	MachineName      string `json:"machine_name,omitempty"`
	LauncherName     string `json:"launcher_name,omitempty"`
	ProjectName      string `json:"project_name,omitempty"`
	RepoURL          string `json:"repo_url,omitempty"`
	SpawnedBy        string `json:"spawned_by,omitempty"`
	SpawnedAt        string `json:"spawned_at,omitempty"`
	// Imported marks the profile of a sender from v1 history, built from the session v1 recorded for it; LastSeen is when v1 last saw it.
	Imported bool   `json:"imported,omitempty"`
	LastSeen string `json:"last_seen,omitempty"`
	// Start is what starting a historical sender as a v2 agent needs (only for an imported sender, only for someone who may spawn agents);
	// ResumedFrom is, on a v2 agent that was started from one, the v1 agent it continues.
	Start       *LegacyStartInfo `json:"start,omitempty"`
	ResumedFrom string           `json:"resumed_from,omitempty"`
	// Turns and cost over the last 24 hours.
	TurnsToday int      `json:"turns_today"`
	CostToday  *float64 `json:"cost_today,omitempty"`
}

// AgentProfile reads the profile of an agent that is a member of roomID, or that was one and has spoken in it (a departed sender's
// name opens its profile from the chat).
func (s *Store) AgentProfile(ctx context.Context, roomID, agentID string) (AgentProfile, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentProfile{}, err
	}
	p := AgentProfile{AgentID: agentID}
	var tool, machineID, checkoutID, launcherID, projectID, createdBy string
	err := s.db.QueryRowContext(ctx, `SELECT a.tool,a.machine_id,a.checkout_id,COALESCE(a.launcher_id,''),a.project_id,a.created_by,a.created_at
		FROM agent a LEFT JOIN room_member m ON m.member_id=a.id AND m.member_kind='agent' AND m.left_at IS NULL AND m.room_id=?
		WHERE a.id=? AND (m.member_id IS NOT NULL OR EXISTS (SELECT 1 FROM message WHERE room_id=? AND author_kind='agent' AND author_id=a.id))`, roomID, agentID, roomID).Scan(&tool, &machineID, &checkoutID, &launcherID, &projectID, &createdBy, &p.SpawnedAt)
	if err != nil {
		return AgentProfile{}, err
	}
	var native sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT native_session_id,start_mode,started_at FROM session WHERE agent_id=? ORDER BY generation DESC LIMIT 1`, agentID).
		Scan(&native, &p.StartMode, &p.SessionStartedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AgentProfile{}, err
	}
	p.NativeSessionID = native.String
	if err := s.db.QueryRowContext(ctx, `SELECT canonical_path FROM checkout WHERE id=?`, checkoutID).Scan(&p.Cwd); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AgentProfile{}, err
	}
	// A room with a ready worktree on the agent's machine is where it actually works.
	var wtPath, branch string
	err = s.db.QueryRowContext(ctx, `SELECT i.canonical_path,w.branch_name FROM room_worktree w
		JOIN room_worktree_instance i ON i.room_id=w.room_id AND i.machine_id=?
		WHERE w.room_id=? AND w.state='ready' AND i.state='ready'`, machineID, roomID).Scan(&wtPath, &branch)
	if err == nil {
		// Only when that worktree is the agent's own checkout: an agent started elsewhere (a continued v1 session in its old folder)
		// does not work in the room's worktree.
		if wtPath == p.Cwd {
			p.Worktree, p.Branch = true, branch
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return AgentProfile{}, err
	}
	for _, query := range []struct {
		sql  string
		args []any
		dest []any
	}{
		{`SELECT name FROM machine WHERE id=?`, []any{machineID}, []any{&p.MachineName}},
		{`SELECT name,COALESCE(repo_url,'') FROM project WHERE id=?`, []any{projectID}, []any{&p.ProjectName, &p.RepoURL}},
	} {
		if err := s.db.QueryRowContext(ctx, query.sql, query.args...).Scan(query.dest...); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return AgentProfile{}, err
		}
	}
	containerized := false
	if launcherID != "" {
		if err := s.db.QueryRowContext(ctx, `SELECT name,containerized FROM machine_launcher WHERE id=?`, launcherID).Scan(&p.LauncherName, &containerized); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return AgentProfile{}, err
		}
	}
	// Who started it: a person's login, or the agent that spawned it.
	p.SpawnedBy = createdBy
	var login string
	if err := s.db.QueryRowContext(ctx, `SELECT login FROM user WHERE id=?`, createdBy).Scan(&login); err == nil {
		p.SpawnedBy = login
	} else if !errors.Is(err, sql.ErrNoRows) {
		return AgentProfile{}, err
	} else {
		var slug string
		if err := s.db.QueryRowContext(ctx, `SELECT slug FROM agent WHERE id=?`, createdBy).Scan(&slug); err == nil {
			p.SpawnedBy = slug
		}
	}
	// A session inside a container cannot be resumed from the host with this command, so none is offered.
	if !containerized {
		p.ResumeCommand = resumeCommand(tool, p.NativeSessionID, p.Cwd)
	}
	if p.ResumedFrom, err = s.LegacyStarted(ctx, agentID); err != nil {
		return AgentProfile{}, err
	}
	since := time.Now().UTC().Add(-24 * time.Hour).Format(timestampLayout)
	var cost sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, agentTodaySQL, agentID, since).Scan(&p.TurnsToday, &cost); err != nil {
		return AgentProfile{}, err
	}
	if cost.Valid {
		p.CostToday = &cost.Float64
	}
	return p, nil
}

// agentTodaySQL counts the agent's turns that reported usage, and their cost, since a time.
const agentTodaySQL = `SELECT COUNT(DISTINCT turn_id),SUM(cost_est) FROM usage_record WHERE agent_id=? AND created_at>=?`

// resumeCommand is the command a person types to continue the agent's native session by hand. It is only
// given for tools whose resume syntax is known, and only when the session id is known.
func resumeCommand(tool, session, cwd string) string {
	if session == "" {
		return ""
	}
	var command string
	switch tool {
	case "claude":
		command = "claude --resume " + shellQuote(session)
	case "codex":
		command = "codex resume " + shellQuote(session)
	default:
		return ""
	}
	if cwd != "" {
		return "cd " + shellQuote(cwd) + " && " + command
	}
	return command
}

func shellQuote(value string) string {
	if value != "" && strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_") == "" {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// WithoutHostDetails drops what lets a reader reach the agent's session on its machine: the native session id,
// the resume command and the working directory. A project viewer reads the room but does not operate agents.
func (p AgentProfile) WithoutHostDetails() AgentProfile {
	p.NativeSessionID, p.ResumeCommand, p.Cwd = "", "", ""
	return p
}

// LegacyAgentProfile is the profile of a sender that exists only in imported v1 history: the session v1 recorded for that agent id,
// shown only for an id that wrote in roomID. It has no resume command: v1's ran against v1, and a database imported before that
// was dropped still holds the old text, which is never read. sql.ErrNoRows when v1 recorded none or the agent never wrote there.
func (s *Store) LegacyAgentProfile(ctx context.Context, roomID, agentID string) (AgentProfile, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentProfile{}, err
	}
	p := AgentProfile{AgentID: agentID, Imported: true}
	err := s.db.QueryRowContext(ctx, `SELECT harness_session_id,cwd,registered_at,last_seen,project FROM legacy_agent_session l
		WHERE l.agent_id=? AND EXISTS (SELECT 1 FROM message WHERE room_id=? AND author_kind='agent' AND author_id=l.agent_id)`, agentID, roomID).
		Scan(&p.NativeSessionID, &p.Cwd, &p.SpawnedAt, &p.LastSeen, &p.ProjectName)
	return p, err
}
