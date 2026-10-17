package usages

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// C1: after migration the live files are token-only targets. The usage fetch and
// the target validators accept them; the FULL validators (profile store) do not.
func TestTokenOnlyCodexTargetParsesAndFetches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	doc := `{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"access-secret","refresh_token":"","account_id":"acct"}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, access, err := ValidateCodexCredentials(path); err != nil || access != "access-secret" {
		t.Fatalf("target validator: access=%q err=%v", access, err)
	}
	if _, _, err := ValidateFullCodexCredentials(path); err == nil {
		t.Fatal("a token-only codex file passed the FULL (profile store) validator")
	}
	var gotAuth string
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		return httpJSON(200, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":12,"reset_at":4102444800,"limit_window_seconds":18000}}}`), nil
	})
	snap, err := fetchCodexSnapshotFromPath(context.Background(), path)
	if err != nil || snap.Status != StatusOK {
		t.Fatalf("usage fetch on a token-only live file: %+v %v", snap, err)
	}
	if gotAuth != "Bearer access-secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	// The full-credential loader used by maintenance still rejects it.
	if _, _, err := loadCodexAuth(path); err == nil {
		t.Fatal("loadCodexAuth (full rule) accepted a token-only file")
	}
}

func TestTokenOnlyClaudeTargetParsesAndFetches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	doc := `{"claudeAiOauth":{"accessToken":"access-secret","expiresAt":4102444800000,"subscriptionType":"max"}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateClaudeCredentials(path); err != nil {
		t.Fatalf("target validator: %v", err)
	}
	if _, err := ValidateFullClaudeCredentials(path); err == nil || !strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("a token-only claude file passed the FULL validator: %v", err)
	}
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		return httpJSON(200, `{"five_hour":{"utilization":10,"resets_at":"2030-01-01T00:00:00Z"}}`), nil
	})
	snap, err := fetchClaudeSnapshotFromPath(context.Background(), path)
	if err != nil || snap.Status != StatusOK || len(snap.Windows) == 0 {
		t.Fatalf("usage fetch on a token-only live file: %+v %v", snap, err)
	}
}

// A full credential passes both rules.
func TestFullCredentialPassesBothRules(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, ".credentials.json")
	codex := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(claude, []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":4102444800000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, []byte(`{"tokens":{"access_token":"a","refresh_token":"r","account_id":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() error{
		func() error { _, err := ValidateClaudeCredentials(claude); return err },
		func() error { _, err := ValidateFullClaudeCredentials(claude); return err },
		func() error { _, _, err := ValidateCodexCredentials(codex); return err },
		func() error { _, _, err := ValidateFullCodexCredentials(codex); return err },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
}
