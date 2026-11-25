package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/auth"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// body reads a response for an assertion. A profile body is binary or text;
// only the status and, for the index, a name inside it are checked.
func debugPprofBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The pprof flag is process-wide environment, so these tests stay sequential:
// t.Setenv would race a parallel test's own reads of the environment.

func TestDebugPprofIsOffByDefault(t *testing.T) {
	t.Setenv(debugPprofEnv, "")
	f := newFixture(t)
	for _, path := range []string{"/api/debug/pprof/", "/api/debug/pprof/goroutine", "/api/debug/pprof/profile?seconds=1", "/api/debug/pprof/trace?seconds=1"} {
		resp := f.request(http.MethodGet, path, nil, false)
		if resp.StatusCode != 404 {
			t.Fatalf("GET %s while off: %d, want 404", path, resp.StatusCode)
		}
		debugPprofBody(t, resp)
	}
}

func TestDebugPprofServesTheOwnerAndAnAdmin(t *testing.T) {
	t.Setenv(debugPprofEnv, "1")
	f := newFixture(t)
	resp := f.request(http.MethodGet, "/api/debug/pprof/", nil, false)
	index := debugPprofBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(index, "goroutine") {
		t.Fatalf("index for the owner: %d, goroutine in body=%t", resp.StatusCode, strings.Contains(index, "goroutine"))
	}
	resp = f.request(http.MethodGet, "/api/debug/pprof/goroutine?debug=1", nil, false)
	body := debugPprofBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, "goroutine") {
		t.Fatalf("goroutine profile for the owner: %d", resp.StatusCode)
	}

	// An admin the owner appointed gets the same.
	adminCookie, adminCSRF, adminID := debugPprofUser(t, f, "pprofadmin")
	resp = f.request(http.MethodPut, "/api/users/"+adminID+"/admin", map[string]bool{"admin": true}, true)
	if resp.StatusCode != 204 {
		t.Fatalf("appointing the admin: %d", resp.StatusCode)
	}
	debugPprofBody(t, resp)
	resp = f.requestAs(t, adminCookie, adminCSRF, http.MethodGet, "/api/debug/pprof/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("index for the admin: %d", resp.StatusCode)
	}
	debugPprofBody(t, resp)
	resp = f.requestAs(t, adminCookie, adminCSRF, http.MethodGet, "/api/debug/pprof/goroutine?debug=1", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("goroutine profile for the admin: %d", resp.StatusCode)
	}
	debugPprofBody(t, resp)
}

func TestDebugPprofRefusesEveryoneElse(t *testing.T) {
	t.Setenv(debugPprofEnv, "1")
	f := newFixture(t)
	f.seed()
	cookie, csrf, _ := debugPprofUser(t, f, "plainperson")
	// An ordinary person keeps their session: the refusal is 403 forbidden.
	for _, path := range []string{"/api/debug/pprof/", "/api/debug/pprof/goroutine?debug=1"} {
		resp := f.requestAs(t, cookie, csrf, http.MethodGet, path, nil)
		if resp.StatusCode != 403 {
			t.Fatalf("GET %s as a non-admin person: %d, want 403", path, resp.StatusCode)
		}
		debugPprofBody(t, resp)
	}
	// An agent key is not a session: 401.
	_, key := f.createToolAgent(t, "role_reviewer_default")
	resp := f.toolRequest(t, "/api/debug/pprof/", key)
	if resp.StatusCode != 401 {
		t.Fatalf("with an agent key: %d, want 401", resp.StatusCode)
	}
	debugPprofBody(t, resp)
	// And no session at all: 401 too.
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/debug/pprof/", nil)
	if err != nil {
		t.Fatal(err)
	}
	anonymous, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if anonymous.StatusCode != 401 {
		t.Fatalf("anonymous: %d, want 401", anonymous.StatusCode)
	}
	debugPprofBody(t, anonymous)
}

// debugPprofUser creates a second account and signs in, returning its session
// cookie, CSRF token and user ID.
func debugPprofUser(t *testing.T, f *fixture, login string) (*http.Cookie, string, string) {
	t.Helper()
	hash, params, err := auth.HashPassword(login+" password", f.srv.Auth.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.CreateUser(store.User{Login: login, PWHash: hash, PWParams: params})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPost, "/api/login", map[string]string{"login": login, "password": login + " password"}, false)
	if resp.StatusCode != 200 {
		t.Fatalf("second login: %d", resp.StatusCode)
	}
	cookie := resp.Cookies()[0]
	var body struct {
		UserID    string `json:"user_id"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return cookie, body.CSRFToken, body.UserID
}
