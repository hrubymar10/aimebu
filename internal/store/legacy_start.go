package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

// LegacyStart is what the server knows about an agent that exists only in imported v1 history, for starting it in v2: where it ran,
// which harness session to continue, and the v2 agent already started from it, if any.
type LegacyStart struct {
	AgentID    string `json:"agent_id"`
	Slug       string `json:"slug"`
	Harness    string `json:"harness"`
	Tool       string `json:"tool,omitempty"` // the v2 tool the harness maps to; empty when it cannot be resumed
	Model      string `json:"model"`
	CWD        string `json:"cwd"`
	SessionID  string `json:"session_id,omitempty"`
	LastSeen   string `json:"last_seen,omitempty"`
	RoleKey    string `json:"role_key,omitempty"` // the role v1 gave it most often; the dialog checks v2 has it
	StartedID  string `json:"started_agent_id,omitempty"`
	StartedSlg string `json:"started_agent_slug,omitempty"`
}

// ResumableTool maps a v1 harness name to the v2 tool that can continue its session, or "" when none can. Only Claude Code can: it keeps
// its transcripts in the user's own claude home, which v1 and v2 share. v2 runs codex, pi and opencode with a private home or data
// directory per agent (CODEX_HOME, PI_CODING_AGENT_DIR, an opencode database), so a session v1 made is not visible to them.
func ResumableTool(harness string) string {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude-code", "claude":
		return "claude"
	}
	return ""
}

// ToolForHarness is the v2 tool a v1 harness corresponds to, resumable or not ("" for one v2 has no adapter for).
func ToolForHarness(harness string) string {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude-code", "claude":
		return "claude"
	case "codex":
		return "codex"
	case "pi":
		return "pi"
	case "opencode":
		return "opencode"
	}
	return ""
}

var agentSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,19}[a-z0-9]$`)

// LegacySlug is the name a v1 agent id carries ("sage@aimebu" is "sage"), lower-cased; it is valid only when it fits an AI agent's slug.
func LegacySlug(agentID string) string {
	name, _, _ := strings.Cut(agentID, "@")
	return strings.ToLower(name)
}

// ValidAgentSlug reports whether a name fits the AI agent slug rule and is not one of the group tags (@all, @here, ...), which win over
// a name in a mention and would leave the agent impossible to address.
func ValidAgentSlug(slug string) bool {
	return agentSlugPattern.MatchString(slug) && groupTag(slug) == ""
}

// RecentlySeenWindow is how recently v1 must have seen an agent for its session to count as possibly still in use by v1.
const RecentlySeenWindow = 10 * time.Minute

var sessionHandlePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,199}$`)

// ValidSessionHandle reports whether a harness session id looks like one: letters, digits and a few separators, never a path or an option.
func ValidSessionHandle(handle string) bool { return sessionHandlePattern.MatchString(handle) }

// legacyStartQuery reads the recorded session of a v1 agent that wrote in a room of the project; it goes by author first (the partial
// index message_agent_author_room), so it does not read the messages of every room of the project. A private chat is not a room of the
// project for this purpose.
const legacyStartQuery = `SELECT harness,model,cwd,harness_session_id,last_seen,started_agent_id FROM legacy_agent_session l
	WHERE l.agent_id=? AND EXISTS (SELECT 1 FROM message m JOIN room r ON r.id=m.room_id WHERE m.author_kind='agent' AND m.author_id=l.agent_id AND r.project_id=? AND r.kind<>'dm')`

