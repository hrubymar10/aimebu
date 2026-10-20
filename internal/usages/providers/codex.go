package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/goccy/go-json"
)

const (
	codexUsageURL      = "https://chatgpt.com/backend-api/wham/usage"
	codexLoginRequired = "Codex CLI is not signed in. Run `codex login --device-auth`, then refresh."
)

// CodexOptions overrides where the Codex provider reads credentials and how it
// reaches the usage endpoint. Zero fields use the defaults:
// <CODEX_HOME or ~/.codex>/auth.json, the ChatGPT usage URL and the shared
// usage HTTP client.
type CodexOptions struct {
	AuthPath string
	// AuthData, when set, replaces reading AuthPath: the caller read the
	// bytes (under the switcher lock for a stored profile). The re-read after
	// a 401 is skipped, since the stored copy is not the file the CLI rotates.
	AuthData []byte
	URL      string
	Client   *http.Client
}

// codexProvider is read-only: it never refreshes the OAuth token and never
// writes auth.json. When the usage endpoint rejects the token it re-reads the
// file once, in case the codex CLI rotated it meanwhile.
type codexProvider struct{ opts CodexOptions }

// NewCodexProvider creates a codex usage provider.
func NewCodexProvider() UsageProvider { return &codexProvider{} }

// NewCodexProviderWith builds the provider with explicit overrides.
func NewCodexProviderWith(opts CodexOptions) UsageProvider { return &codexProvider{opts: opts} }

func (p *codexProvider) Key() string { return ProviderCodex }

func (p *codexProvider) usageURL() string {
	if p.opts.URL != "" {
		return p.opts.URL
	}
	return codexUsageURL
}

func (p *codexProvider) client() *http.Client {
	if p.opts.Client != nil {
		return p.opts.Client
	}
	return usageHTTPClient
}

