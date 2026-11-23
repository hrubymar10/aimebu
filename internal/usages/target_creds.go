package usages

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// credentialView is the token-level reading of one credential document, shared
// by the refresher's due check, its classification of a CLI run and its status
// rows. It is tolerant on purpose: a token-only live file (no refresh token) is
// a valid input, and the caller applies whichever rule (full or target) it needs.
type credentialView struct {
	Access      string
	Refresh     string
	ExpiresAt   *time.Time // claude: expiresAt; codex: the access token's exp claim
	LastRefresh *time.Time // codex only
}

func viewCredential(tool string, data []byte) (credentialView, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return credentialView{}, err
	}
	if tool == "codex" {
		var tokens map[string]json.RawMessage
		if raw, ok := root["tokens"]; ok {
			_ = json.Unmarshal(raw, &tokens)
		}
		v := credentialView{
			Access:  jsonString(tokens, "access_token", "accessToken"),
			Refresh: jsonString(tokens, "refresh_token", "refreshToken"),
		}
		v.ExpiresAt = CodexTokenExpiry(v.Access)
		var last string
		if raw, ok := root["last_refresh"]; ok {
			_ = json.Unmarshal(raw, &last)
		}
		v.LastRefresh = parseRFC3339(last)
		return v, nil
	}
	var tokens map[string]json.RawMessage
	for _, section := range []string{"claudeAiOauth", "claude_ai_oauth"} {
		raw, ok := root[section]
		if !ok {
			continue
		}
		var candidate map[string]json.RawMessage
		if json.Unmarshal(raw, &candidate) != nil {
			continue
		}
		if tokens == nil || jsonString(tokens, "accessToken", "access_token") == "" {
			tokens = candidate
		}
	}
	v := credentialView{
		Access:  jsonString(tokens, "accessToken", "access_token"),
		Refresh: jsonString(tokens, "refreshToken", "refresh_token"),
	}
	for _, key := range []string{"expiresAt", "expires_at"} {
		var ms float64
		if raw, ok := tokens[key]; ok && json.Unmarshal(raw, &ms) == nil && ms > 0 {
			t := time.UnixMilli(int64(ms)).UTC()
			v.ExpiresAt = &t
			break
		}
	}
	return v, nil
}

func jsonString(m map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var s string
		if raw, ok := m[key]; ok && json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func parseRFC3339(value string) *time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}

// tolerantExpiry reads a credential's expiry without demanding a refresh token,
// so a token-only live file is judged by its access token. Nil when unknown.
func tolerantExpiry(tool string, data []byte) *time.Time {
	v, err := viewCredential(tool, data)
	if err != nil {
		return nil
	}
	return v.ExpiresAt
}

// CredentialExpiry is tolerantExpiry for callers outside the package.
func CredentialExpiry(tool string, data []byte) *time.Time { return tolerantExpiry(tool, data) }

// accessTokenOf returns a credential document's access token ("" when it has
// none). It tells whether a profile already holds a given rotated login even when
// the provider kept the refresh token unchanged.
func accessTokenOf(tool string, data []byte) string {
	v, err := viewCredential(tool, data)
	if err != nil {
		return ""
	}
	return v.Access
}

// credentialAdvanced says whether candidate is a usable login newer than start:
// Claude by a later expiry with a non-empty refresh token, Codex by a different
// access token with a later last_refresh. It looks at the tokens that actually
// moved, never at the refresh token alone, because a provider may rotate the
// access token without rotating the refresh token.
func credentialAdvanced(tool string, start, candidate []byte) bool {
	old, err := viewCredential(tool, start)
	if err != nil {
		return false
	}
	next, err := viewCredential(tool, candidate)
	if err != nil || next.Refresh == "" {
		return false // a dead or dropped refresh token is not a usable login
	}
	if tool == "codex" {
		return old.Access != "" && next.Access != "" && next.Access != old.Access &&
			next.LastRefresh != nil && (old.LastRefresh == nil || next.LastRefresh.After(*old.LastRefresh))
	}
	return old.ExpiresAt != nil && next.ExpiresAt != nil && next.ExpiresAt.After(*old.ExpiresAt)
}

// CredentialAdvanced is credentialAdvanced for callers outside the package: the switcher's Codex overwrite check saves a credential a running
// process wrote back into its profile only when it is newer by the rule the refresher commits a rotation by.
func CredentialAdvanced(tool string, start, candidate []byte) bool {
	return credentialAdvanced(tool, start, candidate)
}

// validRotated reports whether data parses as a complete credential with a
// refresh token.
func validRotated(tool string, data []byte) bool {
	if _, err := refreshToken(data, tool); err != nil {
		return false
	}
	v, err := viewCredential(tool, data)
	if err != nil || v.Access == "" {
		return false
	}
	return tool == "codex" || v.ExpiresAt != nil
}

// claudeRefreshTokenField reports a claude credential document's refresh token
// and whether the key is present at all. Claude itself overwrites the refresh
// token with "" when the provider answers invalid_grant, so present-and-empty
// after a run is the signal that the account needs a new login.
func claudeRefreshTokenField(data []byte) (token string, present bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return "", false
	}
	for _, section := range []string{"claudeAiOauth", "claude_ai_oauth"} {
		raw, ok := root[section]
		if !ok {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		for _, key := range []string{"refreshToken", "refresh_token"} {
			if v, ok := fields[key]; ok {
				var s string
				_ = json.Unmarshal(v, &s)
				return s, true
			}
		}
	}
	return "", false
}

// forceExpireClaudeTempCredentials makes only the throwaway credential copy
// look expired so claude performs a real pre-emptive rotation. The stored
// profile remains byte-for-byte untouched until the rotated temp file passes
// validation and the commit's compare-and-swap. Raw JSON maps preserve fields
// aimebu does not interpret.
func forceExpireClaudeTempCredentials(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse credentials: %w", err)
	}
	updated := false
	for sectionName, expiryName := range map[string]string{
		"claudeAiOauth":   "expiresAt",
		"claude_ai_oauth": "expires_at",
	} {
		raw, ok := root[sectionName]
		if !ok {
			continue
		}
		var tokens map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return fmt.Errorf("parse %s: %w", sectionName, err)
		}
		if _, ok := tokens[expiryName]; !ok {
			continue
		}
		tokens[expiryName] = json.RawMessage("1")
		encoded, err := json.Marshal(tokens)
		if err != nil {
			return fmt.Errorf("encode %s: %w", sectionName, err)
		}
		root[sectionName] = encoded
		updated = true
	}
	if !updated {
		return errors.New("OAuth expiresAt field missing")
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	return atomicWriteBytes(path, encoded, 0o600)
}

// forceExpireCodexTempCredentials blanks the access token of the throwaway copy
// so codex refreshes it.
func forceExpireCodexTempCredentials(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err = json.Unmarshal(data, &root); err != nil {
		return err
	}
	var tokens map[string]json.RawMessage
	if err = json.Unmarshal(root["tokens"], &tokens); err != nil {
		return errors.New("codex OAuth tokens missing")
	}
	key := "access_token"
	if _, ok := tokens[key]; !ok {
		key = "accessToken"
		if _, ok := tokens[key]; !ok {
			return errors.New("codex access token missing")
		}
	}
	tokens[key] = json.RawMessage(`""`)
	raw, _ := json.Marshal(tokens)
	root["tokens"] = raw
	out, err := json.Marshal(root)
	if err != nil {
		return err
	}
	return atomicWriteBytes(path, out, 0o600)
}
