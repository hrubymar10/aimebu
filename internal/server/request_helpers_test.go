package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/store"
)

// registerSecondUser creates a second user and returns its session cookie and
// CSRF token, for cross-user isolation checks.
func registerSecondUser(t *testing.T, f *fixture, login string) (*http.Cookie, string) {
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
	var loginBody struct {
		CSRFToken string `json:"csrf_token"`
	}
	json.NewDecoder(resp.Body).Decode(&loginBody)
	resp.Body.Close()
	return cookie, loginBody.CSRFToken
}

// requestAs sends a request as the given session, with its CSRF token on writes.
func (f *fixture) requestAs(t *testing.T, cookie *http.Cookie, csrf string, method, path string, body any) *http.Response {
	t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, f.http.URL+path, strings.NewReader(string(data)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(cookie)
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
