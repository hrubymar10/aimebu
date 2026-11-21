package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func userRequest(t *testing.T, f *fixture, cookie *http.Cookie, csrf, method, path string, body any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(method, f.http.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readInvite(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("invite status=%d", resp.StatusCode)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Token == "" {
		t.Fatal("empty invite token")
	}
	return result.Token
}

func TestUserInviteSignupAndViewerPermissions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	token := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"}, true))
	resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "Friend", "password": "a secure password"})
	if resp.StatusCode != 201 {
		t.Fatalf("redeem status=%d", resp.StatusCode)
	}
	var redeemed struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&redeemed); err != nil {
		t.Fatal(err)
	}
	if redeemed.Login != "friend" {
		t.Fatalf("stored login=%q", redeemed.Login)
	}
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "another", "password": "a secure password"})
	if resp.StatusCode != 401 {
		t.Fatalf("replay status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/login", map[string]string{"login": "FRIEND", "password": "a secure password"})
	if resp.StatusCode != 200 {
		t.Fatalf("friend login=%d", resp.StatusCode)
	}
	cookie := resp.Cookies()[0]
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, test := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/projects", map[string]string{"name": "not allowed", "folder": t.TempDir()}},
		{"POST", "/api/projects/" + f.project.ID + "/rooms", map[string]string{"title": "not allowed"}},
		{"POST", "/api/rooms/" + f.room.ID + "/messages", map[string]string{"body": "not allowed"}},
		{"POST", "/api/agents/" + agent.ID + "/control", map[string]any{"desired": "stopped", "rev": 0}},
		{"GET", "/api/users?project_id=" + f.project.ID, nil},
		{"GET", "/api/usages", nil},
		{"GET", "/api/switcher", nil},
	} {
		resp = userRequest(t, f, cookie, login.CSRF, test.method, test.path, test.body)
		if resp.StatusCode != 403 {
			t.Errorf("%s %s status=%d", test.method, test.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp = userRequest(t, f, cookie, login.CSRF, "GET", "/api/projects/"+f.project.ID+"/rooms", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("viewer read=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestExistingAccountRedeemsGrantAndResetRevokesSessions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	second := f.request("POST", "/api/projects", map[string]string{"name": "second", "folder": t.TempDir()}, true)
	var project store.Project
	if err := json.NewDecoder(second.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	join := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "operator"}, true))
	resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": join, "login": "friend", "password": "a secure password"})
	if resp.StatusCode != 201 {
		t.Fatalf("signup=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/login", map[string]string{"login": "friend", "password": "a secure password"})
	if resp.StatusCode != 200 {
		t.Fatalf("login=%d", resp.StatusCode)
	}
	cookie := resp.Cookies()[0]
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()
	more := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": project.ID, "role": "viewer"}, true))
	resp = userRequest(t, f, cookie, login.CSRF, "POST", "/api/invites/redeem", map[string]string{"token": more})
	if resp.StatusCode != 201 {
		t.Fatalf("existing grant=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, cookie, login.CSRF, "GET", "/api/projects", nil)
	var projects []store.Project
	json.NewDecoder(resp.Body).Decode(&projects)
	resp.Body.Close()
	if len(projects) != 2 {
		t.Fatalf("projects=%+v", projects)
	}
	friend, err := f.db.UserByLogin(context.Background(), "friend")
	if err != nil {
		t.Fatal(err)
	}
	resp = f.request("PUT", "/api/projects/"+f.project.ID+"/members/"+friend.ID, map[string]string{"role": "viewer"}, true)
	if resp.StatusCode != 204 {
		t.Fatalf("change member role=%d", resp.StatusCode)
	}
	resp.Body.Close()
	role, err := f.db.UserProjectRole(context.Background(), friend.ID, f.project.ID)
	if err != nil || role != "viewer" {
		t.Fatalf("changed role=%q err=%v", role, err)
	}
	reset := readInvite(t, f.request("POST", "/api/invites", map[string]string{"kind": "reset", "target_user_id": friend.ID}, true))
	resp = userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": reset, "password": "another secure password"})
	if resp.StatusCode != 201 {
		t.Fatalf("reset=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, cookie, login.CSRF, "GET", "/api/me", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("old session=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/login", map[string]string{"login": "friend", "password": "another secure password"})
	if resp.StatusCode != 200 {
		t.Fatalf("new login=%d", resp.StatusCode)
	}
	newCookie := resp.Cookies()[0]
	resp.Body.Close()
	resp = f.request("POST", "/api/users/"+friend.ID+"/deactivate", nil, true)
	if resp.StatusCode != 204 {
		t.Fatalf("deactivate=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, newCookie, "", "GET", "/api/me", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("deactivated session=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/login", map[string]string{"login": "friend", "password": "another secure password"})
	if resp.StatusCode != 401 {
		t.Fatalf("deactivated login=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUserInviteExpiryAndLastOwner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	owner, err := f.db.UserByLogin(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	resp := f.request("DELETE", "/api/projects/"+f.project.ID+"/members/"+owner.ID, nil, true)
	if resp.StatusCode != 409 {
		t.Fatalf("last owner=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("POST", "/api/users/"+owner.ID+"/deactivate", nil, true)
	if resp.StatusCode != 403 {
		t.Fatalf("global owner deactivation=%d", resp.StatusCode)
	}
	resp.Body.Close()
	token := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"}, true))
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE user_invite SET expires_at='2000-01-01T00:00:00.000000000Z'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "expired", "password": "a secure password"})
	if resp.StatusCode != 401 {
		t.Fatalf("expired invite=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProjectOwnerManagesOnlyOwnMembers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	second := f.request("POST", "/api/projects", map[string]string{"name": "second", "folder": t.TempDir()}, true)
	var project store.Project
	if err := json.NewDecoder(second.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	token := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "owner"}, true))
	resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "manager", "password": "a secure password"})
	if resp.StatusCode != 201 {
		t.Fatalf("manager signup=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/login", map[string]string{"login": "manager", "password": "a secure password"})
	if resp.StatusCode != 200 {
		t.Fatalf("manager login=%d", resp.StatusCode)
	}
	cookie := resp.Cookies()[0]
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, test := range []struct {
		body map[string]string
		want int
	}{
		{map[string]string{"project_id": f.project.ID, "role": "viewer"}, 201},
		{map[string]string{"project_id": project.ID, "role": "viewer"}, 403},
		{map[string]string{"kind": "reset", "target_user_id": "anything"}, 403},
	} {
		resp = userRequest(t, f, cookie, login.CSRF, "POST", "/api/invites", test.body)
		if resp.StatusCode != test.want {
			t.Errorf("invite %+v status=%d want=%d", test.body, resp.StatusCode, test.want)
		}
		resp.Body.Close()
	}
	resp = userRequest(t, f, cookie, login.CSRF, "GET", "/api/users?project_id="+project.ID, nil)
	if resp.StatusCode != 403 {
		t.Errorf("other project users=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, cookie, login.CSRF, "GET", "/api/users?project_id="+f.project.ID, nil)
	if resp.StatusCode != 200 {
		t.Errorf("own project users=%d", resp.StatusCode)
	}
	resp.Body.Close()
	manager, err := f.db.UserByLogin(context.Background(), "manager")
	if err != nil {
		t.Fatal(err)
	}
	pending := readInvite(t, userRequest(t, f, cookie, login.CSRF, "POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"}))
	resp = userRequest(t, f, cookie, login.CSRF, "DELETE", "/api/projects/"+f.project.ID+"/members/"+manager.ID, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("remove self=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, cookie, login.CSRF, "POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"})
	if resp.StatusCode != 403 {
		t.Errorf("demoted manager invite=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": pending, "login": "lateuser", "password": "a secure password"})
	if resp.StatusCode != 401 {
		t.Errorf("removed manager's invite redeemed=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestInviteLinksUseConfiguredPublicOriginAndTLSFallback(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.Auth.PublicURL = "https://chat.example.test:8443"
	resp := f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("invite=%d", resp.StatusCode)
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(result.URL, "https://chat.example.test:8443/#invite=aui_") {
		t.Fatalf("public link=%q", result.URL)
	}
	f.srv.Auth.PublicURL = ""
	f.srv.TLSPort = 9443
	resp = f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("TLS fallback invite=%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(result.URL, "https://127.0.0.1:9443/#invite=aui_") {
		t.Fatalf("TLS fallback link=%q", result.URL)
	}
}

func TestInviteDoesNotRevealExistingLoginOrCollideWithOwnerCase(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE user SET login='Owner' WHERE login='owner'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	token := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"}, true))
	for _, login := range []string{"OWNER", "owner"} {
		resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": login, "password": "a secure password"})
		var problem struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || problem.Error != "invalid_invite" {
			t.Fatalf("existing login %q: %d %s", login, resp.StatusCode, problem.Error)
		}
	}
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE user SET login='Kate' WHERE login='Owner'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "kate", "password": "a secure password"})
	if resp.StatusCode != 401 {
		t.Fatalf("Unicode case collision status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "newperson", "password": "a secure password"})
	if resp.StatusCode != 201 {
		t.Fatalf("valid login after collision=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestReverseProxyOriginUpgradesAndSecuresCookies(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.Auth.PublicURL = "https://aimebu.example.com"
	login := func(host string) *http.Cookie {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"login": "owner", "password": "correct horse battery staple"})
		req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/login", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || len(resp.Cookies()) == 0 {
			t.Fatalf("login via %q=%d cookies=%v", host, resp.StatusCode, resp.Cookies())
		}
		return resp.Cookies()[0]
	}
	if direct := login(""); direct.Secure {
		t.Fatal("direct plain-HTTP login to a non-public host got a Secure cookie")
	}
	if lan := login("192.168.1.20:9897"); lan.Secure {
		t.Fatal("plain-HTTP login via a LAN address got a Secure cookie")
	}
	proxied := login("aimebu.example.com")
	if !proxied.Secure {
		t.Fatal("login through the HTTPS public host lacks Secure")
	}
	f.cookie = proxied
	conn := f.dial(t, 0, "https://aimebu.example.com")
	conn.Close(websocket.StatusNormalClosure, "")
	header := http.Header{}
	header.Set("Origin", "https://evil.example.com")
	header.Set("Cookie", f.cookie.String())
	_, wsResp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/events", &websocket.DialOptions{HTTPHeader: header})
	if err == nil || wsResp == nil || wsResp.StatusCode != 403 {
		t.Fatalf("foreign origin: %v, %v", err, wsResp)
	}
}

func proxiedLogin(t *testing.T, f *fixture, login, password, forwardedFor string) int {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"login": login, "password": password})
	req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/login", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", forwardedFor)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestTrustedProxyClientsDoNotShareLoginBackoff(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	for i := range 6 {
		if status := proxiedLogin(t, f, fmt.Sprintf("ghost-%d", i), "wrong", "198.51.100.1"); status != 401 {
			t.Fatalf("attacker failure %d=%d", i, status)
		}
	}
	if status := proxiedLogin(t, f, "ghost-next", "wrong", "198.51.100.1"); status != 429 {
		t.Fatalf("attacker after backoff=%d", status)
	}
	if status := proxiedLogin(t, f, "owner", "correct horse battery staple", "198.51.100.2"); status != 200 {
		t.Fatalf("second proxied client shared the attacker's backoff: %d", status)
	}
}

func TestUntrustedForwardedForDoesNotSplitLoginBackoff(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for i := range 6 {
		if status := proxiedLogin(t, f, fmt.Sprintf("ghost-%d", i), "wrong", fmt.Sprintf("198.51.100.%d", i+1)); status != 401 {
			t.Fatalf("failure %d=%d", i, status)
		}
	}
	if status := proxiedLogin(t, f, "ghost-next", "wrong", "198.51.100.99"); status != 429 {
		t.Fatalf("spoofed X-Forwarded-For escaped the per-IP backoff: %d", status)
	}
}

// A proxy that rewrites Host to the upstream address (nginx's default) sends
// Host: 127.0.0.1:9897 with the browser's public Origin. Both aimebu's origin
// check and the WebSocket library's must accept it.
func TestEventSocketUpgradesWhenProxyRewritesHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dial := func(origin string) (int, error) {
		header := http.Header{}
		header.Set("Origin", origin)
		header.Set("Cookie", f.cookie.String())
		conn, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/events",
			&websocket.DialOptions{HTTPHeader: header, Host: "127.0.0.1:9897"})
		if conn != nil {
			conn.Close(websocket.StatusNormalClosure, "")
		}
		if resp == nil {
			return 0, err
		}
		return resp.StatusCode, err
	}
	for _, tc := range []struct {
		publicURL, origin string
		want              int
	}{
		{"https://aimebu.example.com", "https://aimebu.example.com", http.StatusSwitchingProtocols},
		{"https://aimebu.example.com:443", "https://aimebu.example.com", http.StatusSwitchingProtocols},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443", http.StatusSwitchingProtocols},
		{"https://aimebu.example.com", "http://aimebu.example.com", http.StatusForbidden},
		{"https://aimebu.example.com", "https://evil.example.com", http.StatusForbidden},
		{"", "https://aimebu.example.com", http.StatusForbidden},
	} {
		f.srv.Auth.PublicURL = tc.publicURL
		if status, err := dial(tc.origin); status != tc.want {
			t.Errorf("public %q origin %q: status %d (%v), want %d", tc.publicURL, tc.origin, status, err, tc.want)
		}
	}
}
