package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
)

func firstRunOf(t *testing.T, resp *http.Response) bool {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	value, ok := body["first_run"].(bool)
	if !ok {
		t.Fatalf("first_run missing or not a boolean: %v", body)
	}
	return value
}

func TestFirstRunWelcomeIsForTheOwnerUntilDismissed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	// A new install: the owner has not seen the welcome; nobody else is ever shown it.
	if !firstRunOf(t, f.request("GET", "/api/me", nil, false)) {
		t.Fatal("the owner of a new install should get the welcome")
	}
	if firstRunOf(t, f.requestAs(t, cookie, csrf, "GET", "/api/me", nil)) {
		t.Fatal("a person who is not the owner never gets it")
	}
	// Only the owner can dismiss it, with CSRF.
	resp := f.requestAs(t, cookie, csrf, "POST", "/api/welcome/seen", map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("non-owner dismiss: %d", resp.StatusCode)
	}
	resp = f.request("POST", "/api/welcome/seen", map[string]any{}, false)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF: %d", resp.StatusCode)
	}
	if !firstRunOf(t, f.request("GET", "/api/me", nil, false)) {
		t.Fatal("a refused dismissal must not count")
	}
	resp = f.request("POST", "/api/welcome/seen", map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("owner dismiss: %d", resp.StatusCode)
	}
	if firstRunOf(t, f.request("GET", "/api/me", nil, false)) {
		t.Fatal("the welcome came back after it was dismissed")
	}
	// Dismissing twice is harmless.
	resp = f.request("POST", "/api/welcome/seen", map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("second dismiss: %d", resp.StatusCode)
	}
}

func TestLoginReportsFirstRunToTheOwner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	resp := f.request("POST", "/api/login", map[string]string{"login": "owner", "password": "correct horse battery staple"}, false)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if !firstRunOf(t, resp) {
		t.Fatal("the login response tells the owner of a new install to show the welcome")
	}
}

// The folder fields of the first run start at the host's home folder; only the people who create projects are told it.
func TestHostHomeIsToldToAdminsOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	hostHome := func(resp *http.Response) (string, bool) {
		t.Helper()
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		value, ok := body["host_home"].(string)
		return value, ok
	}
	home, ok := hostHome(f.request("GET", "/api/me", nil, false))
	if !ok || !filepath.IsAbs(home) {
		t.Fatalf("the owner should be told the host home folder, got %q %v", home, ok)
	}
	if want, err := os.UserHomeDir(); err != nil || home != want {
		t.Fatalf("host_home = %q, want the server's home %q (%v)", home, want, err)
	}
	if home, ok := hostHome(f.requestAs(t, cookie, csrf, "GET", "/api/me", nil)); ok {
		t.Fatalf("a person who cannot create projects must not be told a path of the host, got %q", home)
	}
}
