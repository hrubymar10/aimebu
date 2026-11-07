package server

import (
	"bytes"
	"net/http"
	"testing"
)

// The routes that need no login answer only a Host this server expects, so a page that has rebound its own name to the server's address cannot
// reach them; the routes behind a login are not affected.
func TestRoutesWithoutALoginAnswerOnlyAnExpectedHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.Auth.PublicURL = "https://aimebu.example.com"
	f.srv.AllowedHosts = []string{"mac.lan"}
	post := func(host, path string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, f.http.URL+path, bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	paths := []string{"/api/login", "/api/setup/token", "/api/setup", "/api/invites/redeem", "/api/invites/inspect", "/api/machine-pairings/redeem"}
	for _, path := range paths {
		for _, host := range []string{"evil.example", "evil.example:9997", "aimebu.example.com.evil.example", ""} {
			if got := post(host, path); got != http.StatusMisdirectedRequest && host != "" {
				t.Fatalf("POST %s with Host %q = %d, want 421", path, host, got)
			}
		}
		for _, host := range []string{"localhost", "localhost:9997", "127.0.0.1:9997", "[::1]:9997", "app.localhost", "192.168.1.5", "aimebu.example.com", "AIMEBU.example.com:443", "mac.lan"} {
			if got := post(host, path); got == http.StatusMisdirectedRequest {
				t.Fatalf("POST %s with Host %q was refused", path, host)
			}
		}
	}
	// Behind a login the Host does not matter: the session cookie is the credential, and the browser sends it only to the name it was set for.
	req, _ := http.NewRequest(http.MethodGet, f.http.URL+"/api/me", nil)
	req.Host = "evil.example"
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an authenticated route with an unexpected Host = %d", resp.StatusCode)
	}
}

func TestResolveAllowedHostsTakesNamesOnly(t *testing.T) {
	got, err := ResolveAllowedHosts(" Mac.LAN, studio.local. ,,[fd00::1]")
	if err != nil || len(got) != 3 || got[0] != "mac.lan" || got[1] != "studio.local" || got[2] != "fd00::1" {
		t.Fatalf("hosts=%q err=%v", got, err)
	}
	for _, bad := range []string{"https://mac.lan", "mac.lan:9997", "mac.lan/path", "user@mac.lan"} {
		if _, err := ResolveAllowedHosts(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}