func (p *codexProvider) Fetch(ctx context.Context, store *Store) (Snapshot, error) {
	authPath := p.opts.AuthPath
	var err error
	var auth codexAuthSnapshot
	var detail *ErrorDetail
	if p.opts.AuthData != nil {
		var creds codexCredentials
		creds, detail, err = parseCodexAuthData(p.opts.AuthData)
		auth = codexAuthSnapshot{Credentials: creds}
	} else {
		if authPath == "" {
			authPath, err = codexAuthPath()
		}
		if err != nil {
			return codexStatus(StatusAuthMissing, "Codex auth path unavailable.", nil), nil
		}
		auth, detail, err = loadCodexAuthSnapshot(authPath)
	}
	if err != nil {
		return codexStatus(StatusAuthMissing, err.Error(), detail), nil
	}
	creds := auth.Credentials
	raw, detail, status, err := p.fetchUsage(ctx, creds)
	if err != nil && status == StatusAuthMissing && p.opts.AuthData == nil {
		reloaded, reloadDetail, changed, reloadErr := reloadCodexAuthIfChanged(authPath, auth.Fingerprint)
		if changed {
			if reloadErr != nil {
				return codexStatus(StatusAuthMissing, reloadErr.Error(), reloadDetail), nil
			}
			creds = reloaded
			raw, detail, status, err = p.fetchUsage(ctx, creds)
		}
	}
	if err != nil {
		snap := codexStatus(status, err.Error(), detail)
		if status == StatusFetchError {
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		return snap, nil
	}
	snap, detail, err := normalizeCodexUsage(raw, creds)
	if err != nil {
		snap = codexStatus(StatusFetchError, err.Error(), detail)
		return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
	}
	if detail != nil && len(detail.Fields) > 0 {
		snap.ErrorDetail = detail
	}
	return snap, nil
}

type codexAuthFile struct {
	OpenAIAPIKey string          `json:"OPENAI_API_KEY,omitempty"`
	Tokens       codexAuthTokens `json:"tokens"`
	LastRefresh  string          `json:"last_refresh,omitempty"`
}

type codexAuthTokens struct {
	AccessToken   string `json:"access_token,omitempty"`
	AccessTokenC  string `json:"accessToken,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	RefreshTokenC string `json:"refreshToken,omitempty"`
	IDToken       string `json:"id_token,omitempty"`
	IDTokenC      string `json:"idToken,omitempty"`
	AccountID     string `json:"account_id,omitempty"`
	AccountIDC    string `json:"accountId,omitempty"`
}

type codexCredentials struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
	LastRefresh  *time.Time
}

func codexAuthPath() (string, error) {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

type codexAuthSnapshot struct {
	Credentials codexCredentials
	Fingerprint []byte
}

func loadCodexAuthSnapshot(path string) (codexAuthSnapshot, *ErrorDetail, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codexAuthSnapshot{}, fieldDetail("auth.json", "missing"), errors.New(codexLoginRequired)
		}
		return codexAuthSnapshot{}, nil, errors.New("Codex auth.json could not be read.")
	}
	creds, detail, err := parseCodexAuthData(data)
	if err != nil {
		return codexAuthSnapshot{}, detail, err
	}
	return codexAuthSnapshot{Credentials: creds, Fingerprint: append([]byte(nil), data...)}, nil, nil
}

func loadCodexAuth(path string) (codexCredentials, *ErrorDetail, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codexCredentials{}, fieldDetail("auth.json", "missing"), errors.New(codexLoginRequired)
		}
		return codexCredentials{}, nil, errors.New("Codex auth.json could not be read.")
	}
	return parseCodexAuthData(data)
}

func parseCodexAuthData(data []byte) (codexCredentials, *ErrorDetail, error) {
	var raw codexAuthFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return codexCredentials{}, fieldDetail("auth.json", "invalid_json"), errors.New("Codex auth.json could not be decoded.")
	}
	if strings.TrimSpace(raw.OpenAIAPIKey) != "" && raw.Tokens.AccessToken == "" && raw.Tokens.AccessTokenC == "" {
		return codexCredentials{}, fieldDetail("auth.json.OPENAI_API_KEY", "oauth_required"), errors.New("Codex auth.json contains only an API key; OAuth tokens are required.")
	}
	creds := codexCredentials{
		AccessToken:  firstNonEmpty(raw.Tokens.AccessToken, raw.Tokens.AccessTokenC),
		RefreshToken: firstNonEmpty(raw.Tokens.RefreshToken, raw.Tokens.RefreshTokenC),
		IDToken:      firstNonEmpty(raw.Tokens.IDToken, raw.Tokens.IDTokenC),
		AccountID:    firstNonEmpty(raw.Tokens.AccountID, raw.Tokens.AccountIDC),
	}
	creds.AccountID = firstNonEmpty(
		creds.AccountID,
		codexAccountIDFromJWT(creds.IDToken),
		codexAccountIDFromJWT(creds.AccessToken),
	)
	// Polling never refreshes the token, so only the access token is needed
	// (the codex CLI keeps the refresh token for itself).
	if creds.AccessToken == "" {
		return codexCredentials{}, fieldDetail("auth.json.tokens", "missing_oauth_tokens"), errors.New(codexLoginRequired)
	}
	if t := parseCodexTime(raw.LastRefresh); t != nil {
		creds.LastRefresh = t
	}
	return creds, nil, nil
}

func reloadCodexAuthIfChanged(path string, previous []byte) (codexCredentials, *ErrorDetail, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codexCredentials{}, fieldDetail("auth.json", "missing"), false, errors.New("Codex auth.json not found. Run `codex` to log in.")
		}
		return codexCredentials{}, nil, false, errors.New("Codex auth.json could not be read.")
	}
	if bytes.Equal(data, previous) {
		return codexCredentials{}, nil, false, nil
	}
	creds, detail, err := parseCodexAuthData(data)
	return creds, detail, true, err
}

type codexUsageRaw struct {
	PlanType             string                       `json:"plan_type"`
	RateLimit            codexRateLimitRaw            `json:"rate_limit"`
	AdditionalRateLimits codexAdditionalRateLimitList `json:"additional_rate_limits"`
	Credits              codexCreditDetails           `json:"credits"`
}

type codexRateLimitRaw struct {
	PrimaryWindow   *codexWindowRaw `json:"primary_window"`
	SecondaryWindow *codexWindowRaw `json:"secondary_window"`
}

type codexWindowRaw struct {
	UsedPercent        float64 `json:"used_percent"`
	ResetAt            int64   `json:"reset_at"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
}

type codexAdditionalRateLimitRaw struct {
	LimitName      string            `json:"limit_name"`
	MeteredFeature string            `json:"metered_feature"`
	RateLimit      codexRateLimitRaw `json:"rate_limit"`
}

type codexAdditionalRateLimitList []codexAdditionalRateLimitRaw

func (l *codexAdditionalRateLimitList) UnmarshalJSON(data []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil
	}
	out := make([]codexAdditionalRateLimitRaw, 0, len(items))
	for _, item := range items {
		var raw codexAdditionalRateLimitRaw
		if err := json.Unmarshal(item, &raw); err != nil {
			continue
		}
		out = append(out, raw)
	}
	*l = out
	return nil
}

