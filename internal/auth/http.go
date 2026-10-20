package auth

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/goccy/go-json"
)

const SessionCookieName = "aimebu_session"

type userContextKey struct{}

func UserIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userContextKey{}).(string)
	return id, ok
}

func (service *Service) SessionCookie(token string, secure bool) *http.Cookie {
	return &http.Cookie{Name: SessionCookieName, Value: token,
		Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure,
		MaxAge: int(service.SessionTTL.Seconds()), Expires: service.Now().UTC().Add(service.SessionTTL)}
}

// RequireUser validates the session and checks CSRF on every unsafe method.
func (service *Service) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil {
			writeHTTPError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		session, err := service.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			writeHTTPError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRFToken)) != 1 {
			writeHTTPError(w, http.StatusForbidden, "invalid_csrf", "invalid CSRF token")
			return
		}
		http.SetCookie(w, service.SessionCookie(cookie.Value, service.SecureRequest(r)))
		ctx := context.WithValue(r.Context(), userContextKey{}, session.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeHTTPError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// SecureRequest reports whether the browser reached aimebu over HTTPS: either
// this listener terminated TLS, or the request is addressed to the configured
// HTTPS public origin (a reverse proxy terminated TLS and forwarded the Host).
// A plain-HTTP request to any other host, such as a LAN IP, is not secure:
// browsers drop Secure cookies there and sign-in would silently fail.
func (service *Service) SecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	public := service.PublicOrigin()
	return strings.HasPrefix(public, "https://") && normalizedOrigin("https", r.Host) == public
}

// PublicOrigin returns the configured public URL as a browser would send it in
// an Origin header: lowercase scheme and host, default port omitted. It is
// empty when no public URL is configured.
func (service *Service) PublicOrigin() string {
	if service.PublicURL == "" {
		return ""
	}
	parsed, err := url.Parse(service.PublicURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return normalizedOrigin(parsed.Scheme, parsed.Host)
}

func normalizedOrigin(scheme, host string) string {
	scheme, host = strings.ToLower(scheme), strings.ToLower(host)
	if name, port, err := net.SplitHostPort(host); err == nil &&
		(scheme == "https" && port == "443" || scheme == "http" && port == "80") {
		host = name
		if strings.Contains(name, ":") {
			host = "[" + name + "]"
		}
	}
	return scheme + "://" + host
}

// CheckWSOrigin enforces a browser same-origin upgrade with a valid session.
// The origin must match this listener (scheme from r.TLS, host from r.Host)
// or the configured public origin that a reverse proxy serves.
func (service *Service) CheckWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	wantScheme := "http"
	if r.TLS != nil {
		wantScheme = "https"
	}
	sameListener := parsed.Scheme == wantScheme && strings.EqualFold(parsed.Host, r.Host)
	public := service.PublicOrigin()
	if !sameListener && (public == "" || normalizedOrigin(parsed.Scheme, parsed.Host) != public) {
		return false
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return false
	}
	_, err = service.Authenticate(r.Context(), cookie.Value)
	return err == nil
}
