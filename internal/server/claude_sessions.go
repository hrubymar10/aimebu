package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/ccsessions"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

var (
	errSessionScanUnsupported = errors.New("this machine's supervisor predates Claude Code session scans; update it")
	errSessionNotFound        = errors.New("this machine has no Claude Code session with that id")
	errSessionUnreadable      = errors.New("that session's transcript has no readable working folder, so it cannot be continued from here")
)

const sessionScanTimeout = 15 * time.Second

// scanClaudeSessions asks a machine's supervisor for its Claude Code sessions: those that worked in one of the spellings of a folder (or
// below), or the one session with the id. The supervisor reads the Claude home of the launcher that would continue the session (the one
// its environment references select), empty meaning the supervisor's own.
func (s *Server) scanClaudeSessions(ctx context.Context, conn *machineConn, launcherID string, folders []string, sessionID string) (protocol.Frame, error) {
	return s.requestSessionScan(ctx, conn, protocol.Frame{LauncherID: launcherID, ScanFolders: folders, ScanSessionID: sessionID}, sessionScanTimeout)
}

// requestSessionScan sends a session_scan frame (the request fields of scan; the rest is filled in here) and waits for the supervisor's
// answer.
func (s *Server) requestSessionScan(ctx context.Context, conn *machineConn, scan protocol.Frame, timeout time.Duration) (protocol.Frame, error) {
	if !conn.supportsSessionScan {
		return protocol.Frame{}, errSessionScanUnsupported
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return protocol.Frame{}, err
	}
	id := hex.EncodeToString(nonce[:])
	channel := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	if conn.pathPending == nil {
		conn.pathPending = make(map[string]chan protocol.Frame)
	}
	conn.pathPending[id] = channel
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.pathPending, id); conn.harnessMu.Unlock() }()
	scan.V, scan.Type, scan.ReqID = protocol.Version, protocol.SessionScan, id
	if err := conn.send(scan); err != nil {
		return protocol.Frame{}, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reply := <-channel:
		switch reply.Code {
		case "":
			return reply, nil
		case "session_not_found":
			return protocol.Frame{}, errSessionNotFound
		case "session_unreadable":
			return protocol.Frame{}, errSessionUnreadable
		default:
			return protocol.Frame{}, fmt.Errorf("session scan failed: %s", reply.Message)
		}
	case <-conn.done:
		return protocol.Frame{}, errors.New("session scan failed: the supervisor disconnected")
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	case <-timer.C:
		return protocol.Frame{}, errors.New("session scan timed out")
	}
}

// claudeSessionView is one session as the Spawn dialog lists it.
type claudeSessionView struct {
	protocol.SessionCandidate
	// OutsideFolder says the session worked outside the project's folder: continuing it needs the owner's approval first.
	OutsideFolder bool `json:"outside_folder"`
	// Agent is the live agent that already continues it.
	Agent *store.SessionAgent `json:"agent,omitempty"`
}