type codexCreditDetails struct {
	Balance       *float64 `json:"-"`
	BalanceDetail string   `json:"-"`
}

func (c *codexCreditDetails) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	balanceRaw, ok := raw["balance"]
	if !ok || string(balanceRaw) == "null" {
		return nil
	}
	value, ok := parseCodexBalance(balanceRaw)
	if ok {
		c.Balance = &value
		return nil
	}
	c.BalanceDetail = jsonTypeName(balanceRaw)
	return nil
}

func (p *codexProvider) fetchUsage(ctx context.Context, creds codexCredentials) (codexUsageRaw, *ErrorDetail, Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.usageURL(), nil)
	if err != nil {
		return codexUsageRaw{}, nil, StatusFetchError, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Accept", "application/json")
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", creds.AccountID)
	}
	resp, err := doWithRetry(ctx, p.client(), req, RetryPolicy{MaxRetries: 1})
	if err != nil {
		return codexUsageRaw{}, nil, usageRequestStatus(err), fmt.Errorf("Codex usage request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
	case resp.StatusCode == http.StatusUnauthorized:
		return codexUsageRaw{}, jsonShapeDetail("usage", data), StatusAuthMissing, fmt.Errorf("Codex usage endpoint rejected the OAuth token with HTTP %d.", resp.StatusCode)
	case resp.StatusCode == http.StatusForbidden:
		return codexUsageRaw{}, jsonShapeDetail("usage", data), StatusScopeMissing, fmt.Errorf("Codex usage endpoint rejected the OAuth scope with HTTP %d.", resp.StatusCode)
	default:
		return codexUsageRaw{}, httpStatusDetail("usage", data, resp.StatusCode), StatusFetchError, fmt.Errorf("Codex usage endpoint returned HTTP %d.", resp.StatusCode)
	}
	var raw codexUsageRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return codexUsageRaw{}, jsonShapeDetail("usage", data), StatusFetchError, errors.New("Codex usage response could not be decoded.")
	}
	return raw, nil, "", nil
}

func normalizeCodexUsage(raw codexUsageRaw, creds codexCredentials) (Snapshot, *ErrorDetail, error) {
	detail := &ErrorDetail{Fields: map[string]string{}}
	windows := make([]Window, 0, 2)
	addWindow := func(path string, w *codexWindowRaw) {
		if w == nil {
			return
		}
		key := codexWindowKey(w.LimitWindowSeconds)
		if key == "" {
			detail.Fields[path+".limit_window_seconds"] = fmt.Sprintf("number=%d", w.LimitWindowSeconds)
			return
		}
		if w.ResetAt <= 0 {
			detail.Fields[path+".reset_at"] = "missing"
			return
		}
		reset := time.Unix(w.ResetAt, 0).UTC()
		win := normalizedWindow(key, w.UsedPercent, &reset, w.LimitWindowSeconds)
		win.Pace = computeWindowPace(win, time.Now())
		windows = append(windows, win)
	}
	addWindow("rate_limit.primary_window", raw.RateLimit.PrimaryWindow)
	addWindow("rate_limit.secondary_window", raw.RateLimit.SecondaryWindow)
	windows = append(windows, codexAdditionalWindows(raw.AdditionalRateLimits)...)
	ordered := make([]Window, 0, len(windows))
	for _, want := range []string{"session", "weekly", "monthly", "codex_spark", "codex_spark_weekly"} {
		for _, w := range windows {
			if w.Key == want {
				ordered = append(ordered, w)
			}
		}
	}
	snap := Snapshot{
		Status:  StatusOK,
		Plan:    codexPlanDisplayName(firstNonEmpty(raw.PlanType, codexPlanFromIDToken(creds.IDToken))),
		Windows: ordered,
	}
	if raw.Credits.Balance != nil {
		snap.Credits = &Credits{Label: "Credits", Balance: *raw.Credits.Balance}
	}
	if raw.Credits.BalanceDetail != "" {
		detail.Fields["credits.balance"] = raw.Credits.BalanceDetail
	}
	if len(ordered) == 0 && snap.Credits == nil {
		return Snapshot{}, detailOrNil(detail), errors.New("Codex usage response did not include recognized rate-limit windows.")
	}
	return snap, detailOrNil(detail), nil
}

