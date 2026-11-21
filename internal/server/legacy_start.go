package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// sessionCheck is what looking for a harness session on the host found.
type sessionCheck int

const (
	sessionFound     sessionCheck = iota // the session is where the harness will look for it
	sessionMissing                       // no trace of it on this machine
	sessionUnchecked                     // this server cannot look (another machine, a container, a harness whose layout is not known)
)

// legacyResumeCWD is the folder a resumed session must run in. The harness looks the session up under the working folder it was made in
// (claude keeps transcripts per folder, opencode per project), so the agent works where v1 did, even outside the project folder; one
// that is gone is refused. On the host it is resolved here; on another machine that machine resolves it.
func (s *Server) legacyResumeCWD(ctx context.Context, machine store.Machine, cwd string) (string, error) {
	return s.resumeCWD(ctx, machine, cwd, "the folder v1 worked in")
}

// resumeCWD resolves the folder a continued session must run in on the machine: it has to exist there. subject names the folder in an
// error ("the folder v1 worked in", "the folder the session worked in").
func (s *Server) resumeCWD(ctx context.Context, machine store.Machine, cwd, subject string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("no usable working folder was recorded for it")
	}
	if machine.IsHost {
		real, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return "", fmt.Errorf("%s (%s) does not exist on %s", subject, cwd, machine.Name)
		}
		if info, err := os.Stat(real); err != nil || !info.IsDir() {
			return "", fmt.Errorf("%s (%s) is not a folder on %s", subject, cwd, machine.Name)
		}
		return filepath.Clean(real), nil
	}
	canonical, err := s.probeMachinePath(ctx, machine.ID, cwd)
	if err != nil {
		return "", fmt.Errorf("%s (%s) could not be checked on %s: %w", subject, cwd, machine.Name, err)
	}
	return canonical, nil
}

// checkHarnessSession looks for a Claude Code session's transcript in the claude home, on the host only and only for the native launcher (a
// container has its own home). It never reads a transcript: it only looks at names.
func checkHarnessSession(tool, handle, cwd string) sessionCheck {
	home, _ := os.UserHomeDir()
	switch tool {
	case "claude":
		base := os.Getenv("CLAUDE_CONFIG_DIR")
		if base == "" {
			base = filepath.Join(home, ".claude")
		}
		matches, _ := filepath.Glob(filepath.Join(base, "projects", "*", handle+".jsonl"))
		if len(matches) == 0 {
			return sessionMissing
		}
		for _, match := range matches {
			if filepath.Base(filepath.Dir(match)) == claudeFolderName(cwd) {
				return sessionFound
			}
		}
		// The transcript exists under another folder name. Claude's naming has more cases than are worth mirroring (long paths are
		// truncated with a hash and the rule has changed between versions), so a name that does not match is not proof that the
		// session cannot be resumed from here: let the harness decide.
		return sessionUnchecked
	}
	return sessionUnchecked
}

