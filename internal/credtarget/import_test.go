package credtarget

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChooseImportOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 21, 30, 5, 0, time.UTC)
	for _, test := range []struct {
		name string
		env  ImportEnv
		want ImportDecision
	}{
		{
			name: "empty active profile gets the login even when the account is known elsewhere",
			env: ImportEnv{Tool: ToolClaude, ActiveProfile: "main", ActiveEmpty: true, LiveAccount: "bob@example.test",
				ProfileAccount: map[string]string{"backup": "bob@example.test"}},
			want: ImportDecision{Profile: "main", Reason: ReasonActiveEmpty},
		},
		{
			name: "same account as the active profile replaces it",
			env: ImportEnv{Tool: ToolClaude, ActiveProfile: "main", LiveAccount: " Alice@Example.test ",
				ProfileAccount: map[string]string{"main": "alice@example.test", "other": "bob@example.test"}},
			want: ImportDecision{Profile: "main", Reason: ReasonSameAccount},
		},
		{
			name: "another known account replaces that profile, not the active one",
			env: ImportEnv{Tool: ToolClaude, ActiveProfile: "main", LiveAccount: "bob@example.test",
				ProfileAccount: map[string]string{"main": "alice@example.test", "other": "bob@example.test"}},
			want: ImportDecision{Profile: "other", Reason: ReasonEmailMatch},
		},
		{
			name: "unknown account becomes a new login profile",
			env: ImportEnv{Tool: ToolClaude, ActiveProfile: "main", LiveAccount: "carol@example.test", Now: now,
				ProfileAccount: map[string]string{"main": "alice@example.test", "other": "bob@example.test"}},
			want: ImportDecision{Profile: "login-20261016T213005", Create: true, Reason: ReasonNewLogin},
		},
		{
			name: "an unidentifiable login never claims an existing profile",
			env: ImportEnv{Tool: ToolClaude, ActiveProfile: "main", LiveAccount: "", Now: now,
				ProfileAccount: map[string]string{"main": "", "other": ""}},
			want: ImportDecision{Profile: "login-20261016T213005", Create: true, Reason: ReasonNewLogin, Note: "the login's account could not be identified"},
		},
		{
			name: "a profile with an unknown account is not a match for an unknown login",
			env: ImportEnv{Tool: ToolClaude, ActiveProfile: "main", LiveAccount: "", Now: now,
				ProfileAccount: map[string]string{"main": ""}},
			want: ImportDecision{Profile: "login-20261016T213005", Create: true, Reason: ReasonNewLogin, Note: "the login's account could not be identified"},
		},
		{
			name: "two profiles holding the account is ambiguous and keeps the login separate",
			env: ImportEnv{Tool: ToolClaude, ActiveProfile: "main", LiveAccount: "bob@example.test", Now: now,
				ProfileAccount: map[string]string{"main": "alice@example.test", "b1": "bob@example.test", "b2": "bob@example.test"}},
			want: ImportDecision{Profile: "login-20261016T213005", Create: true, Reason: ReasonNewLogin, Note: "2 profiles hold this account"},
		},
		{
			name: "no active profile and no match creates a login profile",
			env:  ImportEnv{Tool: ToolCodex, LiveAccount: "acct:acct_1", Now: now, ProfileAccount: map[string]string{}},
			want: ImportDecision{Profile: "login-20261016T213005", Create: true, Reason: ReasonNewLogin},
		},
		{
			name: "codex same account by id",
			env: ImportEnv{Tool: ToolCodex, ActiveProfile: "work", LiveAccount: "acct:acct_1",
				ProfileAccount: map[string]string{"work": "acct:acct_1"}},
			want: ImportDecision{Profile: "work", Reason: ReasonSameAccount},
		},
		{
			name: "codex other known account by email",
			env: ImportEnv{Tool: ToolCodex, ActiveProfile: "work", LiveAccount: "pat@example.test",
				ProfileAccount: map[string]string{"work": "kim@example.test", "home": "pat@example.test"}},
			want: ImportDecision{Profile: "home", Reason: ReasonEmailMatch},
		},
		{
			name: "codex empty active profile",
			env:  ImportEnv{Tool: ToolCodex, ActiveProfile: "work", ActiveEmpty: true, LiveAccount: "pat@example.test"},
			want: ImportDecision{Profile: "work", Reason: ReasonActiveEmpty},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ChooseImport(test.env)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("decision = %+v, want %+v", got, test.want)
			}
		})
	}
	if _, err := ChooseImport(ImportEnv{Tool: "vibe"}); err == nil {
		t.Fatal("an unknown tool must be rejected")
	}
}

func TestClaudeAccountEmailIsBestEffort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if got := ClaudeAccountEmail(path); got != "" {
		t.Fatalf("missing file = %q", got)
	}
	if err := os.WriteFile(path, []byte(`{"numStartups":3,"oauthAccount":{"emailAddress":"alice@example.test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeAccountEmail(path); got != "alice@example.test" {
		t.Fatalf("email = %q", got)
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeAccountEmail(path); got != "" {
		t.Fatalf("corrupt file = %q", got)
	}
}

func TestCodexAccountPrefersEmailThenAccountID(t *testing.T) {
	t.Parallel()
	withEmail := `{"tokens":{"id_token":"` + fakeJWTForEmail("pat@example.test") + `","account_id":"acct_9"}}`
	if got := CodexAccount([]byte(withEmail)); got != "pat@example.test" {
		t.Fatalf("with email = %q", got)
	}
	if got := CodexAccount([]byte(`{"tokens":{"id_token":"","account_id":"acct_9"}}`)); got != "acct:acct_9" {
		t.Fatalf("account id only = %q", got)
	}
	if got := CodexAccount([]byte(`{"tokens":{}}`)); got != "" {
		t.Fatalf("nothing = %q", got)
	}
	if got := CodexAccount([]byte(`nope`)); got != "" {
		t.Fatalf("corrupt = %q", got)
	}
}

// fakeIDToken builds an unsigned JWT-shaped string whose payload carries email.
func fakeJWTForEmail(email string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(`{"email":"`+email+`"}`) + ".sig"
}