func codexPlanDisplayName(raw string) string {
	words := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(raw)), func(r rune) bool {
		return r == '_' || r == '-' || unicode.IsSpace(r)
	})
	if len(words) == 0 {
		return ""
	}
	normalized := strings.Join(words, " ")
	switch normalized {
	case "pro":
		return "Pro 20x"
	case "prolite", "pro lite":
		return "Pro 5x"
	}
	for i, word := range words {
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

func codexAdditionalWindows(entries []codexAdditionalRateLimitRaw) []Window {
	if len(entries) == 0 {
		return nil
	}
	out := make([]Window, 0, 2)
	seen := map[string]bool{}
	add := func(w *codexWindowRaw) {
		if w == nil {
			return
		}
		key := codexAdditionalWindowKey(w.LimitWindowSeconds)
		if key == "" || seen[key] || w.ResetAt <= 0 {
			return
		}
		seen[key] = true
		reset := time.Unix(w.ResetAt, 0).UTC()
		out = append(out, normalizedWindow(key, w.UsedPercent, &reset, 0))
	}
	for _, entry := range entries {
		if !codexAdditionalLimitIsSpark(entry) {
			continue
		}
		add(entry.RateLimit.PrimaryWindow)
		add(entry.RateLimit.SecondaryWindow)
	}
	return out
}

func codexAdditionalLimitIsSpark(entry codexAdditionalRateLimitRaw) bool {
	return strings.Contains(strings.ToLower(entry.LimitName), "spark") ||
		strings.Contains(strings.ToLower(entry.MeteredFeature), "spark")
}

func codexAdditionalWindowKey(seconds int64) string {
	switch {
	case seconds > 0 && seconds <= int64((24*time.Hour)/time.Second):
		return "codex_spark"
	case seconds >= int64((6*24*time.Hour)/time.Second) && seconds <= int64((31*24*time.Hour)/time.Second):
		return "codex_spark_weekly"
	default:
		return ""
	}
}

func codexWindowKey(seconds int64) string {
	switch {
	case seconds > 0 && seconds <= int64((24*time.Hour)/time.Second):
		return "session"
	case seconds > int64((24*time.Hour)/time.Second) && seconds <= int64((14*24*time.Hour)/time.Second):
		return "weekly"
	case seconds > int64((14*24*time.Hour)/time.Second) && seconds <= int64((31*24*time.Hour)/time.Second):
		return "monthly"
	default:
		return ""
	}
}

func parseCodexBalance(data []byte) (float64, bool) {
	var f float64
	if err := json.Unmarshal(data, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return v, err == nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err == nil {
		for _, key := range []string{"balance", "remaining", "available", "amount", "value"} {
			if raw, ok := obj[key]; ok {
				if v, ok := parseCodexBalance(raw); ok {
					return v, true
				}
			}
		}
	}
	return 0, false
}

func codexPlanFromIDToken(token string) string {
	payload := codexJWTClaims(token)
	if payload == nil {
		return ""
	}
	if v, ok := payload["https://api.openai.com/auth.chatgpt_plan_type"].(string); ok {
		return strings.TrimSpace(v)
	}
	if auth, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
		if v, ok := auth["chatgpt_plan_type"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	if v, ok := payload["chatgpt_plan_type"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func codexAccountIDFromJWT(token string) string {
	payload := codexJWTClaims(token)
	if payload == nil {
		return ""
	}
	for _, key := range []string{
		"https://api.openai.com/auth.chatgpt_account_id",
		"chatgpt_account_id",
		"account_id",
	} {
		if v, ok := payload[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if auth, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
		for _, key := range []string{"chatgpt_account_id", "account_id"} {
			if v, ok := auth[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func codexJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	return payload
}

func parseCodexTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}

func codexStatus(status Status, message string, detail *ErrorDetail) Snapshot {
	return Snapshot{Status: status, Error: message, ErrorDetail: detail}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func jsonTypeName(data []byte) string {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return "invalid_json"
	}
	switch value.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}
