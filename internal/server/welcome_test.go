package server

import (
	"net/http"
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
