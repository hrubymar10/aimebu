package server

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/auth"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func userError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "insufficient permission")
	case errors.Is(err, store.ErrLastOwner):
		apiError(w, 409, "last_owner", "a project needs an active owner")
	case errors.Is(err, store.ErrInviteInvalid):
		apiError(w, 401, "invalid_invite", "invite is invalid, expired, or already used")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "already_member", "user already belongs to this project")
	case errors.Is(err, store.ErrInvalidTransition):
		apiError(w, 400, "invalid_request", "invalid user, role, or password")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "user or project not found")
	default:
		fail(w, err)
	}
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	items, err := s.Store.ListUsersForManager(r.Context(), userID(r), projectID)
	if err != nil {
		userError(w, err)
		return
	}
	global, err := s.Store.IsGlobalOwner(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	admin, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	body := map[string]any{"users": items, "global_owner": global, "global_admin": admin}
	if admin && projectID == "" {
		// The owner and the admins manage people in every project, including ones they are not members of.
		all, err := s.Store.AllProjects(r.Context(), userID(r))
		if err != nil {
			fail(w, err)
			return
		}
		projects := make([]map[string]string, 0, len(all))
		for _, p := range all {
			projects = append(projects, map[string]string{"id": p.ID, "name": p.Name})
		}
		body["projects"] = projects
	}
	writeJSON(w, 200, body)
}

func (s *Server) createUserInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind         string `json:"kind"`
		ProjectID    string `json:"project_id"`
		Role         string `json:"role"`
		TargetUserID string `json:"target_user_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Kind == "" {
		req.Kind = "join"
	}
	invite, token, err := s.Auth.CreateUserInvite(r.Context(), userID(r), req.Kind, req.ProjectID, req.Role, req.TargetUserID)
	if err != nil {
		userError(w, err)
		return
	}
	link := s.publicBase(r) + "/#invite=" + url.QueryEscape(token)
	writeJSON(w, 201, map[string]any{"invite": invite, "token": token, "url": link})
}

// POST /api/invites/inspect: what an invitation link is for (join which project, or reset whose password), so the page can say it
// before asking for anything. The token is in the body, not the URL, and a wrong one counts against the same throttle as using it.
func (s *Server) inspectUserInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	info, err := s.Auth.InspectUserInvite(r.Context(), req.Token, s.clientIP(r))
	if errors.Is(err, auth.ErrRateLimited) {
		delay := s.Auth.InviteRetryAfter(req.Token, s.clientIP(r))
		seconds := int((delay + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		apiError(w, 429, "rate_limited", "rate limited")
		return
	}
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, 200, info)
}

func (s *Server) redeemUserInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var existingID string
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		if session, err := s.Auth.CheckSession(r.Context(), cookie.Value); err == nil {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRFToken)) != 1 {
				apiError(w, 403, "invalid_csrf", "invalid CSRF token")
				return
			}
			existingID = session.UserID
		}
	}
	invite, id, login, err := s.Auth.RedeemUserInvite(r.Context(), req.Token, req.Login, req.Password, existingID, s.clientIP(r))
	if errors.Is(err, auth.ErrRateLimited) {
		delay := s.Auth.InviteRetryAfter(req.Token, s.clientIP(r))
		seconds := int((delay + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		apiError(w, 429, "rate_limited", "rate limited")
		return
	}
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"user_id": id, "login": login, "kind": invite.Kind, "project_id": invite.ProjectID, "role": invite.Role})
}

func (s *Server) projectMember(w http.ResponseWriter, r *http.Request) {
	role := ""
	if r.Method == http.MethodPut {
		var req struct {
			Role string `json:"role"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		role = req.Role
	}
	err := s.Store.SetProjectMember(r.Context(), userID(r), r.PathValue("project"), r.PathValue("user"), role)
	if err != nil {
		userError(w, err)
		return
	}
	w.WriteHeader(204)
}

// PUT /api/users/{user}/admin {admin}: the owner gives or takes away the admin flag.
func (s *Server) setUserAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Admin *bool `json:"admin"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Admin == nil {
		apiError(w, 400, "invalid_request", "admin is required")
		return
	}
	if err := s.Store.SetUserAdmin(r.Context(), userID(r), r.PathValue("user"), *req.Admin); err != nil {
		userError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deactivateUser(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeactivateUser(r.Context(), userID(r), r.PathValue("user")); err != nil {
		userError(w, err)
		return
	}
	w.WriteHeader(204)
}

// publicBase is the address people reach this server at: the configured public URL, else the one the request came to.
func (s *Server) publicBase(r *http.Request) string {
	if s.Auth.PublicURL != "" {
		return s.Auth.PublicURL
	}
	scheme, host := "http", r.Host
	if r.TLS != nil || s.TLSPort != 0 {
		scheme = "https"
	}
	if r.TLS == nil && s.TLSPort != 0 {
		name, _, err := net.SplitHostPort(host)
		if err != nil {
			name = host
		}
		host = net.JoinHostPort(name, strconv.Itoa(s.TLSPort))
	}
	return scheme + "://" + host
}