// claudeSessions lists the Claude Code sessions of a project's folder on a machine (GET /api/projects/{project}/claude-sessions
// ?machine_id=), or finds one by id wherever it worked (?session_id=). For the project's owners and operators: a title is what somebody
// typed to Claude, and starting an agent from a session is theirs to decide.
func (s *Server) claudeSessions(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	if !s.projectOperator(w, r, projectID) {
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	conn, machine, err := s.connectedMachine(r.Context(), machineID)
	if err != nil {
		machineSelectionError(w, machine, err)
		return
	}
	launcherID, ok := s.claudeLauncher(w, r, conn, machine, r.URL.Query().Get("launcher_id"))
	if !ok {
		return
	}
	var folders []string
	var folderLabel, sessionID string
	if sessionID = r.URL.Query().Get("session_id"); sessionID != "" {
		if !ccsessions.ValidID(sessionID) {
			apiError(w, 400, "invalid_request", "session_id is not a Claude Code session id")
			return
		}
		// Looking an id up scans no folder, but it is the project's business only where the project has a folder to compare it with.
		if mapped, err := s.Store.ProjectMachineFolder(r.Context(), projectID, machine.ID); err != nil {
			fail(w, err)
			return
		} else if mapped == "" {
			apiError(w, 409, "machine_folder_missing", "set this project's folder on "+machine.Name)
			return
		}
	} else {
		configured, err := s.Store.ProjectMachineFolder(r.Context(), projectID, machine.ID)
		if err != nil {
			fail(w, err)
			return
		}
		canonical, err := s.selectedMachinePath(r.Context(), projectID, machine)
		if errors.Is(err, errMissingMachineFolder) {
			apiError(w, 409, "machine_folder_missing", "set this project's folder on "+machine.Name)
			return
		}
		if err != nil {
			apiError(w, 409, "folder_unverified", err.Error())
			return
		}
		folders, folderLabel = []string{canonical}, canonical
		if filepath.IsAbs(configured) && filepath.Clean(configured) != canonical {
			folders = append(folders, filepath.Clean(configured))
		}
	}
	reply, err := s.scanClaudeSessions(r.Context(), conn, launcherID, folders, sessionID)
	switch {
	case errors.Is(err, errSessionScanUnsupported):
		apiError(w, 409, "supervisor_update_needed", err.Error())
		return
	case errors.Is(err, errSessionNotFound):
		apiError(w, 404, "session_not_found", err.Error())
		return
	case errors.Is(err, errSessionUnreadable):
		apiError(w, 409, "session_unreadable", err.Error())
		return
	case err != nil:
		apiError(w, 503, "scan_failed", err.Error())
		return
	}
	found := reply.Sessions
	ids := make([]string, 0, len(found))
	for _, session := range found {
		ids = append(ids, session.ID)
	}
	agents, err := s.Store.ClaudeSessionAgents(r.Context(), machine.ID, ids)
	if err != nil {
		fail(w, err)
		return
	}
	// A session is inside the project when it worked in a folder the project has on this machine: the scanned spellings, or for an id
	// lookup (which scans no folder) the machine's mapped folder; on the host also the project's own folder.
	inside := folders
	if len(inside) == 0 {
		mapped, err := s.Store.ProjectMachineFolder(r.Context(), projectID, machine.ID)
		if err != nil {
			fail(w, err)
			return
		}
		inside = []string{mapped}
	}
	var hostFolder string
	if machine.IsHost {
		if hostFolder, err = s.Store.ProjectFolder(r.Context(), projectID); err != nil {
			fail(w, err)
			return
		}
	}
	views := make([]claudeSessionView, 0, len(found))
	for _, session := range found {
		view := claudeSessionView{SessionCandidate: session, OutsideFolder: !withinFolder(session.CWD, inside, hostFolder)}
		if agent, ok := agents[session.ID]; ok {
			view.Agent = &agent
		}
		views = append(views, view)
	}
	writeJSON(w, 200, map[string]any{"machine_id": machine.ID, "folder": folderLabel, "launcher_id": launcherID, "sessions": views, "unreadable": reply.Unreadable, "truncated": reply.ScanTruncated})
}

// claudeLauncher is the launcher whose Claude home a listing reads: the one named, or the machine's native claude launcher. A supervisor
// that predates launchers, or a machine with no native claude launcher yet, reads its own home (a listing changes nothing; starting an
// agent still needs a launcher). A launcher that is named but cannot be used is refused. It writes the error response itself.
func (s *Server) claudeLauncher(w http.ResponseWriter, r *http.Request, conn *machineConn, machine store.Machine, id string) (string, bool) {
	if !conn.supportsLaunchers {
		return "", true
	}
	launcher, err := s.resolveLauncher(r.Context(), machine.ID, "claude", "native", id)
	if err != nil && id == "" {
		return "", true
	}
	if err != nil || !launcher.Enabled {
		apiError(w, 409, "launcher_unavailable", errLauncherUnavailable.Error())
		return "", false
	}
	if launcherKeepsOwnSessions(&launcher, launcherAlias(launcher.Name)) && !sharesClaudeHome(conn, &launcher) {
		apiError(w, 409, "needs_native", sessionsMessage("claude", &launcher))
		return "", false
	}
	return launcher.ID, true
}

// withinFolder says a session's working folder is inside the project's folder (in any spelling that was scanned, or as configured on the
// server for the host machine).
func withinFolder(cwd string, scanned []string, configured string) bool {
	for _, folder := range append(append([]string(nil), scanned...), configured) {
		if folder == "" {
			continue
		}
		if folder = filepath.Clean(folder); filepath.IsAbs(folder) && (cwd == folder || strings.HasPrefix(cwd, folder+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// connectedMachine is an approved machine (the host by default) whose supervisor is connected now, whatever harness it offers.
func (s *Server) connectedMachine(ctx context.Context, id string) (*machineConn, store.Machine, error) {
	if id == "" {
		host, err := s.Store.HostMachine(ctx)
		if err != nil {
			return nil, store.Machine{}, err
		}
		id = host.ID
	}
	machine, err := s.Store.MachineByID(ctx, id)
	if err != nil {
		return nil, machine, err
	}
	if !machine.Approved || machine.RevokedAt.Valid {
		return nil, machine, ErrMachineUnavailable
	}
	conn, err := s.machineForID(id)
	if err != nil {
		return nil, machine, err
	}
	return conn, machine, nil
}

// resumableSession finds the session a spawn asks to continue on the chosen machine, by id (the client never supplies a folder), and
// refuses what cannot or should not be continued. It writes the error response itself.
func (s *Server) resumableSession(w http.ResponseWriter, r *http.Request, conn *machineConn, machine store.Machine, launcherID string, req *spawnRequest) (*protocol.SessionCandidate, bool) {
	if req.Tool != "claude" {
		apiError(w, 400, "tool_mismatch", "to continue a Claude Code session the tool must be claude")
		return nil, false
	}
	reply, err := s.scanClaudeSessions(r.Context(), conn, launcherID, nil, req.ResumeSession)
	found := reply.Sessions
	switch {
	case errors.Is(err, errSessionUnreadable):
		apiError(w, 409, "session_unreadable", err.Error())
		return nil, false
	case errors.Is(err, errSessionScanUnsupported):
		apiError(w, 409, "supervisor_update_needed", err.Error())
		return nil, false
	case errors.Is(err, errSessionNotFound) || err == nil && len(found) != 1:
		apiError(w, 404, "session_not_found", "the Claude Code session "+req.ResumeSession+" was not found on "+machine.Name)
		return nil, false
	case err != nil:
		apiError(w, 503, "scan_failed", err.Error())
		return nil, false
	}
	session := found[0]
	if session.ID != req.ResumeSession {
		apiError(w, 404, "session_not_found", "the Claude Code session "+req.ResumeSession+" was not found on "+machine.Name)
		return nil, false
	}
	if req.ResumeMode == resumeFork {
		// A copy leaves the original as it is, so a live process holding it open and an agent continuing it are no obstacle.
		if !conn.supportsSessionFork {
			apiError(w, 409, "supervisor_update_needed", "this machine's supervisor predates working on a copy of a session; update it, or continue the session instead")
			return nil, false
		}
		return &session, true
	}
	agents, err := s.Store.ClaudeSessionAgents(r.Context(), machine.ID, []string{session.ID})
	if err != nil {
		fail(w, err)
		return nil, false
	}
	if agent, ok := agents[session.ID]; ok {
		apiError(w, 409, "already_started", "this session is already continued by "+agent.Slug+"; use that agent")
		return nil, false
	}
	if session.Active && !req.ConfirmSessionClosed {
		apiError(w, 409, "session_active", "a Claude process has this session open; close it first (two processes on one session corrupt it), then confirm")
		return nil, false
	}
	return &session, true
}

// sessionFoundFrom says Claude, started in path, would find the session where it saved it. Claude looks a session up under a directory named
// after the folder it is started in; the session sits in the directory named after the spelling it was made in. When that spelling and path
// are different (a link), the names differ and the session is not found. Where the saved directory does not follow the naming rule at all
// (a hashed long path, another Claude version) nothing can be said, so it is let through and the harness decides.
func sessionFoundFrom(session protocol.SessionCandidate, path string) bool {
	if session.Dir == "" || session.Dir != ccsessions.FolderName(session.CWD) {
		return true
	}
	return session.Dir == ccsessions.FolderName(path)
}

// sameDirectory says a session's saved working folder (cwd, in the spelling it was made under) is the directory canonical, on the machine that
// has it: the host resolves it here; another machine already resolved it (the path probe that produced canonical took cwd itself), so there
// is nothing more to check from this side.
func sameDirectory(machine store.Machine, cwd, canonical string) bool {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return false
	}
	if !machine.IsHost {
		return true
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	return err == nil && filepath.Clean(resolved) == canonical
}

// sessionRoomTitle is the title of the room a continued session gets: its own title, or the start of its id when it has none.
func sessionRoomTitle(session protocol.SessionCandidate) string {
	if title, err := store.NormalizeRoomTitle(session.Title); err == nil && title != "" {
		return title
	}
	short := session.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return "Claude session " + short
}
