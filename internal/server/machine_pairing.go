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

	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/worktree"
)

type machineRow struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	IsHost            bool   `json:"is_host"`
	Approved          bool   `json:"approved"`
	RevokedAt         string `json:"revoked_at,omitempty"`
	LastSeen          string `json:"last_seen,omitempty"`
	SupervisorVersion string `json:"supervisor_version,omitempty"`
	Connected         bool   `json:"connected"`
	ProjectAllowed    bool   `json:"project_allowed,omitempty"`
	// ProjectWorktree says whether a task room can have a worktree in the project folder: the check the server makes when a worktree is
	// requested, with the reason when it cannot. Only the host machine can be asked from here, and only when a project is named; absent
	// means not known.
	ProjectWorktree *worktreeCheck `json:"project_worktree,omitempty"`
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
	return machineRow{ID: m.ID, Name: m.Name, IsHost: m.IsHost, Approved: m.Approved, RevokedAt: m.RevokedAt.String,
		LastSeen: m.LastSeen.String, SupervisorVersion: m.SupervisorVersion.String, Connected: connected}
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
	writeJSON(w, 201, map[string]any{"token": token, "expires_at": expires, "command": "aimebu supervisor --pair " + quote(url) + " " + quote(token)})
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
	writeJSON(w, 200, map[string]any{"approved": m.Approved, "name": m.Name})
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
	s.machineMu.Lock()
	conn := s.machines[id]
	s.machineMu.Unlock()
	if conn != nil {
		conn.stop()
	}
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
