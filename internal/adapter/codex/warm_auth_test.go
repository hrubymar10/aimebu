package codex

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

func TestWarmAuthRefreshOnlyUsesNewSameAccountTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	claims, _ := json.Marshal(map[string]string{"chatgpt_plan_type": "pro"})
	id := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	write := func(account, access, refresh string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"auth_mode": "chatgptAuthTokens", "tokens": map[string]string{
			"account_id": account, "access_token": access, "refresh_token": refresh, "id_token": id}})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("acct-one", "old", "")
	initial, err := readCodexWarmAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &codexWarmSession{authPath: path, initialAuth: initial}
	if result, problem := w.refreshAuthTokens(json.RawMessage(`{"previousAccountId":"acct-one"}`)); problem == nil || result != nil {
		t.Fatal("unchanged token was supplied")
	}
	write("acct-two", "new", "")
	if result, problem := w.refreshAuthTokens(json.RawMessage(`{"previousAccountId":"acct-one"}`)); problem == nil || result != nil {
		t.Fatal("another account's token was supplied")
	}
	write("acct-one", "new", "still-full")
	if result, problem := w.refreshAuthTokens(json.RawMessage(`{"previousAccountId":"acct-one"}`)); problem == nil || result != nil {
		t.Fatal("refresh token target was supplied")
	}
	write("acct-one", "new", "")
	result, problem := w.refreshAuthTokens(json.RawMessage(`{"previousAccountId":"acct-one"}`))
	if problem != nil {
		t.Fatalf("new same-account token refused: %v", problem)
	}
	fields := result.(map[string]string)
	if fields["accessToken"] != "new" || fields["chatgptAccountId"] != "acct-one" || fields["chatgptPlanType"] != "pro" {
		t.Fatalf("response metadata: %+v", fields)
	}
	if result, problem := w.refreshAuthTokens(json.RawMessage(`{"previousAccountId":"acct-other"}`)); problem == nil || result != nil {
		t.Fatal("mismatched request account accepted")
	}
}

func TestWarmAuthAllowsAPIKeyOnlyWithoutRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	for _, test := range []struct {
		name, body string
		allowed    bool
	}{
		{"api key", `{"auth_mode":"apiKey","OPENAI_API_KEY":"test-key"}`, true},
		{"key and refresh", `{"auth_mode":"apiKey","OPENAI_API_KEY":"test-key","tokens":{"refresh_token":"test-refresh"}}`, false},
		{"chatgpt full", `{"auth_mode":"chatgpt","tokens":{"access_token":"test-access","refresh_token":"test-refresh","account_id":"acct"}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			err := warmAuthAllowed(path, adapter.TurnSpec{})
			if test.allowed && err != nil || !test.allowed && err == nil {
				t.Fatalf("warm auth eligibility: %v", err)
			}
		})
	}
}
