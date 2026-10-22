package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/store"
)

const testPassword = "correct horse battery staple"

func testService(t *testing.T) (*Service, string, string) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "auth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(db)
	service.Policy = PasswordPolicy{Iterations: 1000, SaltBytes: 16}
	_, machineID, machineKey, created, err := service.Bootstrap(context.Background(), "owner", testPassword, "host")
	if err != nil || !created {
		t.Fatalf("bootstrap: created=%v error=%v", created, err)
	}
	return service, machineID, machineKey
}

func TestPasswordHashAndPolicyUpgrade(t *testing.T) {
	old := PasswordPolicy{Iterations: 500, SaltBytes: 16}
	current := PasswordPolicy{Iterations: 1000, SaltBytes: 16}
	hash, params, err := HashPassword(testPassword, old)
	if err != nil {
		t.Fatal(err)
	}
	matched, rehash, err := VerifyPassword(testPassword, hash, params, current)
	if err != nil || !matched || !rehash {
		t.Fatalf("match=%v rehash=%v error=%v", matched, rehash, err)
	}
	matched, rehash, err = VerifyPassword("wrong password", hash, params, current)
	if err != nil || matched || rehash {
		t.Fatalf("wrong password: match=%v rehash=%v error=%v", matched, rehash, err)
	}
	if _, _, err := HashPassword(testPassword, PasswordPolicy{Iterations: 0, SaltBytes: 16}); err == nil {
		t.Fatal("invalid policy accepted")
	}
}

