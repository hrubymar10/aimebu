package credtarget

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Fake credentials only: nothing here is, or looks like, a real token.
const (
	fakeRefresh   = "FAKE-REFRESH-TOKEN-DO-NOT-LEAK"
	fakeAccessA   = "FAKE-ACCESS-A"
	fakeAccessB   = "FAKE-ACCESS-B"
	bwIDToken     = "FAKE.ID.TOKEN"
	fakeAccount   = "fake-account-id"
	fullClaudeA   = `{"claudeAiOauth":{"accessToken":"` + fakeAccessA + `","refreshToken":"` + fakeRefresh + `","expiresAt":1792153600000,"scopes":["user:inference","user:profile"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x","refreshTokenExpiresAt":1792999999000}}`
	fullClaudeBNo = `{"claudeAiOauth":{"accessToken":"` + fakeAccessB + `","refreshToken":"` + fakeRefresh + `-B","expiresAt":1792160000000,"scopes":["user:inference"],"subscriptionType":"pro"}}`
)

// A realistic target: Claude's own pretty-printed file with the login plus the
// MCP OAuth tokens and a gateway pin that the server must never touch.
const claudeTargetBefore = `{
  "claudeAiOauth": {
    "accessToken": "OLD-ACCESS",
    "expiresAt": 1700000000000,
    "scopes": ["old:scope"],
    "subscriptionType": "team",
    "rateLimitTier": "old_tier",
    "refreshTokenExpiresAt": 1700000001000,
    "futureField": {"keep":  "me"}
  },
  "mcpOAuth": {
    "plane|abc": {"serverName": "plane", "accessToken": "MCP-A", "expiresAt": 1.5e12, "note": "café"},
    "grafana|def": {"serverName": "grafana", "accessToken": "MCP-G"}
  },
  "gatewayPin": "pinned:\"quotes\"",
  "z": [1,   2, 3]
}
`

func parsed(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, data)
	}
	return out
}

func section(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	s, ok := doc[name].(map[string]any)
	if !ok {
		t.Fatalf("no %q section in %v", name, doc)
	}
	return s
}

func TestBuildFullTargetRestoresRefreshAndKeepsOtherFields(t *testing.T) {
	claude, err := BuildFullTarget(ToolClaude, []byte(claudeTargetBefore), []byte(fullClaudeA))
	if err != nil {
		t.Fatal(err)
	}
	if got := section(t, parsed(t, claude), "claudeAiOauth")["refreshToken"]; got != fakeRefresh {
		t.Fatalf("claude refresh = %v", got)
	}
	if !bytes.Contains(claude, []byte(`"mcpOAuth"`)) || !bytes.Contains(claude, []byte(`"gatewayPin"`)) {
		t.Fatal("unrelated Claude fields lost")
	}
	codexFull := []byte(`{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"access","refresh_token":"refresh","account_id":"acct"}}`)
	codex, err := BuildFullTarget(ToolCodex, []byte(`{"tokens":{"access_token":"old","refresh_token":""},"OPENAI_API_KEY":"keep"}`), codexFull)
	if err != nil {
		t.Fatal(err)
	}
	if got := section(t, parsed(t, codex), "tokens")["refresh_token"]; got != "refresh" {
		t.Fatalf("codex refresh = %v", got)
	}
	if parsed(t, codex)["OPENAI_API_KEY"] != "keep" {
		t.Fatal("unrelated Codex field lost")
	}
}

func TestClaudeTargetNeverCarriesARefreshToken(t *testing.T) {
	t.Parallel()
	withLogin := `{"claudeAiOauth":{"accessToken":"old","refreshToken":"OLD-RT-FROM-A-LOGIN"},"mcpOAuth":{"a":1}}`
	for _, existing := range [][]byte{nil, []byte(""), []byte("  \n"), []byte(claudeTargetBefore), []byte(`{}`), []byte(withLogin)} {
		got, err := BuildClaudeTarget(existing, []byte(fullClaudeA))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(got, []byte(fakeRefresh)) || bytes.Contains(got, []byte("OLD-RT")) || bytes.Contains(got, []byte(`"refreshToken"`)) {
			t.Fatalf("target carries a refresh token:\n%s", got)
		}
		oauth := section(t, parsed(t, got), "claudeAiOauth")
		if _, present := oauth["refreshToken"]; present {
			t.Fatalf("refreshToken key present (must be deleted, not blank):\n%s", got)
		}
		if oauth["accessToken"] != fakeAccessA {
			t.Fatalf("access token not written: %v", oauth)
		}
	}
}

