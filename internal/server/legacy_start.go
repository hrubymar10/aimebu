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

	"github.com/hrubymar10/aimebu/internal/store"
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

// legacyStartChecks validates a request to start an agent from imported v1 history, before anything is chosen or created. It writes the
// error response itself. resuming says the harness session will be continued (the harness can, and v1 kept its id); otherwise the agent
// starts fresh under the same name.
func (s *Server) legacyStartChecks(w http.ResponseWriter, r *http.Request, room store.Room, actorKind string, req *spawnRequest) (legacy store.LegacyStart, resuming, ok bool) {
	if actorKind != "user" {
		apiError(w, 403, "forbidden", "only a person starts an agent from v1 history")
		return legacy, false, false
	}
	// Scoped to the project: an agent that never wrote in any of its rooms is not found, with nothing said about it.
	legacy, err := s.Store.LegacyStart(r.Context(), room.ProjectID, req.ResumeLegacy)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "no such agent in this project's history")
		return legacy, false, false
	}
	if err != nil {
		fail(w, err)
		return legacy, false, false
	}
	if legacy.StartedID != "" {
		apiError(w, 409, "already_started", "this v1 agent was already started as "+legacy.StartedSlg+"; restart that agent instead")
		return legacy, false, false
	}
	if !store.ValidAgentSlug(legacy.Slug) {
		apiError(w, 400, "invalid_name", "the v1 name "+legacy.Slug+" cannot be an agent name in v2")
		return legacy, false, false
	}
	if legacy.Tool != "" && store.ValidSessionHandle(legacy.SessionID) {
		resuming = true
		if req.Tool != legacy.Tool {
			apiError(w, 400, "tool_mismatch", "to continue this session the tool must be "+legacy.Tool)
			return legacy, false, false
		}
		if req.Worktree {
			apiError(w, 400, "invalid_request", "a continued session runs where it was made, so it cannot use a worktree")
			return legacy, false, false
		}
		if legacy.SeenRecently(time.Now()) && !req.ConfirmV1Idle {
			apiError(w, 409, "v1_may_be_active", "v1 saw this agent a moment ago and may still be using the session; stop it first, then confirm")
			return legacy, false, false
		}
	}
	return legacy, resuming, true
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
