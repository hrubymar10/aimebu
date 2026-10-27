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
	// Mismatch says, in plain words, why the session v1 kept under this name was not made by the agent whose messages are in the history
	// (another harness or model, or a time before its first message). A session that does not belong is never offered: SessionID and Tool
	// are empty, and Harness and Model are what the messages say.
	Mismatch string `json:"mismatch,omitempty"`
	// Exact says the session is the one v1 itself named as the sender of the agent's latest message (its stamp, matched against the history),
	// or that a transcript scan found it holding that message; otherwise it is the latest session v1 kept under the name, checked as far as
	// the messages allow.
	Exact bool `json:"exact,omitempty"`
}

// ResumableTool maps a v1 harness name to the v2 tool that can continue its session, or "" when none can. Only Claude Code can: it keeps
// its transcripts in the user's own claude home, which v1 and v2 share. v2 runs codex, pi, opencode and vibe with a private home or data
// directory per agent (CODEX_HOME, PI_CODING_AGENT_DIR, an opencode database, VIBE_HOME), so a session v1 made is not visible to them.
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
	case "vibe":
		return "vibe"
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
	if err := s.checkLegacySession(ctx, projectID, &out); err != nil {
		return LegacyStart{}, err
	}
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

// legacyActivity is what v1's messages say about the agent that wrote them: the harness and model of its latest message that names them
// (older messages carry neither), and the time span of the messages that share that harness and model.
type legacyActivity struct {
	harness, model, first, last string
}

// legacyActivityOf reads the activity of agentID in projectID's rooms (a private chat does not count). found is false when none of its messages
// names a harness, in which case nothing can be said against its session.
func (s *Store) legacyActivityOf(ctx context.Context, projectID, agentID string) (act legacyActivity, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT m.author_harness,m.author_model FROM message m JOIN room r ON r.id=m.room_id
		WHERE m.author_kind='agent' AND m.author_id=? AND r.project_id=? AND r.kind<>'dm' AND m.author_harness<>''
		ORDER BY m.created_at DESC, m.seq DESC LIMIT 1`, agentID, projectID).Scan(&act.harness, &act.model)
	if errors.Is(err, sql.ErrNoRows) {
		return act, false, nil
	}
	if err != nil {
		return act, false, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT MIN(m.created_at),MAX(m.created_at) FROM message m JOIN room r ON r.id=m.room_id
		WHERE m.author_kind='agent' AND m.author_id=? AND r.project_id=? AND r.kind<>'dm' AND m.author_harness=? AND m.author_model=?`,
		agentID, projectID, act.harness, act.model).Scan(&act.first, &act.last)
	return act, err == nil, err
}

// legacySessionSlack is how far a v1 session's last_seen may lie before the agent's first message, or its registration after the last, and
// still count as the same activity: the clocks are the same machine's, but a registration is recorded before the first message and a
// heartbeat after the last.
const legacySessionSlack = 6 * time.Hour

