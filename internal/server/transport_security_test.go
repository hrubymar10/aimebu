package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func transportServer() *Server {
	s := New(nil)
	s.TLSPort = 9996
	s.AllowedHosts = []string{"mac.lan"}
	// The test peers: another machine on the LAN, and a Docker bridge.
	s.Allow = append(s.Allow, netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("172.17.0.0/16"))
	return s
}

// With https configured the plain listener sends another machine to it, with the method and the whole address kept, and serves this machine's
// own clients as they were.
func TestPlainListenerRedirectsOtherMachinesToHTTPS(t *testing.T) {
	t.Parallel()
	s := transportServer()
	served := false
	handler := s.RedirectPlainToHTTPS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served = true; w.WriteHeader(204) }))
	do := func(remote, host, target string) *httptest.ResponseRecorder {
		served = false
		req := httptest.NewRequest(http.MethodPost, "http://"+host+target, nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	rec := do("203.0.113.9:5555", "mac.lan:9997", "/api/login?x=1")
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://mac.lan:9996/api/login?x=1" || served {
		t.Fatalf("a remote client: %d %q served=%v", rec.Code, rec.Header().Get("Location"), served)
	}
	if rec := do("203.0.113.9:5555", "[2001:db8::1]:9997", "/"); rec.Header().Get("Location") != "https://[2001:db8::1]:9996/" {
		t.Fatalf("an IPv6 host: %q", rec.Header().Get("Location"))
	}
	if rec := do("203.0.113.9:5555", "evil.example", "/"); rec.Code != http.StatusMisdirectedRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("a host this server does not answer to is not redirected to: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, remote := range []string{"127.0.0.1:5555", "[::1]:5555", "[::ffff:127.0.0.1]:5555"} {
		if rec := do(remote, "localhost:9997", "/api/x"); rec.Code != 204 || !served {
			t.Fatalf("%s is this machine and is served as it was: %d served=%v", remote, rec.Code, served)
		}
	}
	// Without https configured nothing is redirected.
	plain := New(nil)
	called := false
	plain.RedirectPlainToHTTPS(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://mac.lan/", nil))
	if !called {
		t.Fatal("a server without https redirects")
	}
}

// What cannot be redirected is served as it is: a Docker agent on a Linux bridge calls /api/agent/... as host.docker.internal from a bridge address that
// is neither loopback nor the server's own, a supervisor connects to /api/machine..., and neither can be assumed to hold a name the certificate covers.
func TestAgentAndMachineRoutesAreNotRedirected(t *testing.T) {
	t.Parallel()
	s := transportServer()
	served := ""
	handler := s.RedirectPlainToHTTPS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served = r.URL.Path; w.WriteHeader(204) }))
	for _, path := range []string{"/api/agent/mcp", "/api/agent/turns/t1/attachments/a1", "/api/machine", "/api/machines/m1/pair-status", "/api/machine-pairings/redeem"} {
		served = ""
		req := httptest.NewRequest(http.MethodPost, "http://host.docker.internal:9897"+path, nil)
		req.RemoteAddr = "172.17.0.2:41000" // a bridge peer: not loopback, not the server's own address
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 204 || served != path {
			t.Fatalf("%s from a bridge address under an unlisted host: %d %q served=%q", path, rec.Code, rec.Header().Get("Location"), served)
		}
	}
	// The rest of the API from the same peer is still sent to https (or refused for an unlisted host).
	req := httptest.NewRequest(http.MethodGet, "http://host.docker.internal:9897/api/rooms", nil)
	req.RemoteAddr = "172.17.0.2:41000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("a page route from the bridge under an unlisted host: %d", rec.Code)
	}
}

// A peer that may not connect is refused before it can learn that there is a TLS port: the allowlist comes before the redirect.
func TestPlainListenerRefusesAPeerOutsideTheAllowlistBeforeRedirecting(t *testing.T) {
	t.Parallel()
	s := transportServer()
	handler := s.RedirectPlainToHTTPS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	req := httptest.NewRequest(http.MethodGet, "http://mac.lan:9897/", nil)
	req.RemoteAddr = "198.51.100.9:5000" // not in AIMEBU2_ALLOW
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" {
		t.Fatalf("a peer outside the allowlist: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// Responses over https say so to the browser, on the https port only and never for localhost; responses over plain http do not.
func TestHSTSIsSentOnlyOverHTTPSOnTheStandardPort(t *testing.T) {
	t.Parallel()
	s := transportServer()
	handler := s.withHSTS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	over := func(host string, tlsState *tls.ConnectionState) string {
		req := httptest.NewRequest("GET", "https://"+host+"/", nil)
		req.TLS = tlsState
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Header().Get("Strict-Transport-Security")
	}
	if got := over("mac.lan", &tls.ConnectionState{}); got != hstsValue {
		t.Fatalf("https on the standard port: %q", got)
	}
	if got := over("mac.lan:443", &tls.ConnectionState{}); got != hstsValue {
		t.Fatalf("https on port 443: %q", got)
	}
	for _, host := range []string{"mac.lan:9996", "localhost", "localhost:443", "app.localhost"} {
		if got := over(host, &tls.ConnectionState{}); got != "" {
			t.Fatalf("%s: HSTS %q would pin a port or a local name", host, got)
		}
	}
	if got := over("mac.lan", nil); got != "" {
		t.Fatalf("over plain http: %q", got)
	}
	// Behind a proxy that terminates TLS for the public https URL, the response is https as far as the browser knows.
	s.Auth.PublicURL = "https://aimebu.example.com"
	if got := over("aimebu.example.com", nil); got != hstsValue {
		t.Fatalf("behind the https proxy: %q", got)
	}
}
