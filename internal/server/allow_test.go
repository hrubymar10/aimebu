package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestResolveAllowDefaultsAndNormalizes(t *testing.T) {
	t.Parallel()
	defaults, err := ResolveAllow("")
	if err != nil || len(defaults) != 2 || defaults[0].String() != "127.0.0.0/8" || defaults[1].String() != "::1/128" {
		t.Fatalf("default allowlist=%v %v", defaults, err)
	}
	prefixes, err := ResolveAllow("127.0.0.1, 172.28.47.62,172.28.47.19/24,::1,127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"127.0.0.1/32", "172.28.47.62/32", "172.28.47.0/24", "::1/128"}
	if len(prefixes) != len(want) {
		t.Fatalf("prefixes=%v", prefixes)
	}
	for i, prefix := range prefixes {
		if prefix.String() != want[i] {
			t.Fatalf("prefix[%d]=%s want %s", i, prefix, want[i])
		}
	}
}

func TestResolveAllowRejectsMalformedEntries(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"127.0.0.1,", ",127.0.0.1", "127.0.0.1,,::1", "localhost", "10.0.0.0/33", "::ffff:192.0.2.0/120", " "} {
		if _, err := ResolveAllow(raw); err == nil {
			t.Fatalf("accepted malformed allowlist %q", raw)
		}
	}
}

func TestAllowMiddlewareUsesRemoteAddressForEveryRoute(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	handler := f.srv.Handler()
	for _, test := range []struct {
		path, remote, forwarded string
		want                    int
	}{
		{path: "/", remote: "10.0.0.1:1234", want: 403},
		{path: "/api/machine", remote: "10.0.0.1:1234", forwarded: "127.0.0.1", want: 403},
		{path: "/api/machine", remote: "127.3.4.5:1234", forwarded: "10.0.0.1", want: 401},
		{path: "/api/machine", remote: "[::ffff:127.0.0.1]:1234", want: 401},
		{path: "/api/machine", remote: "[::1]:1234", want: 401},
		{path: "/api/machine", remote: "unparseable", want: 403},
	} {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		req.RemoteAddr = test.remote
		if test.forwarded != "" {
			req.Header.Set("X-Forwarded-For", test.forwarded)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != test.want {
			t.Fatalf("%s from %s (forwarded %s): %d want %d", test.path, test.remote, test.forwarded, response.Code, test.want)
		}
	}
}

func TestAllowMiddlewareAcceptsConfiguredNetwork(t *testing.T) {
	t.Parallel()
	prefixes := []netip.Prefix{netip.MustParsePrefix("172.28.47.0/24")}
	hit := false
	handler := allowMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	}), prefixes)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.28.47.62:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent || !hit {
		t.Fatalf("configured network response=%d hit=%t", response.Code, hit)
	}
}

func TestClientIPUsesForwardedForOnlyFromTrustedProxies(t *testing.T) {
	t.Parallel()
	trusted, err := ResolveTrustedProxies("127.0.0.0/8, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	if none, err := ResolveTrustedProxies(""); err != nil || none != nil {
		t.Fatalf("empty trusted proxies=%v %v", none, err)
	}
	if _, err := ResolveTrustedProxies("10.0.0.0/8,,"); err == nil {
		t.Fatal("malformed trusted proxies accepted")
	}
	s := &Server{TrustedProxies: trusted}
	for _, tc := range []struct {
		remote string
		xff    []string
		want   string
	}{
		{"127.0.0.1:5000", nil, "127.0.0.1"},
		{"127.0.0.1:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"127.0.0.1:5000", []string{"203.0.113.66, 198.51.100.7, 10.0.0.2"}, "198.51.100.7"},
		{"127.0.0.1:5000", []string{"203.0.113.66", "198.51.100.7"}, "198.51.100.7"},
		{"127.0.0.1:5000", []string{"10.0.0.3, 127.0.0.1"}, "127.0.0.1"},
		{"127.0.0.1:5000", []string{"203.0.113.66, garbage"}, "127.0.0.1"},
		{"[::ffff:127.0.0.1]:5000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"192.0.2.9:5000", []string{"198.51.100.7"}, "192.0.2.9"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
		r.RemoteAddr = tc.remote
		for _, value := range tc.xff {
			r.Header.Add("X-Forwarded-For", value)
		}
		if got := s.clientIP(r); got != tc.want {
			t.Errorf("remote %s xff %v = %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := (&Server{}).clientIP(r); got != "127.0.0.1" {
		t.Fatalf("X-Forwarded-For honoured without trusted proxies: %s", got)
	}
}

func TestAllowlistIgnoresForwardedForFromTrustedProxy(t *testing.T) {
	t.Parallel()
	handler := allowMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "192.0.2.10")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("allowlist used X-Forwarded-For: %d", w.Code)
	}
}
