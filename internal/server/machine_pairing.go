package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/auth"
	"github.com/hrubymar10/aimebu/v2/internal/buildversion"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/tlsclient"
	"github.com/hrubymar10/aimebu/v2/internal/worktree"
)

type machineRow struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	IsHost            bool     `json:"is_host"`
	ScopeMode         string   `json:"scope_mode"`
	AllowedProjects   []string `json:"allowed_project_ids"`
	Disabled          bool     `json:"disabled"`
	Approved          bool     `json:"approved"`
	RevokedAt         string   `json:"revoked_at,omitempty"`
	LastSeen          string   `json:"last_seen,omitempty"`
	SupervisorVersion string   `json:"supervisor_version,omitempty"`
	ServerVersion     string   `json:"server_version,omitempty"`
	OlderBuild        bool     `json:"older_build"`
	Connected         bool     `json:"connected"`
	RestartPending    bool     `json:"restart_pending,omitempty"`
	UpgradeRequired   bool     `json:"upgrade_required,omitempty"`
	ProjectAllowed    bool     `json:"project_allowed,omitempty"`
	// ProjectWorktree says whether a task room can have a worktree in the project folder: the check the server makes when a worktree is
	// requested, with the reason when it cannot. Only the host machine can be asked from here, and only when a project is named; absent
	// means not known.
	ProjectWorktree *worktreeCheck `json:"project_worktree,omitempty"`
	// Warm is what the machine's supervisor last said about the agent processes it keeps warm (only for a connected one that keeps any): how many
	// are live and what they use. The memory is not per agent: RSSBytes covers the processes the supervisor runs itself, ContainerWideBytes every
	// running container (Docker reports no finer, and the harness container is shared by all agents).
	Warm *machineWarm `json:"warm,omitempty"`
}

type machineWarm struct {
	Idle                int   `json:"idle"`
	Busy                int   `json:"busy"`
	Started             int   `json:"started"`
	Reused              int   `json:"reused"`
	UnconfirmedPingEnds int   `json:"unconfirmed_ping_ends,omitempty"`
	RSSBytes            int64 `json:"rss_bytes"`
	ContainerWideBytes  int64 `json:"container_wide_bytes"`
}

func (s *Server) ownerMachine(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.Store.IsGlobalOwner(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return false
	}
	if !ok {
		apiError(w, 403, "forbidden", "owner required")
		return false
	}
	return true
}

// errMachineNotAdmitted is the refusal to work on another machine's copy of a project before the owner has admitted that machine for the project.
var errMachineNotAdmitted = errors.New("this machine is not admitted for the project yet: the project's owner admits it (POST /api/projects/{project}/machine-folders/{machine}/admit, or by approving the first agent that asks to run there)")

// machineAdmitted says whether a project may be worked on at a machine by the person the request is from: the host always, another machine once
// the owner admitted it for the project, and for the project's owner and the admins always (admitting it is their business, so their own
// request does not wait for an approval nobody else could give). What reads or writes a project's folder on a machine does so only when this holds.
func (s *Server) machineAdmitted(ctx context.Context, projectID string, machine store.Machine) (bool, error) {
	if err := s.Store.CheckMachineProject(ctx, projectID, machine.ID); err != nil {
		return false, err
	}
	if machine.IsHost {
		return true, nil
	}
	if allowed, err := s.Store.ProjectMachineAllowed(ctx, projectID, machine.ID); err != nil || allowed {
		return allowed, err
	}
	user, _ := auth.UserIDFromContext(ctx)
	if user == "" {
		return false, nil
	}
	if admin, err := s.Store.IsGlobalAdmin(ctx, user); err != nil || admin {
		return admin, err
	}
	role, err := s.Store.UserProjectRole(ctx, user, projectID)
	if errors.Is(err, store.ErrForbidden) {
		return false, nil
	}
	return role == "owner", err
}