// claudeFolderName is how claude names a working folder's transcript directory: every character that is not a letter or a digit
// (separators, dots, underscores, spaces, "@", "+", "~", ...) becomes a dash.
func claudeFolderName(cwd string) string {
	return nonAlphanumeric.ReplaceAllString(cwd, "-")
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]`)

// legacyCheckError is a refusal of a start from v1 history: the HTTP status and code it is reported with, and the words.
type legacyCheckError struct {
	status    int
	code, msg string
}

func (e *legacyCheckError) Error() string { return e.msg }

// legacyStartChecks validates a request to start an agent from imported v1 history, before anything is chosen or created. It writes the
// error response itself. resuming says the harness session will be continued (the harness can, and v1 kept its id); otherwise the agent
// starts fresh under the same name.
func (s *Server) legacyStartChecks(w http.ResponseWriter, r *http.Request, room store.Room, actorKind string, req *spawnRequest) (legacy store.LegacyStart, resuming, ok bool) {
	legacy, resuming, err := s.checkLegacyStart(r.Context(), room.ProjectID, actorKind, req)
	if err != nil {
		var refusal *legacyCheckError
		if errors.As(err, &refusal) {
			apiError(w, refusal.status, refusal.code, refusal.msg)
		} else {
			fail(w, err)
		}
		return legacy, false, false
	}
	return legacy, resuming, true
}

// checkLegacyStart is the check behind legacyStartChecks, for a start that is not a spawn request (a team flow's leader): a refusal is a
// *legacyCheckError, anything else is a failure of the server.
func (s *Server) checkLegacyStart(ctx context.Context, projectID, actorKind string, req *spawnRequest) (legacy store.LegacyStart, resuming bool, err error) {
	if actorKind != "user" {
		return legacy, false, &legacyCheckError{403, "forbidden", "only a person starts an agent from v1 history"}
	}
	// Scoped to the project: an agent that never wrote in any of its rooms is not found, with nothing said about it.
	legacy, err = s.resolveLegacyStart(ctx, projectID, req.ResumeLegacy)
	if errors.Is(err, sql.ErrNoRows) {
		return legacy, false, &legacyCheckError{404, "not_found", "no such agent in this project's history"}
	}
	if err != nil {
		return legacy, false, err
	}
	if legacy.StartedID != "" {
		return legacy, false, &legacyCheckError{409, "already_started", "this v1 agent was already started as " + legacy.StartedSlg + "; restart that agent instead"}
	}
	if !store.ValidAgentSlug(legacy.Slug) {
		return legacy, false, &legacyCheckError{400, "invalid_name", "the v1 name " + legacy.Slug + " cannot be an agent name in v2"}
	}
	if legacy.Tool != "" && store.ValidSessionHandle(legacy.SessionID) {
		resuming = true
		if req.Tool != legacy.Tool {
			return legacy, false, &legacyCheckError{400, "tool_mismatch", "to continue this session the tool must be " + legacy.Tool}
		}
		if req.Worktree {
			return legacy, false, &legacyCheckError{400, "invalid_request", "a continued session runs where it was made, so it cannot use a worktree"}
		}
		// A copy leaves the original as it is, so v1 still having it open is no obstacle.
		if legacy.SeenRecently(time.Now()) && !req.ConfirmV1Idle && req.ResumeMode != resumeFork {
			return legacy, false, &legacyCheckError{409, "v1_may_be_active", "v1 saw this agent a moment ago and may still be using the session; stop it first, then confirm"}
		}
	}
	if req.ResumeMode == resumeFork && !resuming {
		return legacy, false, &legacyCheckError{400, "invalid_request", "this agent's session cannot be continued, so there is nothing to work on a copy of"}
	}
	if req.ResumeMode == resumeFork && legacy.Tool != "claude" {
		return legacy, false, &legacyCheckError{400, "invalid_request", "working on a copy is available only for Claude Code sessions"}
	}
	return legacy, resuming, nil
}

// sessionsMessage says why a Claude Code session cannot be continued through a containerized launcher, and what would make it possible. A
// Docker launcher that works in the host's Claude home (the claude_home_shared feature) can; any other keeps its own home and cannot see a
// session made elsewhere.
func sessionsMessage(tool string, launcher *store.Launcher) string {
	switch {
	case tool != "claude":
		return "a containerized launcher keeps its own sessions, so an existing session can only be continued with the native launcher"
	case launcher != nil && launcher.EnvRefs["CLAUDE_CONFIG_DIR"] != "":
		return "this Docker launcher remaps CLAUDE_CONFIG_DIR, which claude-docker does not honour: a session made on the host is not in the home the container uses; remove the reference, or continue it with the native claude launcher"
	}
	return "this Docker launcher does not report that its Claude home is the host's (claude_home_shared), so a session made on the host cannot be assumed visible inside it; update harness-docker, or continue the session with the native claude launcher"
}

// sharesClaudeHome says a launcher runs Claude in the host's Claude home, as its supervisor measured it: the Docker launcher printed
// claude_home_shared, and nothing remaps CLAUDE_CONFIG_DIR for it (claude-docker does not honour that, so the scan would read another home
// than the container uses). Unknown is no.
func sharesClaudeHome(conn *machineConn, launcher *store.Launcher) bool {
	if conn == nil || launcher == nil || !launcher.Containerized || launcher.Tool != "claude" || launcher.EnvRefs["CLAUDE_CONFIG_DIR"] != "" {
		return false
	}
	conn.harnessMu.Lock()
	defer conn.harnessMu.Unlock()
	for _, row := range conn.launchers {
		if row.ID == launcher.ID {
			for _, feature := range row.Features {
				if feature == adapter.FeatureClaudeHomeShared {
					return true
				}
			}
		}
	}
	return false
}

// launcherKeepsOwnSessions says whether an agent started with this launcher runs in a container, which has its own home and so cannot see
// a v1 session. A launcher says so itself (Containerized), whatever it is named; only a machine with no launcher records falls back to
// the variant name.
func launcherKeepsOwnSessions(launcher *store.Launcher, variant string) bool {
	if launcher != nil {
		return launcher.Containerized
	}
	return variant == "docker"
}

// legacyAgents lists the imported v1 agents of a project that held a role and are not started yet (GET /api/projects/{project}/legacy-agents
// ?role=leader), each as the start info of its profile: what the team flow wizard offers to continue instead of a new agent. For the
// project's owners and operators, who may start them. Only an agent whose most frequent v1 role is the one asked for is listed.
func (s *Server) legacyAgents(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	if !s.projectOperator(w, r, projectID) {
		return
	}
	role := r.URL.Query().Get("role")
	if role == "" {
		apiError(w, 400, "invalid_request", "role required")
		return
	}
	ids, err := s.Store.LegacyAgentsWithRole(r.Context(), projectID, role)
	if err != nil {
		fail(w, err)
		return
	}
	folder, _ := s.Store.ProjectFolder(r.Context(), projectID)
	agents := []store.LegacyStartInfo{}
	for _, id := range ids {
		legacy, err := s.resolveLegacyStart(r.Context(), projectID, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			fail(w, err)
			return
		}
		if legacy.StartedID != "" || legacy.RoleKey != role || !store.ValidAgentSlug(legacy.Slug) {
			continue
		}
		agents = append(agents, legacy.StartInfo(time.Now(), folder))
	}
	writeJSON(w, 200, map[string]any{"agents": agents})
}
