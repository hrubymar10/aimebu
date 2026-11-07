package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// A page on another site can make the person's browser send requests to this server under the attacker's own name by pointing that name at
// the server's address after the page has loaded (DNS rebinding). The browser then treats them as the same origin, and the page reads
// the answers. What it cannot change is the Host header: it carries the attacker's name. So the routes that need no login (signing in, the
// first-run setup, an invite, a machine's pairing) answer only a request whose Host is one this server expects: localhost, an IP address
// (a name is what rebinding needs), the host of AIMEBU2_PUBLIC_URL, and the names listed in AIMEBU2_ALLOWED_HOSTS.

// ResolveAllowedHosts reads a comma-separated list of host names (no scheme, port or path), lowercased.
func ResolveAllowedHosts(raw string) ([]string, error) {
	var hosts []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if strings.ContainsAny(item, "/:@ ") && net.ParseIP(strings.Trim(item, "[]")) == nil {
			return nil, &net.ParseError{Type: "host name (no scheme, port or path)", Text: item}
		}
		hosts = append(hosts, strings.TrimSuffix(strings.Trim(item, "[]"), "."))
	}
	return hosts, nil
}

// requestHostName is the Host header without its port, brackets and trailing dot, lowercased.
func requestHostName(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func (s *Server) hostAllowed(r *http.Request) bool {
	host := requestHostName(r)
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if public := s.Auth.PublicURL; public != "" {
		if parsed, err := parsePublicHost(public); err == nil && host == parsed {
			return true
		}
	}
	for _, allowed := range s.AllowedHosts {
		if host == allowed {
			return true
		}
	}
	return false
}

func parsePublicHost(public string) (string, error) {
	request, err := http.NewRequest(http.MethodGet, public, nil)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSuffix(request.URL.Hostname(), ".")), nil
}

// hostChecked wraps a route that needs no login so it answers only a request for a host this server expects; anything else is 421 and says
// how to add the name.
func (s *Server) hostChecked(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r) {
			apiError(w, http.StatusMisdirectedRequest, "unexpected_host", "this server does not answer to the host name "+requestHostName(r)+"; reach it by localhost, an IP address, or its public URL, or list the name in AIMEBU2_ALLOWED_HOSTS")
			return
		}
		next(w, r)
	}
}
