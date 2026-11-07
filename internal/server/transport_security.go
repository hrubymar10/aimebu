package server

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// hstsValue tells a browser to use https for this host for a year after it has seen an https response from it, so a later plain-http link, or
// an attacker on the network downgrading it, goes to https first. It is sent only on responses that travelled over https.
const hstsValue = "max-age=31536000"

// withHSTS adds Strict-Transport-Security to a response that was served over TLS on the https port (443), or by a proxy that terminated TLS for the
// public https URL, and never for localhost. A browser keeps the port when it upgrades a host, so on another port (9896, say) the pin would turn a
// later http://name:<plain port> into a TLS attempt against the plain listener, which fails instead of redirecting; and a pin on localhost would
// hold every local http service for a year.
func (s *Server) withHSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Auth.SecureRequest(r) && hstsFits(r) {
			w.Header().Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}

func hstsFits(r *http.Request) bool {
	host := requestHostName(r)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if r.TLS != nil {
		// Served by this process: only on the standard https port (no port in the Host header means 443).
		if _, port, err := net.SplitHostPort(r.Host); err == nil && port != "443" {
			return false
		}
	}
	return true
}

// RedirectPlainToHTTPS is the handler of the plain-http listener when https is configured: a request from another machine is sent to the same
// address on the https port, so nothing a person types or a supervisor sends crosses the network in the clear by default. A request from this
// machine itself (loopback, or from the server's own address, where the embedded supervisor and the command line connect) is served as it
// was: nothing there crosses a network. The agents' toolbox calls (/api/agent/...) and the machine routes (/api/machine..., a supervisor's socket,
// its pairing) are served as they are: a Docker agent on a Linux bridge reaches the server as host.docker.internal from a bridge address that is
// neither loopback nor the server's own, and a name and a certificate for it cannot be assumed; those callers authenticate with their key, and
// the supervisor refuses plain http to another machine on its own unless told otherwise. The redirect keeps the method (308), goes only to a host
// this server answers to, and is behind the allowlist, so a peer that may not connect gets the same 403 as anywhere else.
func (s *Server) RedirectPlainToHTTPS(next http.Handler) http.Handler {
	if s.TLSPort == 0 {
		return next
	}
	redirecting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || fromThisMachine(r) || strings.HasPrefix(r.URL.Path, "/api/agent/") || strings.HasPrefix(r.URL.Path, "/api/machine") {
			next.ServeHTTP(w, r)
			return
		}
		if !s.hostAllowed(r) {
			apiError(w, http.StatusMisdirectedRequest, "unexpected_host", "this server does not answer to the host name "+requestHostName(r))
			return
		}
		host := requestHostName(r)
		if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() { // keep an IPv6 literal bracketed
			host = "[" + host + "]"
		}
		target := "https://" + host + ":" + strconv.Itoa(s.TLSPort) + r.URL.RequestURI()
		if s.TLSPort == 443 {
			target = "https://" + host + r.URL.RequestURI()
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
	return allowMiddleware(redirecting, s.Allow)
}

// fromThisMachine says whether the connection came from the machine the server runs on: a loopback address, or the same address it was
// accepted on (a client that reaches the server through its own LAN address).
func fromThisMachine(r *http.Request) bool {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	if remote.Addr().Unmap().IsLoopback() {
		return true
	}
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if tcp, ok := local.(*net.TCPAddr); ok {
			if ip, ok := netip.AddrFromSlice(tcp.IP); ok {
				return ip.Unmap() == remote.Addr().Unmap()
			}
		}
	}
	return false
}