// LegacyStart reads one legacy agent for starting in projectID: it must be an agent that wrote in some room of that project (a private chat
// does not count: whoever is not in it must not learn that the agent wrote there), so that one
// project's owner cannot start, or learn anything about, an agent that only worked in another project. Which room of the project it is
// started in is up to the caller: the room's own rules (lead-only home, permissions, limits) are checked there, not here.
// sql.ErrNoRows when v1 recorded no session for it or it never wrote in the project.
func (s *Store) LegacyStart(ctx context.Context, projectID, agentID string) (LegacyStart, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return LegacyStart{}, err
	}
	out := LegacyStart{AgentID: agentID, Slug: LegacySlug(agentID)}
	var started sql.NullString
	err := s.db.QueryRowContext(ctx, legacyStartQuery, agentID, projectID).
		Scan(&out.Harness, &out.Model, &out.CWD, &out.SessionID, &out.LastSeen, &started)
	if err != nil {
		return LegacyStart{}, err
	}
	out.Tool = ResumableTool(out.Harness)
	if started.Valid && started.String != "" {
		// Started once: from then on the v2 agent holds the name, and is restarted with the ordinary controls.
		var slug string
		err := s.db.QueryRowContext(ctx, `SELECT slug FROM agent WHERE id=?`, started.String).Scan(&slug)
		if err == nil {
			out.StartedID, out.StartedSlg = started.String, slug
		} else if !errors.Is(err, sql.ErrNoRows) {
			return LegacyStart{}, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT role_key,COUNT(*) FROM legacy_agent_role WHERE agent_id=? GROUP BY role_key`, agentID)
	if err != nil {
		return LegacyStart{}, err
	}
	defer rows.Close()
	type count struct {
		key string
		n   int
	}
	var counts []count
	for rows.Next() {
		var c count
		if err := rows.Scan(&c.key, &c.n); err != nil {
			return LegacyStart{}, err
		}
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		return LegacyStart{}, err
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].n != counts[j].n {
			return counts[i].n > counts[j].n
		}
		return counts[i].key < counts[j].key
	})
	if len(counts) > 0 {
		out.RoleKey = counts[0].key
	}
	return out, nil
}

// SeenRecently says whether the legacy agent's last_seen is within the window before now (an unparsable value counts as not recent).
func (l LegacyStart) SeenRecently(now time.Time) bool {
	seen, err := time.Parse(time.RFC3339, l.LastSeen)
	return err == nil && now.Sub(seen) < RecentlySeenWindow && now.Sub(seen) > -RecentlySeenWindow
}

// LegacyStarted is the v2 agent's link back to the v1 agent it continues, for its profile: the legacy id and name, or empty.
func (s *Store) LegacyStarted(ctx context.Context, agentID string) (legacyID string, err error) {
	var id sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT resumed_from_legacy_id FROM agent WHERE id=?`, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id.String, err
}

// LegacyStartInfo is what the web needs to offer "Start as v2 agent" for an imported sender.
type LegacyStartInfo struct {
	Slug           string `json:"slug"`
	Tool           string `json:"tool,omitempty"`
	Model          string `json:"model,omitempty"`
	RoleKey        string `json:"role_key,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	Resumable      bool   `json:"resumable"`
	Reason         string `json:"reason,omitempty"` // why a session cannot be continued, in plain words
	V1Active       bool   `json:"v1_active,omitempty"`
	OutsideFolder  bool   `json:"outside_folder,omitempty"`
	StartedAgentID string `json:"started_agent_id,omitempty"`
	StartedSlug    string `json:"started_agent_slug,omitempty"`
}

// StartInfo describes the legacy agent for the start dialog; projectFolder is the project's folder on the machine it would start on,
// to say whether the old working folder lies outside it.
func (l LegacyStart) StartInfo(now time.Time, projectFolder string) LegacyStartInfo {
	info := LegacyStartInfo{Slug: l.Slug, Tool: l.Tool, Model: l.Model, RoleKey: l.RoleKey, CWD: l.CWD, StartedAgentID: l.StartedID, StartedSlug: l.StartedSlg}
	switch {
	case l.Tool == "":
		info.Tool = ToolForHarness(l.Harness)
		if info.Tool != "" {
			info.Reason = "v2 keeps " + info.Tool + " sessions per agent, apart from v1's, so this v1 session cannot be continued: the agent starts fresh with the same name"
		} else {
			info.Reason = "this harness (" + l.Harness + ") has no v2 adapter and cannot resume a session: the agent starts fresh with the same name"
		}
	case l.SessionID == "" || !ValidSessionHandle(l.SessionID):
		info.Reason = "v1 did not keep a usable session id for this agent: the agent starts fresh with the same name"
	default:
		info.Resumable = true
		info.V1Active = l.SeenRecently(now)
		info.OutsideFolder = projectFolder != "" && !WithinFolder(projectFolder, l.CWD)
	}
	return info
}
