package providers

import (
	"net/http"
	"time"
)

// Login is the part of a live harness login that usage polling needs to decide
// whether to call the usage endpoint at all. It carries the access token so a
// caller can redact it from error text; never persist or log it.
type Login struct {
	AccessToken string
	// ExpiresAt is when the access token stops working, when the credentials
	// say so (Claude: expiresAt; Codex: the access token's JWT exp claim).
	ExpiresAt *time.Time
}

// ReadClaudeLogin reads a Claude Code .credentials.json without modifying it.
// The returned error text is safe to show: it never contains credential
// content.
func ReadClaudeLogin(path string) (Login, error) {
	creds, _, err := loadClaudeAuth(path)
	if err != nil {
		return Login{}, err
	}
	return Login{AccessToken: creds.AccessToken, ExpiresAt: creds.ExpiresAt}, nil
}

// ReadCodexLogin reads a Codex auth.json without modifying it. The returned
// error text is safe to show: it never contains credential content.
func ReadCodexLogin(path string) (Login, error) {
	creds, _, err := loadCodexAuth(path)
	if err != nil {
		return Login{}, err
	}
	login := Login{AccessToken: creds.AccessToken}
	if exp, ok := codexJWTClaims(creds.AccessToken)["exp"].(float64); ok && exp > 0 {
		t := time.Unix(int64(exp), 0).UTC()
		login.ExpiresAt = &t
	}
	return login, nil
}

// ParseClaudeLogin reads the same facts from credentials bytes the caller
// already read (a stored switcher profile, read under the switcher lock).
func ParseClaudeLogin(data []byte) (Login, error) {
	creds, _, err := parseClaudeAuthData(data)
	if err != nil {
		return Login{}, err
	}
	return Login{AccessToken: creds.AccessToken, ExpiresAt: creds.ExpiresAt}, nil
}

// ParseCodexLogin is ParseClaudeLogin for a Codex auth.json.
func ParseCodexLogin(data []byte) (Login, error) {
	creds, _, err := parseCodexAuthData(data)
	if err != nil {
		return Login{}, err
	}
	login := Login{AccessToken: creds.AccessToken}
	if exp, ok := codexJWTClaims(creds.AccessToken)["exp"].(float64); ok && exp > 0 {
		t := time.Unix(int64(exp), 0).UTC()
		login.ExpiresAt = &t
	}
	return login, nil
}

// NewUsageHTTPClient returns an HTTP client for usage endpoints that strips
// credential headers from cross-origin redirects.
func NewUsageHTTPClient(timeout time.Duration) *http.Client {
	client := newUsageHTTPClient()
	client.Timeout = timeout
	return client
}