// canonModel is a model name reduced to what two spellings of one model share: v1 canonicalises slugs ("opus5.5"), but a hand-set name may
// differ in case or punctuation.
func canonModel(model string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(model) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sessionMismatch compares a session v1 recorded under a name with what that name's messages say. v1 keeps one session per name, so a name
// that was reused by another harness or model, or by a new instance weeks later, still holds the earlier holder's session. It answers ""
// when the session can belong to the agent, and otherwise why it cannot.
func sessionMismatch(harness, model, registeredAt, lastSeen string, act legacyActivity) string {
	sessionTool, messageTool := ToolForHarness(harness), ToolForHarness(act.harness)
	if sessionTool == "" {
		sessionTool = strings.ToLower(strings.TrimSpace(harness))
	}
	if messageTool == "" {
		messageTool = strings.ToLower(strings.TrimSpace(act.harness))
	}
	if sessionTool != "" && messageTool != "" && sessionTool != messageTool {
		return "v1 kept a " + harness + " session under this name, but its messages came from " + act.harness + ", so that session is not this agent's"
	}
	sm, am := canonModel(model), canonModel(act.model)
	if sm != "" && am != "" && sm != "unknown" && am != "unknown" && sm != am {
		return "v1 kept a session of model " + model + " under this name, but its messages came from " + act.model + ", so that session is not this agent's"
	}
	first, errFirst := time.Parse(time.RFC3339, act.first)
	last, errLast := time.Parse(time.RFC3339, act.last)
	if seen, err := time.Parse(time.RFC3339, lastSeen); err == nil && errFirst == nil && seen.Before(first.Add(-legacySessionSlack)) {
		return "v1 last saw the session it kept under this name on " + seen.UTC().Format("2006-01-02") + ", before this agent's first message (" + first.UTC().Format("2006-01-02") + "), so that session is not this agent's"
	}
	if registered, err := time.Parse(time.RFC3339, registeredAt); err == nil && errLast == nil && registered.After(last.Add(legacySessionSlack)) {
		return "v1 recorded the session it kept under this name after this agent's last message, so that session is not this agent's"
	}
	return ""
}

// checkLegacySession drops the session of a legacy start that does not belong to the agent whose messages are in the project (see
// sessionMismatch), and shows what the messages say instead of what the stray session says.
func (s *Store) checkLegacySession(ctx context.Context, projectID string, out *LegacyStart) error {
	// Exact first: a v1 message that names the session it was sent from says which session did that sender's work, whatever v1 kept as the
	// latest session of the name.
	var sid string
	err := s.db.QueryRowContext(ctx, `SELECT m.author_session FROM message m JOIN room r ON r.id=m.room_id
		WHERE m.author_kind='agent' AND m.author_id=? AND r.project_id=? AND r.kind<>'dm' AND m.author_session<>''
		ORDER BY m.created_at DESC, m.seq DESC LIMIT 1`, out.AgentID, projectID).Scan(&sid)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if sid != "" {
		if sid == out.SessionID {
			out.Exact = true
			return nil // the latest session of the name is the one that sent the latest message
		}
		var row struct{ harness, model, cwd, seen string }
		err := s.db.QueryRowContext(ctx, `SELECT harness,model,cwd,last_seen FROM legacy_agent_session_history WHERE agent_id=? AND harness_session_id=?`, out.AgentID, sid).
			Scan(&row.harness, &row.model, &row.cwd, &row.seen)
		if err == nil {
			out.SessionID, out.Harness, out.Model, out.CWD, out.LastSeen = sid, row.harness, row.model, row.cwd, row.seen
			out.Tool = ResumableTool(out.Harness)
			out.Exact = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		out.Mismatch = "v1 stamped this agent's latest message with session " + sid + ", which it kept no record of, so the session v1 kept under this name is not this agent's"
		out.SessionID, out.Tool = "", ""
		return nil
	}
	act, found, err := s.legacyActivityOf(ctx, projectID, out.AgentID)
	if err != nil || !found {
		return err
	}
	var registered string
	if err := s.db.QueryRowContext(ctx, `SELECT registered_at FROM legacy_agent_session WHERE agent_id=?`, out.AgentID).Scan(&registered); err != nil {
		return err
	}
	reason := sessionMismatch(out.Harness, out.Model, registered, out.LastSeen, act)
	if reason == "" {
		return nil
	}
	out.Mismatch = reason
	out.Harness, out.Model, out.SessionID, out.Tool = act.harness, act.model, "", ""
	return nil
}

// LegacySenderMessages describes what a v1 agent sent in a project, for matching it to a transcript: the time of its first message and the
// numbers (the v1 message ids, which are what its bus_say returned) of its latest ones, newest first, at most limit.
func (s *Store) LegacySenderMessages(ctx context.Context, projectID, agentID string, limit int) (first string, nos []int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.no,m.created_at FROM message m JOIN room r ON r.id=m.room_id
		WHERE m.author_kind='agent' AND m.author_id=? AND r.project_id=? AND r.kind<>'dm' AND m.no>0 ORDER BY m.no DESC LIMIT ?`, agentID, projectID, limit)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var no int64
		var at string
		if err := rows.Scan(&no, &at); err != nil {
			return "", nil, err
		}
		nos = append(nos, no)
		if first == "" || at < first {
			first = at
		}
	}
	return first, nos, rows.Err()
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
	case l.Mismatch != "":
		info.Tool = ToolForHarness(l.Harness)
		info.CWD = ""
		info.Reason = l.Mismatch + ": the agent starts fresh with the same name"
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
