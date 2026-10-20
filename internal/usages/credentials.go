package usages

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// ClaudeLiveCredPath follows the same directory override as usage polling.
func ClaudeLiveCredPath(home string) string {
	dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, ".credentials.json")
}

func ClaudeAccountFilePath(home string) string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

// ValidateClaudeCredentials checks structure, while allowing expired tokens. It
// is the live-file rule: a refresh token is not required, because the live
// file is a token-only copy once the install runs token copies (use
// ValidateFullClaudeCredentials for a profile-store file).
func ValidateClaudeCredentials(path string) (*time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		OAuth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.OAuth.AccessToken == "" || raw.OAuth.ExpiresAt == 0 {
		return nil, errors.New("Claude OAuth credentials incomplete")
	}
	expiry := time.UnixMilli(raw.OAuth.ExpiresAt).UTC()
	return &expiry, nil
}

// IsClaudeTargetCopy reports whether path holds a token-only Claude copy: a
// usable access token and expiry but no refresh token. The server writes such
// copies to the live location; a full profile credential never looks like this.
func IsClaudeTargetCopy(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw struct {
		OAuth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return false
	}
	return raw.OAuth.AccessToken != "" && raw.OAuth.ExpiresAt != 0 && raw.OAuth.RefreshToken == ""
}

// IsCodexTargetCopy reports whether path holds a token-only Codex copy: an
// access token plus an explicitly empty refresh_token, which is how the
// refresher writes it. A file that merely lacks a refresh token is partial or
// corrupt, not a copy.
func IsCodexTargetCopy(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw struct {
		Tokens struct {
			AccessToken  string  `json:"access_token"`
			RefreshToken *string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return false
	}
	return raw.Tokens.AccessToken != "" && raw.Tokens.RefreshToken != nil && *raw.Tokens.RefreshToken == ""
}

// ValidateCodexCredentials is the live-file rule: an access token, plus either
// a refresh token or the explicitly empty refresh_token a token-only copy
// carries. Expiry is not checked. Use ValidateFullCodexCredentials for a
// profile-store file.
func ValidateCodexCredentials(path string) (idToken, accessToken string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var raw struct {
		Tokens struct {
			IDToken         string  `json:"id_token"`
			AccessToken     string  `json:"access_token"`
			RefreshToken    *string `json:"refresh_token"`
			AccessTokenAlt  string  `json:"accessToken"`
			RefreshTokenAlt string  `json:"refreshToken"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", err
	}
	access := first(raw.Tokens.AccessToken, raw.Tokens.AccessTokenAlt)
	var snake string
	if raw.Tokens.RefreshToken != nil {
		snake = *raw.Tokens.RefreshToken
	}
	refresh := first(snake, raw.Tokens.RefreshTokenAlt)
	targetCopy := raw.Tokens.RefreshToken != nil && snake == ""
	if access == "" || (refresh == "" && !targetCopy) {
		return "", "", errors.New("Codex OAuth credentials incomplete")
	}
	return raw.Tokens.IDToken, access, nil
}

func CodexIDTokenEmail(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(data, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.Email)
}

func CodexTokenExpiry(token string) *time.Time { return jwtExpiry(token) }

// ValidateFullClaudeCredentials validates a profile-store credentials file: the
// live-file rule (ValidateClaudeCredentials) plus a refresh token, because a
// stored credential must be able to refresh. The live file is a token-only
// copy once the install runs token copies.
func ValidateFullClaudeCredentials(path string) (*time.Time, error) {
	expiry, err := ValidateClaudeCredentials(path)
	if err != nil {
		return nil, err
	}
	token, err := fileRefreshToken(path, "claude")
	if err != nil || token == "" {
		return nil, errors.New("Claude refresh token missing (a stored profile must be a full credential)")
	}
	return expiry, nil
}

// ValidateFullCodexCredentials validates a profile-store auth.json: access and
// refresh token both required.
func ValidateFullCodexCredentials(path string) (idToken, accessToken string, err error) {
	idToken, accessToken, err = ValidateCodexCredentials(path)
	if err != nil {
		return "", "", err
	}
	token, rerr := fileRefreshToken(path, "codex")
	if rerr != nil || token == "" {
		return "", "", errors.New("Codex refresh token missing (a stored profile must be a full credential)")
	}
	return idToken, accessToken, nil
}

func fileRefreshToken(path, tool string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return refreshToken(data, tool)
}