// POST /api/projects/{project}/machine-folders/{machine}/admit: the project owner (or an admin) admits a paired machine for the project, so its
// folder there is worked on, without having to start an agent to raise the approval.
func (s *Server) machineAdmit(w http.ResponseWriter, r *http.Request) {
	err := s.Store.AdmitProjectMachine(r.Context(), r.PathValue("project"), r.PathValue("machine"), userID(r))
	switch {
	case errors.Is(err, store.ErrMachineScope):
		apiError(w, 409, "machine_scope", err.Error())
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "project owner required")
	case errors.Is(err, store.ErrInvalidTransition):
		apiError(w, 409, "invalid_transition", "the machine must be approved and have a folder mapped for this project")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "machine not found")
	case err != nil:
		fail(w, err)
	default:
		w.WriteHeader(204)
	}
}

// adminMachine allows the owner and the admins the owner appointed: the reach over people, projects and global settings. Things that
// run code on the host or hold provider logins stay behind ownerMachine.
func (s *Server) adminMachine(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return false
	}
	if !ok {
		apiError(w, 403, "forbidden", "admin required")
		return false
	}
	return true
}

func (s *Server) machineRow(m store.Machine) machineRow {
	s.machineMu.Lock()
	conn, connected := s.machines[m.ID]
	s.machineMu.Unlock()
	if connected {
		select {
		case <-conn.done:
			connected = false
		default:
		}
	}
	if m.RevokedAt.Valid {
		connected = false
	}
	allowed := m.AllowedProjects
	if allowed == nil {
		allowed = []string{}
	}
	row := machineRow{ID: m.ID, Name: m.Name, IsHost: m.IsHost, ScopeMode: m.ScopeMode, AllowedProjects: allowed, Disabled: m.Disabled, Approved: m.Approved, RevokedAt: m.RevokedAt.String,
		LastSeen: m.LastSeen.String, SupervisorVersion: m.SupervisorVersion.String, ServerVersion: s.Version,
		OlderBuild: buildversion.Older(m.SupervisorVersion.String, s.Version), Connected: connected}
	if m.ID == s.localSupervisorID() {
		row.RestartPending = s.localSupervisorPending.Load()
		row.UpgradeRequired = s.localSupervisorUpgradeRequired.Load()
	}
	if connected && conn.supportsWarm {
		conn.warmMu.Lock()
		report := conn.warm
		conn.warmMu.Unlock()
		if report != nil {
			warm := &machineWarm{Started: report.Started, Reused: report.Reused, UnconfirmedPingEnds: report.UnconfirmedPingEnds, RSSBytes: report.RSSBytes, ContainerWideBytes: report.ContainerWideBytes}
			for _, agent := range report.Agents {
				if agent.State == "busy" {
					warm.Busy++
				} else {
					warm.Idle++
				}
			}
			row.Warm = warm
		}
	}
	return row
}