func TestBootstrapAndTransparentRehash(t *testing.T) {
	service, machineID, machineKey := testService(t)
	if !strings.HasPrefix(machineID, "mch_") || !strings.HasPrefix(machineKey, "amk_") {
		t.Fatalf("bad machine credentials %q %q", machineID, machineKey)
	}
	if err := service.VerifyMachineKey(context.Background(), machineID, machineKey); err != nil {
		t.Fatal(err)
	}
	_, _, _, created, err := service.Bootstrap(context.Background(), "", "", "host")
	if err != nil || created {
		t.Fatalf("repeat bootstrap: created=%v error=%v", created, err)
	}
	user, err := service.Store.UserByLogin(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	oldHash, oldParams, err := HashPassword(testPassword, PasswordPolicy{Iterations: 500, SaltBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store.UpdateUserPassword(context.Background(), user.ID, oldHash, oldParams); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(context.Background(), "owner", testPassword, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Store.UserByLogin(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if updated.PWParams == oldParams || updated.PWHash == oldHash {
		t.Fatal("password parameters were not upgraded")
	}
	matched, rehash, err := VerifyPassword(testPassword, updated.PWHash, updated.PWParams, service.Policy)
	if err != nil || !matched || rehash {
		t.Fatalf("upgraded password: match=%v rehash=%v error=%v", matched, rehash, err)
	}
}

func TestLoginComparesCanonicalCase(t *testing.T) {
	service, _, _ := testService(t)
	result, err := service.Login(context.Background(), " OWNER ", testPassword, "127.0.0.1")
	if err != nil || result.UserID == "" {
		t.Fatalf("mixed-case login: %+v %v", result, err)
	}
	user, err := service.Store.UserByLogin(context.Background(), "Owner")
	if err != nil || user.ID != result.UserID {
		t.Fatalf("canonical lookup: %+v %v", user, err)
	}
}

func TestLoginKeepsLegacyExactNamesAndRejectsAmbiguousFallback(t *testing.T) {
	for _, name := range []string{"Owner", " owner with space ", "Älice"} {
		t.Run(name, func(t *testing.T) {
			service, _, _ := testService(t)
			ctx := context.Background()
			owner, err := service.Store.UserByLogin(ctx, "owner")
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Store.Tx(ctx, func(tx *store.Tx) error {
				_, err := tx.Exec(`UPDATE user SET login=? WHERE id=?`, name, owner.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			result, err := service.Login(ctx, name, testPassword, "127.0.0.1")
			if err != nil || result.UserID != owner.ID {
				t.Fatalf("exact legacy login %q: %+v, %v", name, result, err)
			}
			fallback := strings.ToUpper(strings.TrimSpace(name))
			result, err = service.Login(ctx, fallback, testPassword, "127.0.0.1")
			if err != nil || result.UserID != owner.ID {
				t.Fatalf("unique canonical login %q: %+v, %v", fallback, result, err)
			}
		})
	}
	service, _, _ := testService(t)
	ctx := context.Background()
	owner, err := service.Store.UserByLogin(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	otherHash, otherParams, err := HashPassword("another secure password", service.Policy)
	if err != nil {
		t.Fatal(err)
	}
	var other store.User
	if err := service.Store.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE user SET login='Owner' WHERE id=?`, owner.ID); err != nil {
			return err
		}
		other, err = tx.CreateUser(store.User{Login: "owner", PWHash: otherHash, PWParams: otherParams})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ login, password, userID string }{
		{"Owner", testPassword, owner.ID},
		{"owner", "another secure password", other.ID},
	} {
		result, err := service.Login(ctx, check.login, check.password, "127.0.0.1")
		if err != nil || result.UserID != check.userID {
			t.Fatalf("exact collision login %q: %+v, %v", check.login, result, err)
		}
	}
	if _, err := service.Login(ctx, " OWNER ", testPassword, "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ambiguous canonical login = %v", err)
	}
}

func TestInviteFailuresDoNotThrottleAnotherTokenAndIP(t *testing.T) {
	service, _, _ := testService(t)
	first := "aui_" + strings.Repeat("a", 36)
	second := "aui_" + strings.Repeat("b", 36)
	for range 6 {
		_, _, _, err := service.RedeemUserInvite(context.Background(), first, "freshuser", "a secure password", "", "192.0.2.1")
		if !errors.Is(err, store.ErrInviteInvalid) {
			t.Fatalf("first token failure=%v", err)
		}
	}
	if _, _, _, err := service.RedeemUserInvite(context.Background(), first, "freshuser", "a secure password", "", "192.0.2.1"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first token backoff=%v", err)
	}
	if _, _, _, err := service.RedeemUserInvite(context.Background(), second, "freshuser", "a secure password", "", "192.0.2.2"); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("independent token and IP=%v", err)
	}
}

func TestPairingFailuresDoNotThrottleAnotherTokenAndIP(t *testing.T) {
	service, _, _ := testService(t)
	ctx := context.Background()
	first := "amp_" + strings.Repeat("a", 43)
	for range 6 {
		if _, _, err := service.RedeemMachinePairing(ctx, first, "192.0.2.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("first token failure=%v", err)
		}
	}
	if _, _, err := service.RedeemMachinePairing(ctx, first, "192.0.2.1"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first token backoff=%v", err)
	}
	if service.PairingRetryAfter(first, "192.0.2.1") <= 0 {
		t.Fatal("missing pairing retry delay")
	}
	owner, err := service.Store.UserByLogin(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := service.CreateMachinePairing(ctx, "laptop", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, key, err := service.RedeemMachinePairing(ctx, token, "192.0.2.2"); err != nil || key == "" {
		t.Fatalf("independent token and IP blocked: %v", err)
	}
}

func TestBootstrapKeyWriteFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bootstrap.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(db)
	service.Policy = PasswordPolicy{Iterations: 1000, SaltBytes: 16}
	want := errors.New("key write failed")
	_, _, _, created, err := service.BootstrapWithKeySink(ctx, "owner", testPassword, "host", func(_, _ string) error { return want })
	if !errors.Is(err, want) || created {
		t.Fatalf("failed bootstrap: created=%v error=%v", created, err)
	}
	count, err := db.UserCount(ctx)
	if err != nil || count != 0 {
		t.Fatalf("users after failed key write: %d, %v", count, err)
	}
	_, _, _, created, err = service.Bootstrap(ctx, "owner", testPassword, "host")
	if err != nil || !created {
		t.Fatalf("retry bootstrap: created=%v error=%v", created, err)
	}
}

func TestLoginUniformErrorsAndThrottle(t *testing.T) {
	service, _, _ := testService(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	ctx := context.Background()
	_, known := service.Login(ctx, "owner", "wrong", "127.0.0.1")
	_, unknown := service.Login(ctx, "absent", "wrong", "127.0.0.2")
	if !errors.Is(known, ErrInvalidCredentials) || !errors.Is(unknown, ErrInvalidCredentials) || known.Error() != unknown.Error() {
		t.Fatalf("nonuniform errors: %v / %v", known, unknown)
	}
	if _, err := service.Login(ctx, "owner", testPassword, "127.0.0.3"); err != nil {
		t.Fatalf("correct retry after typo = %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := service.Login(ctx, "owner", "wrong", "127.0.0.4"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if _, err := service.Login(ctx, "owner", testPassword, "127.0.0.3"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("per-login throttle = %v", err)
	}
	if _, err := service.Login(ctx, "absent", "wrong", "127.0.0.4"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("per-IP throttle = %v", err)
	}
	now = now.Add(250 * time.Millisecond)
	if _, err := service.Login(ctx, "owner", "wrong", "127.0.0.3"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("attempt after first backoff = %v", err)
	}
	if _, err := service.Login(ctx, "owner", testPassword, "127.0.0.3"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("exponential backoff = %v", err)
	}
	now = now.Add(500 * time.Millisecond)
	if _, err := service.Login(ctx, "owner", testPassword, "127.0.0.3"); err != nil {
		t.Fatalf("login after backoff = %v", err)
	}
}

func TestSessionCookieCSRFExpiryAndRevocation(t *testing.T) {
	service, _, _ := testService(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	login, err := service.Login(context.Background(), "owner", testPassword, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(login.SessionToken, "as_") || len(login.SessionToken) < 40 {
		t.Fatalf("bad session token %q", login.SessionToken)
	}
	session, err := service.Store.SessionByTokenHash(context.Background(), hashToken(login.SessionToken))
	if err != nil || session.ID == login.SessionToken || !strings.HasPrefix(session.ID, "ses_") {
		t.Fatalf("stored session id/hash: %+v, %v", session, err)
	}
	var served bool
	handler := service.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		if id, ok := UserIDFromContext(r.Context()); !ok || id != login.UserID {
			t.Errorf("context user = %q, %v", id, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func(method, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://aimebu.test/rooms", nil)
		r.AddCookie(service.SessionCookie(login.SessionToken, true))
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodPost, ""); w.Code != http.StatusForbidden || served {
		t.Fatalf("missing CSRF: code=%d served=%v", w.Code, served)
	}
	if w := request(http.MethodPost, "bad"); w.Code != http.StatusForbidden || served {
		t.Fatalf("bad CSRF: code=%d served=%v", w.Code, served)
	}
	w := request(http.MethodPost, login.CSRFToken)
	if w.Code != http.StatusNoContent || !served {
		t.Fatalf("valid CSRF: code=%d served=%v", w.Code, served)
	}
	if cookie := w.Result().Cookies()[0]; !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags: %+v", cookie)
	}
	if service.SessionCookie(login.SessionToken, false).Secure {
		t.Fatal("plain HTTP cookie marked Secure")
	}
	now = now.Add(24 * time.Hour)
	if _, err := service.Authenticate(context.Background(), login.SessionToken); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6*24*time.Hour + time.Hour)
	if _, err := service.Authenticate(context.Background(), login.SessionToken); err != nil {
		t.Fatalf("sliding expiry failed: %v", err)
	}
	if err := service.RevokeSession(context.Background(), login.SessionToken); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), login.SessionToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoked session = %v", err)
	}
}

func TestCheckSessionDoesNotRenewExpiry(t *testing.T) {
	service, _, _ := testService(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	login, err := service.Login(context.Background(), "owner", testPassword, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		now = now.Add(24 * time.Hour)
		if _, err := service.CheckSession(context.Background(), login.SessionToken); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := service.Store.SessionByTokenHash(context.Background(), hashToken(login.SessionToken))
	if err != nil || stored.ExpiresAt != login.ExpiresAt.Format(sessionTimeFormat) {
		t.Fatalf("read-only check renewed expiry: %+v %v", stored, err)
	}
	now = login.ExpiresAt.Add(time.Second)
	if _, err := service.CheckSession(context.Background(), login.SessionToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired session accepted: %v", err)
	}
}

func TestExpiredSessionAndWebSocketOrigin(t *testing.T) {
	service, _, _ := testService(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	login, err := service.Login(context.Background(), "owner", testPassword, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	request := func(origin string, cookie bool) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://aimebu.test/ws", nil)
		r.TLS = &tls.ConnectionState{}
		r.Header.Set("Origin", origin)
		if cookie {
			r.AddCookie(service.SessionCookie(login.SessionToken, true))
		}
		return r
	}
	for _, tc := range []struct {
		origin string
		cookie bool
		want   bool
	}{
		{"https://aimebu.test", true, true},
		{"https://elsewhere.test", true, false},
		{"http://aimebu.test", true, false},
		{"https://aimebu.test", false, false},
		{"", true, false},
	} {
		if got := service.CheckWSOrigin(request(tc.origin, tc.cookie)); got != tc.want {
			t.Errorf("origin %q cookie %v = %v", tc.origin, tc.cookie, got)
		}
	}
	now = now.Add(8 * 24 * time.Hour)
	if _, err := service.Authenticate(context.Background(), login.SessionToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired session = %v", err)
	}
	if service.CheckWSOrigin(request("https://aimebu.test", true)) {
		t.Fatal("expired WebSocket session accepted")
	}
}

func TestWebSocketOriginBehindReverseProxy(t *testing.T) {
	service, _, _ := testService(t)
	login, err := service.Login(context.Background(), "owner", testPassword, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	// A TLS-terminating proxy forwards plain HTTP to the loopback listener.
	request := func(origin string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9897/api/events", nil)
		r.Header.Set("Origin", origin)
		r.AddCookie(service.SessionCookie(login.SessionToken, true))
		return r
	}
	if service.CheckWSOrigin(request("https://aimebu.example.com")) {
		t.Fatal("public origin accepted without AIMEBU2_PUBLIC_URL")
	}
	if service.SecureRequest(request("https://aimebu.example.com")) {
		t.Fatal("plain HTTP treated as secure without a public HTTPS URL")
	}
	service.PublicURL = "https://Aimebu.Example.com:443"
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"https://aimebu.example.com", true},
		{"https://AIMEBU.example.com", true},
		{"http://aimebu.example.com", false},
		{"https://aimebu.example.com:8443", false},
		{"https://elsewhere.example.com", false},
		{"http://127.0.0.1:9897", true},
	} {
		if got := service.CheckWSOrigin(request(tc.origin)); got != tc.want {
			t.Errorf("origin %q = %v, want %v", tc.origin, got, tc.want)
		}
	}
	withHost := func(host string) *http.Request {
		r := request("https://aimebu.example.com")
		r.Host = host
		return r
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"aimebu.example.com", true},
		{"AIMEBU.example.com:443", true},
		{"127.0.0.1:9897", false},
		{"192.168.1.20:9897", false},
		{"aimebu.example.com:9897", false},
	} {
		if got := service.SecureRequest(withHost(tc.host)); got != tc.want {
			t.Errorf("plain HTTP Host %q secure=%v, want %v", tc.host, got, tc.want)
		}
	}
	tlsRequest := withHost("192.168.1.20:9896")
	tlsRequest.TLS = &tls.ConnectionState{}
	if !service.SecureRequest(tlsRequest) {
		t.Fatal("direct TLS request not secure")
	}
	service.PublicURL = "http://aimebu.example.com"
	if service.SecureRequest(withHost("aimebu.example.com")) {
		t.Fatal("HTTP public URL marked requests secure")
	}
}

func TestAgentAndMachineKeys(t *testing.T) {
	service, machineID, machineKey := testService(t)
	ctx := context.Background()
	if err := service.VerifyMachineKey(ctx, machineID, "amk_wrong"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("wrong machine key = %v", err)
	}
	oldHash, err := service.VerifiedMachineKeyHash(ctx, machineID, machineKey)
	if err != nil || oldHash == machineKey {
		t.Fatalf("verified machine hash=%q %v", oldHash, err)
	}
	if err := service.VerifyMachineKeyHash(ctx, machineID, oldHash); err != nil {
		t.Fatal(err)
	}
	newMachineKey, err := service.IssueMachineKey(ctx, machineID)
	if err != nil || newMachineKey == machineKey {
		t.Fatalf("machine key rotation: %q, %v", newMachineKey, err)
	}
	if err := service.VerifyMachineKey(ctx, machineID, machineKey); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("old machine key accepted")
	}
	if err := service.VerifyMachineKey(ctx, machineID, newMachineKey); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyMachineKeyHash(ctx, machineID, oldHash); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("old socket hash accepted after rotation: %v", err)
	}
	newHash, err := service.VerifiedMachineKeyHash(ctx, machineID, newMachineKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyMachineKeyHash(ctx, machineID, newHash); err != nil {
		t.Fatal(err)
	}
	project, err := service.Store.CreateProject(ctx, store.Project{Name: "keys"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_key_test','worker','Worker','','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path)
			VALUES ('chk_key_test',?,?,?)`, project.ID, machineID, "/tmp/key-test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := service.Store.CreateAgent(ctx, store.Agent{Slug: "worker", ProjectID: project.ID,
		RoleID: "role_key_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test",
		MachineID: machineID, CheckoutID: "chk_key_test", KeyHash: "pending", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	agentKey, err := service.IssueAgentKey(ctx, agent.ID)
	if err != nil || !strings.HasPrefix(agentKey, "aak_") {
		t.Fatalf("agent key: %q, %v", agentKey, err)
	}
	if err := service.VerifyAgentKey(ctx, agent.ID, agentKey); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyAgentKey(ctx, agent.ID, "aak_wrong"); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("wrong agent key accepted")
	}
	if err := service.Store.RevokeAgent(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyAgentKey(ctx, agent.ID, agentKey); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("revoked agent key accepted")
	}
	if err := service.Store.RevokeMachine(ctx, machineID); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyMachineKey(ctx, machineID, newMachineKey); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("revoked machine key accepted")
	}
	if err := service.VerifyMachineKeyHash(ctx, machineID, newHash); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked socket hash accepted: %v", err)
	}
}

func TestThrottleReservesParallelAttempts(t *testing.T) {
	throttle := NewThrottler()
	now := time.Now()
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if throttle.reserve("target", "127.0.0.1", now) {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := admitted.Load(); got != 1 {
		t.Fatalf("parallel admissions=%d", got)
	}
	throttle.fail("target", "127.0.0.1", now)
	for i := 1; i < freeFailureBudget; i++ {
		if !throttle.reserve("target", "127.0.0.1", now) {
			t.Fatalf("free attempt %d blocked", i)
		}
		throttle.fail("target", "127.0.0.1", now)
	}
	if !throttle.reserve("target", "127.0.0.1", now) {
		t.Fatal("last free attempt blocked")
	}
	throttle.fail("target", "127.0.0.1", now)
	if throttle.reserve("target", "127.0.0.1", now) {
		t.Fatal("backoff bypassed")
	}
	if throttle.RetryAfter("target", "127.0.0.1", now) <= 0 {
		t.Fatal("missing retry delay")
	}
	if !throttle.reserve("target", "127.0.0.1", now.Add(time.Second)) {
		t.Fatal("retry blocked after backoff")
	}
}

func TestLoginTypoThenCorrect(t *testing.T) {
	service, _, _ := testService(t)
	ctx := context.Background()
	if _, err := service.Login(ctx, "owner", "typo", "same-ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("typo: %v", err)
	}
	if _, err := service.Login(ctx, "owner", testPassword, "same-ip"); err != nil {
		t.Fatalf("correct retry: %v", err)
	}
	if _, ok := service.Throttle.entries["login:owner"]; ok {
		t.Fatal("login throttle entry survived a successful login")
	}
	if entry := service.Throttle.entries["ip:same-ip"]; entry.failures != 1 {
		t.Fatalf("IP failures after success=%d, want 1", entry.failures)
	}
}

func TestSuccessfulLoginDoesNotResetIPFailures(t *testing.T) {
	service, _, _ := testService(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	ctx := context.Background()
	for i := range freeFailureBudget {
		if _, err := service.Login(ctx, fmt.Sprintf("victim-%d", i), "guess", "198.51.100.9"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("spray %d: %v", i, err)
		}
	}
	// The sprayer signs in to its own account from the same address.
	if _, err := service.Login(ctx, "owner", testPassword, "198.51.100.9"); err != nil {
		t.Fatalf("own login: %v", err)
	}
	if _, err := service.Login(ctx, "victim-next", "guess", "198.51.100.9"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("sixth guess: %v", err)
	}
	if _, err := service.Login(ctx, "victim-last", "guess", "198.51.100.9"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("successful login reset the per-IP budget: %v", err)
	}
}

func TestThrottleEntriesAreBoundedAndPruned(t *testing.T) {
	throttle := NewThrottler()
	now := time.Now()
	for i := 0; i < maxThrottleEntries+100; i++ {
		login := fmt.Sprintf("user-%d", i)
		ip := fmt.Sprintf("ip-%d", i)
		if !throttle.reserve(login, ip, now) {
			t.Fatalf("reservation %d rejected", i)
		}
		throttle.fail(login, ip, now)
	}
	if got := len(throttle.entries); got > maxThrottleEntries {
		t.Fatalf("entries=%d", got)
	}
	if !throttle.reserve("fresh", "fresh-ip", now.Add(16*time.Minute)) {
		t.Fatal("fresh reservation blocked")
	}
	if got := len(throttle.entries); got != 2 {
		t.Fatalf("expired entries retained: %d", got)
	}
}

func TestLoginWithOlderStoredPolicy(t *testing.T) {
	service, _, _ := testService(t)
	now := time.Now()
	service.Now = func() time.Time { return now }
	ctx := context.Background()
	user, err := service.Store.UserByLogin(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	hash, params, err := HashPassword(testPassword, PasswordPolicy{Iterations: 500, SaltBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store.UpdateUserPassword(ctx, user.ID, hash, params); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, "owner", "wrong", "ip-1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old-hash wrong password: %v", err)
	}
	if _, err := service.Login(ctx, "absent", "wrong", "ip-2"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown login: %v", err)
	}
	now = now.Add(time.Second)
	if _, err := service.Login(ctx, "owner", testPassword, "ip-3"); err != nil {
		t.Fatalf("old-hash upgrade: %v", err)
	}
}

func TestAuthenticateSlidesExpiryOnlyWhenItLags(t *testing.T) {
	service, _, _ := testService(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	login, err := service.Login(context.Background(), "owner", testPassword, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	stored := func() string {
		t.Helper()
		session, err := service.Store.SessionByTokenHash(context.Background(), hashToken(login.SessionToken))
		if err != nil {
			t.Fatal(err)
		}
		return session.ExpiresAt
	}
	initial := stored()
	// Well inside the threshold (TTL/10 = 16.8h): authenticated, not written.
	now = now.Add(time.Hour)
	if _, err := service.Authenticate(context.Background(), login.SessionToken); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != initial {
		t.Fatalf("fresh session was rewritten: %s -> %s", initial, got)
	}
	// Past the threshold: the expiry slides to now+TTL.
	now = now.Add(service.slideThreshold())
	if _, err := service.Authenticate(context.Background(), login.SessionToken); err != nil {
		t.Fatal(err)
	}
	want := now.Add(service.SessionTTL).Format(sessionTimeFormat)
	if got := stored(); got != want {
		t.Fatalf("lagging session not extended: got %s want %s", got, want)
	}
}

func TestSlideThresholdBounds(t *testing.T) {
	service := &Service{SessionTTL: 7 * 24 * time.Hour}
	if got := service.slideThreshold(); got != 7*24*time.Hour/10 {
		t.Fatalf("week TTL threshold = %s", got)
	}
	service.SessionTTL = 30 * time.Minute
	if got := service.slideThreshold(); got != 5*time.Minute {
		t.Fatalf("half-hour TTL threshold = %s", got)
	}
	service.SessionTTL = 4 * time.Minute
	if got := service.slideThreshold(); got != 2*time.Minute {
		t.Fatalf("short TTL threshold = %s", got)
	}
}
