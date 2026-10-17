package credtarget

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages"
)

const (
	ToolClaude = "claude"
	ToolCodex  = "codex"
)

// ErrNotImplemented marks skeleton bodies that a piece owner still has to write.
var ErrNotImplemented = errors.New("credtarget: not implemented")

// Role says what a target is.
type Role string

const (
	// RoleLive is the host's own ~/.claude/.credentials.json or ~/.codex/auth.json
	// (the owner's interactive sessions and every worker share it).
	RoleLive Role = "live"
	// RoleExternal is a registered file elsewhere (none in P1).
	RoleExternal Role = "external"
)

// Target is a file that receives token-only rewrites.
type Target struct {
	ID   string // stable id: "live/claude", "live/codex", or the registered id
	Tool string // ToolClaude | ToolCodex
	Path string // absolute path of the credential file (.credentials.json / auth.json)
	Role Role
	// Profile pins the target to a switcher profile. Empty means "follows the
	// tool's active profile" (the live files).
	Profile string
}

// ConfigDir is the directory that holds the target file (and, for Claude, its
// lock files).
func (t Target) ConfigDir() string { return dirOf(t.Path) }

// Credential is the parsed, secret-bearing view of one credential document.
// It must never be logged or serialised.
type Credential struct {
	Tool         string
	AccessToken  string
	RefreshToken string // empty for a target
	AccountID    string // codex account id; empty for claude
	ExpiresAt    *time.Time
}

// ErrRefreshTokenInTarget is returned by ParseTarget (together with the parsed
// credential) when a target carries a non-empty refresh token: a /login landed in
// it and the reconciler must import and strip it.
var ErrRefreshTokenInTarget = errors.New("credtarget: target holds a refresh token")

// ParseFull parses a profile-store credential. Rule (C1): a FULL credential
// requires an access token AND a refresh token (claude also a non-zero expiresAt,
// the observed corruption marker).
func ParseFull(tool string, data []byte) (Credential, error) {
	cred, err := parse(tool, data)
	if err != nil {
		return Credential{}, err
	}
	if cred.AccessToken == "" {
		return Credential{}, errors.New("credtarget: access token missing")
	}
	if cred.RefreshToken == "" {
		return Credential{}, errors.New("credtarget: refresh token missing (not a full credential)")
	}
	if tool == ToolClaude && cred.ExpiresAt == nil {
		return Credential{}, errors.New("credtarget: expiresAt is zero (malformed)")
	}
	return cred, nil
}

// ParseTarget parses a live/external credential. Rule (C1): a TARGET requires an
// access token (codex: plus an account id, from the file or the JWT claims) and
// must not carry a non-empty refresh token: when it does, the parsed credential is
// returned together with ErrRefreshTokenInTarget so the reconciler can act on it.
func ParseTarget(tool string, data []byte) (Credential, error) {
	cred, err := parse(tool, data)
	if err != nil {
		return Credential{}, err
	}
	if cred.AccessToken == "" {
		return Credential{}, errors.New("credtarget: access token missing")
	}
	if tool == ToolCodex && cred.AccountID == "" {
		return Credential{}, errors.New("credtarget: codex account id missing")
	}
	if cred.RefreshToken != "" {
		return cred, ErrRefreshTokenInTarget
	}
	return cred, nil
}

// HasRefreshToken reports whether data carries a usable refresh token (claude:
// a non-empty `refreshToken`; codex: a non-empty `refresh_token`). Unparseable
// data reports false.
func HasRefreshToken(tool string, data []byte) bool {
	cred, err := parse(tool, data)
	return err == nil && cred.RefreshToken != ""
}

// parse extracts the token view without applying any rule.
func parse(tool string, data []byte) (Credential, error) {
	switch tool {
	case ToolClaude:
		return parseClaude(data)
	case ToolCodex:
		return parseCodex(data)
	}
	return Credential{}, fmt.Errorf("credtarget: unknown tool %q", tool)
}

func firstString(m map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var v string
		if raw, ok := m[key]; ok && json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseClaude(data []byte) (Credential, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return Credential{}, errors.New("credtarget: claude credentials are not valid JSON")
	}
	cred := Credential{Tool: ToolClaude}
	for _, section := range []string{"claudeAiOauth", "claude_ai_oauth"} {
		raw, ok := root[section]
		if !ok {
			continue
		}
		var tokens map[string]json.RawMessage
		if json.Unmarshal(raw, &tokens) != nil {
			continue
		}
		access := firstString(tokens, "accessToken", "access_token")
		if access == "" && cred.AccessToken != "" {
			continue
		}
		cred.AccessToken = access
		cred.RefreshToken = firstString(tokens, "refreshToken", "refresh_token")
		for _, key := range []string{"expiresAt", "expires_at"} {
			var ms float64
			if raw, ok := tokens[key]; ok && json.Unmarshal(raw, &ms) == nil && ms != 0 {
				t := time.UnixMilli(int64(ms)).UTC()
				cred.ExpiresAt = &t
				break
			}
		}
		if access != "" {
			break
		}
	}
	return cred, nil
}

func parseCodex(data []byte) (Credential, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return Credential{}, errors.New("credtarget: codex auth.json is not valid JSON")
	}
	cred := Credential{Tool: ToolCodex}
	var tokens map[string]json.RawMessage
	if raw, ok := root["tokens"]; ok {
		_ = json.Unmarshal(raw, &tokens)
	}
	cred.AccessToken = firstString(tokens, "access_token", "accessToken")
	cred.RefreshToken = firstString(tokens, "refresh_token", "refreshToken")
	idToken := firstString(tokens, "id_token", "idToken")
	cred.AccountID = firstString(tokens, "account_id", "accountId")
	if cred.AccountID == "" {
		cred.AccountID = firstNonEmpty(jwtAccountID(idToken), jwtAccountID(cred.AccessToken))
	}
	cred.ExpiresAt = usages.CodexTokenExpiry(cred.AccessToken)
	return cred, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// jwtAccountID reads the ChatGPT account id from a JWT's claims, in the shapes
// codex writes (flat claim, or nested under the https://api.openai.com/auth
// object).
func jwtAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	for _, key := range []string{"https://api.openai.com/auth.chatgpt_account_id", "chatgpt_account_id", "account_id"} {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		for _, key := range []string{"chatgpt_account_id", "account_id"} {
			if v, ok := auth[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}
