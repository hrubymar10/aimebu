package credtarget

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

func jwt(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(claims)) + ".sig"
}

func claudeDoc(refresh string, expiresMs int64) []byte {
	r := ""
	if refresh != "" {
		r = fmt.Sprintf(`,"refreshToken":%q`, refresh)
	}
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"acc","expiresAt":%d%s,"scopes":["a"]},"mcpOAuth":{"x":1}}`, expiresMs, r))
}

func codexDoc(refresh, account string) []byte {
	acct := ""
	if account != "" {
		acct = fmt.Sprintf(`,"account_id":%q`, account)
	}
	return []byte(fmt.Sprintf(`{"tokens":{"access_token":"acc","refresh_token":%q%s}}`, refresh, acct))
}

// C1: the profile store holds FULL credentials (access + refresh); a target
// (live/external file) is token-only.
func TestParseFullRequiresAccessAndRefresh(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	if _, err := ParseFull(ToolClaude, claudeDoc("r", future)); err != nil {
		t.Fatalf("full claude rejected: %v", err)
	}
	if _, err := ParseFull(ToolClaude, claudeDoc("", future)); err == nil {
		t.Fatal("a token-only claude file passed as a full credential")
	}
	if _, err := ParseFull(ToolClaude, claudeDoc("r", 0)); err == nil {
		t.Fatal("expiresAt==0 (the corruption marker) passed as a full credential")
	}
	if _, err := ParseFull(ToolCodex, codexDoc("r", "acct")); err != nil {
		t.Fatalf("full codex rejected: %v", err)
	}
	if _, err := ParseFull(ToolCodex, codexDoc("", "acct")); err == nil {
		t.Fatal("a token-only codex file (refresh_token \"\") passed as a full credential")
	}
	if _, err := ParseFull("pi", codexDoc("r", "a")); err == nil {
		t.Fatal("unknown tool accepted")
	}
	if _, err := ParseFull(ToolClaude, []byte("{not json")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestParseTargetAcceptsTokenOnlyAndFlagsRefreshTokens(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	c, err := ParseTarget(ToolClaude, claudeDoc("", future))
	if err != nil || c.AccessToken != "acc" || c.RefreshToken != "" || c.ExpiresAt == nil {
		t.Fatalf("token-only claude target: %+v %v", c, err)
	}
	// A refresh token in a target is reported with the parsed credential.
	c, err = ParseTarget(ToolClaude, claudeDoc("r", future))
	if !errors.Is(err, ErrRefreshTokenInTarget) || c.RefreshToken != "r" {
		t.Fatalf("claude target with a refresh token: %+v %v", c, err)
	}
	// codex: refresh_token present-and-empty is the target form; account id is required.
	c, err = ParseTarget(ToolCodex, codexDoc("", "acct"))
	if err != nil || c.AccountID != "acct" {
		t.Fatalf("token-only codex target: %+v %v", c, err)
	}
	if _, err := ParseTarget(ToolCodex, codexDoc("", "")); err == nil {
		t.Fatal("a codex target without an account id was accepted")
	}
	if _, err := ParseTarget(ToolCodex, codexDoc("r", "acct")); !errors.Is(err, ErrRefreshTokenInTarget) {
		t.Fatalf("codex target with a refresh token: %v", err)
	}
	// The codex account id may come from the id_token claims.
	doc := []byte(fmt.Sprintf(`{"tokens":{"access_token":"acc","refresh_token":"","id_token":%q}}`, jwt(`{"https://api.openai.com/auth":{"chatgpt_account_id":"from-jwt"}}`)))
	if c, err := ParseTarget(ToolCodex, doc); err != nil || c.AccountID != "from-jwt" {
		t.Fatalf("jwt account id: %+v %v", c, err)
	}
	if _, err := ParseTarget(ToolClaude, []byte(`{"claudeAiOauth":{}}`)); err == nil {
		t.Fatal("a target without an access token was accepted")
	}
}

func TestHasRefreshToken(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	for _, tc := range []struct {
		tool string
		doc  []byte
		want bool
	}{
		{ToolClaude, claudeDoc("r", future), true},
		{ToolClaude, claudeDoc("", future), false},
		{ToolCodex, codexDoc("r", "a"), true},
		{ToolCodex, codexDoc("", "a"), false},
		{ToolCodex, []byte("{broken"), false},
	} {
		if got := HasRefreshToken(tc.tool, tc.doc); got != tc.want {
			t.Errorf("HasRefreshToken(%s) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}