// machineList is read-only: the owner and the admins see every machine (they run the harnesses on them), anyone else the approved machines
// of a project they belong to. Pairing, approving and revoking stay with the owner.
func (s *Server) machineList(w http.ResponseWriter, r *http.Request) {
	owner, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	if !owner {
		if projectID == "" {
			apiError(w, 403, "forbidden", "project access required")
			return
		}
		if err := s.Store.ProjectAccess(r.Context(), userID(r), projectID); err != nil {
			if errors.Is(err, store.ErrForbidden) {
				apiError(w, 403, "forbidden", "project access required")
			} else {
				fail(w, err)
			}
			return
		}
	}
	items, err := s.Store.Machines(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	rows := make([]machineRow, 0, len(items))
	for _, m := range items {
		if projectID != "" && (!m.Approved || m.RevokedAt.Valid) {
			continue
		}
		if projectID != "" {
			if err := s.Store.CheckMachineProject(r.Context(), projectID, m.ID); errors.Is(err, store.ErrMachineScope) {
				continue
			} else if err != nil {
				fail(w, err)
				return
			}
		}
		row := s.machineRow(m)
		if projectID != "" {
			row.ProjectAllowed = m.IsHost
			if m.IsHost {
				if folder, err := s.Store.ProjectFolder(r.Context(), projectID); err == nil {
					row.ProjectWorktree = checkWorktree(r.Context(), folder)
				}
			}
			if !m.IsHost {
				row.ProjectAllowed, err = s.Store.ProjectMachineAllowed(r.Context(), projectID, m.ID)
				if err != nil {
					fail(w, err)
					return
				}
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, 200, map[string]any{"machines": rows})
}

func (s *Server) machineScope(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	var req struct {
		ScopeMode       string   `json:"scope_mode"`
		AllowedProjects []string `json:"allowed_project_ids"`
		Disabled        bool     `json:"disabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	before, err := s.Store.MachineByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.Store.SetMachineScope(r.Context(), id, req.ScopeMode, req.AllowedProjects, req.Disabled); err != nil {
		switch {
		case errors.Is(err, store.ErrMachineScopeActive):
			apiError(w, 409, "machine_scope_active", err.Error())
		case errors.Is(err, store.ErrInvalidTransition):
			apiError(w, 400, "invalid_request", "invalid project list or disabled on a remote machine")
		default:
			fail(w, err)
		}
		return
	}
	if before.IsHost && before.Disabled != req.Disabled {
		if req.Disabled && s.LocalSupervisorDisable != nil {
			s.LocalSupervisorDisable()
		}
		if !req.Disabled && s.LocalSupervisorEnable != nil {
			if err := s.LocalSupervisorEnable(); err != nil {
				fail(w, err)
				return
			}
		}
	}
	after, err := s.Store.MachineByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, s.machineRow(after))
}

func (s *Server) machinePairing(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	token, expires, err := s.Auth.CreateMachinePairing(r.Context(), req.Name, userID(r))
	if errors.Is(err, auth.ErrInvalidCredentials) {
		apiError(w, 400, "invalid_request", "machine name required (at most 100 characters)")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	// Behind a reverse proxy the request is plain HTTP on loopback, so only the
	// configured public URL names the origin a remote supervisor can reach.
	url := s.Auth.PublicURL
	if url == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		url = scheme + "://" + r.Host
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
	answer := map[string]any{"token": token, "expires_at": expires}
	command := "aimebu supervisor --pair " + quote(url) + " " + quote(token)
	if tlsclient.PlainHTTPToARemoteHost(url) {
		// The supervisor refuses this origin unless it is told to accept it, so the command says so, and the dialog says why that is a choice to make.
		answer["insecure_transport"] = true
		answer["warning"] = "This server is reached over plain http, so the pairing token, the machine key it returns and everything the agents on that machine say would cross the network unencrypted. Serve it over https (docs/tls.md) to avoid that; the command below has --insecure only because it will not run without it."
		command = "aimebu supervisor --insecure --pair " + quote(url) + " " + quote(token)
	}
	answer["command"] = command
	writeJSON(w, 201, answer)
}

func (s *Server) redeemMachinePairing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	m, key, err := s.Auth.RedeemMachinePairing(r.Context(), req.Token, s.clientIP(r))
	if errors.Is(err, auth.ErrRateLimited) {
		delay := s.Auth.PairingRetryAfter(req.Token, s.clientIP(r))
		seconds := int((delay + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		apiError(w, 429, "rate_limited", "rate limited")
		return
	}
	if errors.Is(err, auth.ErrInvalidCredentials) {
		apiError(w, 401, "invalid_token", "invalid or expired pairing token")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"machine_id": m.ID, "machine_key": key, "approved": false})
}

func (s *Server) machinePairStatus(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		apiError(w, 401, "unauthorized", "machine credential required")
		return
	}
	m, err := s.Auth.MachinePairingStatus(r.Context(), r.PathValue("id"), strings.TrimPrefix(authz, "Bearer "))
	if err != nil {
		apiError(w, 401, "unauthorized", "invalid machine credential")
		return
	}
	answer := map[string]any{"approved": m.Approved, "name": m.Name}
	if m.Approved {
		keys, err := s.providerKeyRefs(r.Context(), m.ID)
		if err != nil {
			fail(w, err)
			return
		}
		if len(keys) > 0 {
			answer["provider_keys"] = keys
		}
	}
	writeJSON(w, 200, answer)
}

// providerKeyRef names a key variable a custom provider of one of this machine's OpenCode harnesses is read from, with the harness's
// environment references (variable names only), which say where that variable takes its value. `aimebu doctor --machine` asks for these
// with the machine's own key and checks them against the environment it runs in. Never a value.
type providerKeyRef struct {
	Launcher   string            `json:"launcher"`
	ProviderID string            `json:"provider_id"`
	KeyEnv     string            `json:"key_env"`
	EnvRefs    map[string]string `json:"env_refs,omitempty"`
}

func (s *Server) providerKeyRefs(ctx context.Context, machineID string) ([]providerKeyRef, error) {
	launchers, err := s.Store.MachineLaunchers(ctx, machineID)
	if err != nil {
		return nil, err
	}
	providers, err := s.Store.MachineHarnessProviders(ctx, machineID)
	if err != nil {
		return nil, err
	}
	var refs []providerKeyRef
	for _, launcher := range launchers {
		if launcher.Tool != "opencode" || !launcher.Enabled {
			continue
		}
		for _, p := range providers[launcher.ID] {
			if p.KeyEnv != "" {
				refs = append(refs, providerKeyRef{Launcher: launcher.Label, ProviderID: p.ID, KeyEnv: p.KeyEnv, EnvRefs: launcher.EnvRefs})
			}
		}
	}
	return refs, nil
}

func (s *Server) machineApproval(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := s.Store.ApproveMachine(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 404, "not_found", "pending machine not found")
		} else {
			fail(w, err)
		}
		return
	}
	m, err := s.Store.MachineByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, s.machineRow(m))
}

func (s *Server) machineRevocation(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	id := r.PathValue("id")
	m, err := s.Store.MachineByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 404, "not_found", "machine not found")
		} else {
			fail(w, err)
		}
		return
	}
	if m.IsHost {
		apiError(w, 409, "invalid_transition", "host machine cannot be revoked")
		return
	}
	if err := s.Store.RevokeMachine(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 409, "invalid_transition", "machine already revoked")
		} else {
			fail(w, err)
		}
		return
	}
	for _, conn := range s.machineConnections(id) {
		conn.stop()
	}
	s.forgetWarmSeen(r.Context(), id)
	m, err = s.Store.MachineByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, s.machineRow(m))
}

func (s *Server) machineFolders(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := s.Store.ProjectAccess(r.Context(), userID(r), project); err != nil {
		apiError(w, 403, "forbidden", "project access required")
		return
	}
	items, err := s.Store.ProjectMachineFolders(r.Context(), project)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"folders": items})
}

func (s *Server) machineFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folder string `json:"folder"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !filepath.IsAbs(req.Folder) || filepath.Clean(req.Folder) != req.Folder {
		apiError(w, 400, "invalid_request", "absolute clean folder path required")
		return
	}
	item, err := s.Store.SetProjectMachineFolder(r.Context(), r.PathValue("project"), r.PathValue("machine"), userID(r), req.Folder)
	if errors.Is(err, store.ErrMachineScope) {
		apiError(w, 409, "machine_scope", err.Error())
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		apiError(w, 403, "forbidden", "project owner or operator required")
		return
	}
	if errors.Is(err, store.ErrInvalidTransition) {
		apiError(w, 409, "invalid_transition", "host folder is the project folder")
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, item)
}

// worktreeCheck is whether a project folder can have worktrees, with the code and words of the refusal when it cannot.
type worktreeCheck struct {
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// checkWorktree asks the same question the server asks when a worktree is requested (worktree.Validate), so the dialog and the request
// agree: a folder inside a repository, or one git does not trust, is refused here too, with git's own reason. It is nil when the answer
// cannot be had in time, so a caller never refuses something on a guess.
func checkWorktree(ctx context.Context, folder string) *worktreeCheck {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := worktree.Validate(ctx, folder)
	if err == nil {
		return &worktreeCheck{OK: true}
	}
	if ctx.Err() != nil {
		return nil
	}
	var problem *worktree.Error
	if errors.As(err, &problem) {
		return &worktreeCheck{Code: problem.Code, Reason: problem.Error()}
	}
	return nil
}