func TestClaudeTargetReplacesTheLoginFieldsAndKeepsEverythingElseVerbatim(t *testing.T) {
	t.Parallel()
	got, err := BuildClaudeTarget([]byte(claudeTargetBefore), []byte(fullClaudeA))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "claudeAiOauth": {
    "accessToken": "` + fakeAccessA + `",
    "expiresAt": 1792153600000,
    "scopes": ["user:inference","user:profile"],
    "subscriptionType": "max",
    "rateLimitTier": "default_claude_max_20x",
    "refreshTokenExpiresAt": 1792999999000,
    "futureField": {"keep":  "me"}
  },
` + claudeTargetBefore[strings.Index(claudeTargetBefore, `  "mcpOAuth"`):]
	if string(got) != want {
		t.Fatalf("target differs from the expected document\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestClaudeTargetDropsLoginFieldsTheNewCredentialLacks(t *testing.T) {
	t.Parallel()
	// The incoming credential has no rateLimitTier and no refreshTokenExpiresAt;
	// the old ones belonged to another account.
	got, err := BuildClaudeTarget([]byte(claudeTargetBefore), []byte(fullClaudeBNo))
	if err != nil {
		t.Fatal(err)
	}
	oauth := section(t, parsed(t, got), "claudeAiOauth")
	for _, stale := range []string{"rateLimitTier", "refreshTokenExpiresAt"} {
		if _, present := oauth[stale]; present {
			t.Errorf("stale %s survived the switch: %v", stale, oauth)
		}
	}
	if oauth["subscriptionType"] != "pro" || oauth["futureField"] == nil {
		t.Errorf("section content wrong: %v", oauth)
	}
}

func TestClaudeTargetIsIdempotent(t *testing.T) {
	t.Parallel()
	once, err := BuildClaudeTarget([]byte(claudeTargetBefore), []byte(fullClaudeA))
	if err != nil {
		t.Fatal(err)
	}
	twice, err := BuildClaudeTarget(once, []byte(fullClaudeA))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("a second build changed the document:\n%s\n---\n%s", once, twice)
	}
}

func TestClaudeTargetFromNothingIsAWellFormedDocument(t *testing.T) {
	t.Parallel()
	got, err := BuildClaudeTarget(nil, []byte(fullClaudeA))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"claudeAiOauth\": {\n    \"accessToken\": \"" + fakeAccessA + "\",\n    \"expiresAt\": 1792153600000,\n    \"scopes\": [\"user:inference\",\"user:profile\"],\n    \"subscriptionType\": \"max\",\n    \"rateLimitTier\": \"default_claude_max_20x\",\n    \"refreshTokenExpiresAt\": 1792999999000\n  }\n}"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestClaudeTargetAddsTheSectionToADocumentWithoutOne(t *testing.T) {
	t.Parallel()
	got, err := BuildClaudeTarget([]byte("{\n  \"mcpOAuth\": {}\n}\n"), []byte(fullClaudeA))
	if err != nil {
		t.Fatal(err)
	}
	doc := parsed(t, got)
	if doc["mcpOAuth"] == nil || section(t, doc, "claudeAiOauth")["accessToken"] != fakeAccessA {
		t.Fatalf("got %s", got)
	}
	if !strings.HasPrefix(string(got), "{\n  \"mcpOAuth\": {},\n  \"claudeAiOauth\": {\n    \"accessToken\"") || !strings.HasSuffix(string(got), "  }\n}\n") {
		t.Fatalf("section not laid out like the document:\n%s", got)
	}
}

func TestClaudeTargetNormalisesTheLegacySpelling(t *testing.T) {
	t.Parallel()
	legacyFull := `{"claude_ai_oauth":{"access_token":"` + fakeAccessB + `","refresh_token":"` + fakeRefresh + `","expires_at":1792160000000,"subscription_type":"pro"}}`
	for _, existing := range []string{
		`{"claude_ai_oauth":{"access_token":"old","refresh_token":"OLD-RT"},"mcpOAuth":{"a":1}}`,
		`{"claudeAiOauth":{"accessToken":"old"},"claude_ai_oauth":{"access_token":"older"},"mcpOAuth":{"a":1}}`,
	} {
		got, err := BuildClaudeTarget([]byte(existing), []byte(legacyFull))
		if err != nil {
			t.Fatal(err)
		}
		doc := parsed(t, got)
		if _, present := doc["claude_ai_oauth"]; present {
			t.Errorf("legacy section left behind: %s", got)
		}
		oauth := section(t, doc, "claudeAiOauth")
		if oauth["accessToken"] != fakeAccessB || oauth["expiresAt"] != float64(1792160000000) || oauth["subscriptionType"] != "pro" {
			t.Errorf("fields not normalised: %v", oauth)
		}
		if bytes.Contains(got, []byte(fakeRefresh)) || bytes.Contains(got, []byte("OLD-RT")) || bytes.Contains(got, []byte("refresh_token")) {
			t.Errorf("refresh token survived: %s", got)
		}
		if doc["mcpOAuth"] == nil {
			t.Errorf("unrelated key lost: %s", got)
		}
	}
}

func TestClaudeTargetRefusesWhatItCannotTrust(t *testing.T) {
	t.Parallel()
	bad := []struct{ name, existing, full string }{
		{"existing not JSON", `{"a":`, fullClaudeA},
		{"existing not an object", `[1]`, fullClaudeA},
		{"full not JSON", ``, `{`},
		{"full without a section", ``, `{"other":1}`},
		{"full without an access token", ``, `{"claudeAiOauth":{"refreshToken":"x"}}`},
		{"full with a blank access token", ``, `{"claudeAiOauth":{"accessToken":""}}`},
	}
	for _, tc := range bad {
		if _, err := BuildClaudeTarget([]byte(tc.existing), []byte(tc.full)); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
}

const (
	fullCodexA  = `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"` + bwIDToken + `","access_token":"` + fakeAccessA + `","refresh_token":"` + fakeRefresh + `","account_id":"` + fakeAccount + `"},"last_refresh":"2026-10-16T12:00:00.000000Z"}`
	codexBefore = "{\n  \"auth_mode\": \"chatgpt\",\n  \"OPENAI_API_KEY\": \"sk-FAKE\",\n  \"tokens\": {\n    \"id_token\": \"OLD.ID\",\n    \"access_token\": \"OLD-ACCESS\",\n    \"refresh_token\": \"OLD-RT\",\n    \"account_id\": \"old-account\",\n    \"extra\": [1, 2]\n  },\n  \"last_refresh\": \"2020-01-01T00:00:00Z\",\n  \"custom\": {\"keep\":  true}\n}\n"
)

func TestCodexTargetKeepsAnEmptyRefreshTokenAndSwitchesMode(t *testing.T) {
	t.Parallel()
	for _, existing := range [][]byte{nil, []byte(codexBefore), []byte(`{}`), []byte(`{"tokens":null}`)} {
		got, err := BuildCodexTarget(existing, []byte(fullCodexA))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(got, []byte(fakeRefresh)) || bytes.Contains(got, []byte("OLD-RT")) {
			t.Fatalf("target carries a refresh token:\n%s", got)
		}
		doc := parsed(t, got)
		tokens := section(t, doc, "tokens")
		if rt, present := tokens["refresh_token"]; !present || rt != "" {
			t.Fatalf("refresh_token must be present and empty (deleting it logs codex out): %v", tokens)
		}
		if doc["auth_mode"] != "chatgptAuthTokens" {
			t.Fatalf("auth_mode = %v", doc["auth_mode"])
		}
		if tokens["access_token"] != fakeAccessA || tokens["id_token"] != bwIDToken || tokens["account_id"] != fakeAccount {
			t.Fatalf("tokens not written: %v", tokens)
		}
		if doc["last_refresh"] != "2026-10-16T12:00:00.000000Z" {
			t.Fatalf("last_refresh = %v", doc["last_refresh"])
		}
	}
}

func TestCodexTargetKeepsEverythingItDoesNotOwnVerbatim(t *testing.T) {
	t.Parallel()
	got, err := BuildCodexTarget([]byte(codexBefore), []byte(fullCodexA))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"auth_mode\": \"chatgptAuthTokens\",\n  \"OPENAI_API_KEY\": \"sk-FAKE\",\n  \"tokens\": {\n    \"id_token\": \"" + bwIDToken + "\",\n    \"access_token\": \"" + fakeAccessA + "\",\n    \"refresh_token\": \"\",\n    \"account_id\": \"" + fakeAccount + "\",\n    \"extra\": [1, 2]\n  },\n  \"last_refresh\": \"2026-10-16T12:00:00.000000Z\",\n  \"custom\": {\"keep\":  true}\n}\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCodexTargetIsIdempotentAndHandlesCamelCaseInput(t *testing.T) {
	t.Parallel()
	camel := `{"tokens":{"idToken":"` + bwIDToken + `","accessToken":"` + fakeAccessB + `","refreshToken":"` + fakeRefresh + `","accountId":"` + fakeAccount + `"}}`
	once, err := BuildCodexTarget([]byte(codexBefore), []byte(camel))
	if err != nil {
		t.Fatal(err)
	}
	twice, err := BuildCodexTarget(once, []byte(camel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("second build changed the document:\n%s\n---\n%s", once, twice)
	}
	tokens := section(t, parsed(t, once), "tokens")
	for _, key := range []string{"idToken", "accessToken", "refreshToken", "accountId"} {
		if _, present := tokens[key]; present {
			t.Errorf("camelCase key %s left next to the snake_case ones: %v", key, tokens)
		}
	}
	if tokens["access_token"] != fakeAccessB || tokens["account_id"] != fakeAccount {
		t.Errorf("tokens = %v", tokens)
	}
	if _, present := parsed(t, once)["last_refresh"]; present {
		t.Errorf("a stale last_refresh survived although the new credential has none")
	}
}

func TestCodexTargetRefusesWhatItCannotTrust(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, existing, full string }{
		{"existing not JSON", `{`, fullCodexA},
		{"full without tokens", ``, `{"auth_mode":"chatgpt"}`},
		{"full without an access token", ``, `{"tokens":{"refresh_token":"x"}}`},
		{"full not an object", ``, `[]`},
	} {
		if _, err := BuildCodexTarget([]byte(tc.existing), []byte(tc.full)); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
}

func TestStripRefreshTokenTouchesNothingElse(t *testing.T) {
	t.Parallel()
	claude := "{\n  \"claudeAiOauth\": {\n    \"accessToken\": \"A\",\n    \"refreshToken\": \"" + fakeRefresh + "\",\n    \"expiresAt\": 1\n  },\n  \"mcpOAuth\": {\"x\":  1}\n}\n"
	got, err := StripRefreshToken(ToolClaude, []byte(claude))
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"claudeAiOauth\": {\n    \"accessToken\": \"A\",\n    \"expiresAt\": 1\n  },\n  \"mcpOAuth\": {\"x\":  1}\n}\n"; string(got) != want {
		t.Fatalf("claude:\n got %q\nwant %q", got, want)
	}
	again, _ := StripRefreshToken(ToolClaude, got)
	if !bytes.Equal(got, again) {
		t.Fatal("claude strip is not idempotent")
	}

	codex := "{\n  \"tokens\": {\n    \"access_token\": \"A\",\n    \"refresh_token\": \"" + fakeRefresh + "\"\n  },\n  \"x\": 1\n}\n"
	got, err = StripRefreshToken(ToolCodex, []byte(codex))
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"tokens\": {\n    \"access_token\": \"A\",\n    \"refresh_token\": \"\"\n  },\n  \"x\": 1\n}\n"; string(got) != want {
		t.Fatalf("codex:\n got %q\nwant %q", got, want)
	}
	// Codex without the key gets the blank one; a camelCase spelling goes away.
	got, _ = StripRefreshToken(ToolCodex, []byte(`{"tokens":{"access_token":"A","refreshToken":"`+fakeRefresh+`"}}`))
	if string(got) != `{"tokens":{"access_token":"A","refresh_token":""}}` {
		t.Fatalf("codex without the key: %s", got)
	}
	// Nothing to strip: unchanged. Unknown tool and bad JSON: error.
	same, _ := StripRefreshToken(ToolClaude, []byte(`{"other":1}`))
	if string(same) != `{"other":1}` {
		t.Fatalf("no-op changed the document: %s", same)
	}
	if _, err := StripRefreshToken("mistral", []byte(`{}`)); err == nil {
		t.Error("unknown tool accepted")
	}
	if _, err := StripRefreshToken(ToolClaude, []byte(`{`)); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestBuildTargetRoutesByTool(t *testing.T) {
	t.Parallel()
	if _, err := BuildTarget(ToolClaude, nil, []byte(fullClaudeA)); err != nil {
		t.Error(err)
	}
	if _, err := BuildTarget(ToolCodex, nil, []byte(fullCodexA)); err != nil {
		t.Error(err)
	}
	if _, err := BuildTarget("mistral", nil, []byte(`{}`)); err == nil {
		t.Error("unknown tool accepted")
	}
}
