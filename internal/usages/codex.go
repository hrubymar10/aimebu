package usages

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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

const (
	codexAuthRefreshAfter = 8 * 24 * time.Hour
	codexRefreshURL       = "https://auth.openai.com/oauth/token"
	codexUsageURL         = "https://chatgpt.com/backend-api/wham/usage"
	codexClientID         = "app_EMoamEEZ73f0CkXaXp7hrann"

	jsonShapeDetailMaxFields = 50
)

type codexProvider struct{}

func NewCodexProvider() Provider { return codexProvider{} }

func (codexProvider) Key() string { return ProviderCodex }

func (codexProvider) Fetch(ctx context.Context, store *Store) (Snapshot, error) {
	authPath, err := codexAuthPath()
	if err != nil {
		return codexStatus(StatusAuthMissing, "Codex auth path unavailable.", nil), nil
	}
	auth, detail, err := loadCodexAuthSnapshot(authPath)
	if err != nil {
		return codexStatus(StatusAuthMissing, err.Error(), detail), nil
	}
	creds := auth.Credentials
	if creds.needsRefresh(time.Now()) {
		refreshed, detail, err := refreshCodexAuth(ctx, creds)
		if err != nil {
			return codexStatus(StatusAuthMissing, err.Error(), detail), nil
		}
		creds = refreshed
		if err := saveCodexAuth(authPath, creds); err != nil {
			return codexStatus(StatusAuthMissing, "Codex auth refresh could not be saved.", nil), nil
		}
		auth.Fingerprint = codexAuthFingerprint(authPath)
	}

	raw, detail, status, err := fetchCodexUsage(ctx, creds)
	if err != nil && status == StatusAuthMissing && creds.RefreshToken != "" {
		reloaded, reloadDetail, changed, reloadErr := reloadCodexAuthIfChanged(authPath, auth.Fingerprint)
		if changed {
			if reloadErr != nil {
				return codexStatus(StatusAuthMissing, reloadErr.Error(), reloadDetail), nil
			}
			creds = reloaded
			raw, detail, status, err = fetchCodexUsage(ctx, creds)
		}
	}
	if err != nil && status == StatusAuthMissing && creds.RefreshToken != "" {
		refreshed, refreshDetail, refreshErr := refreshCodexAuth(ctx, creds)
		if refreshErr != nil {
			return codexStatus(StatusAuthMissing, refreshErr.Error(), refreshDetail), nil
		}
		creds = refreshed
		if err := saveCodexAuth(authPath, creds); err != nil {
			return codexStatus(StatusAuthMissing, "Codex auth refresh could not be saved.", nil), nil
		}
		raw, detail, status, err = fetchCodexUsage(ctx, creds)
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

func (c codexCredentials) needsRefresh(now time.Time) bool {
	if c.LastRefresh == nil {
		return true
	}
	return now.Sub(*c.LastRefresh) > codexAuthRefreshAfter
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
			return codexAuthSnapshot{}, fieldDetail("auth.json", "missing"), errors.New("Codex auth.json not found. Run `codex` to log in.")
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
			return codexCredentials{}, fieldDetail("auth.json", "missing"), errors.New("Codex auth.json not found. Run `codex` to log in.")
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
	if creds.AccessToken == "" || creds.RefreshToken == "" {
		return codexCredentials{}, fieldDetail("auth.json.tokens", "missing_oauth_tokens"), errors.New("Codex auth.json exists but contains no OAuth tokens.")
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

func codexAuthFingerprint(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return append([]byte(nil), data...)
}

func refreshCodexAuth(ctx context.Context, creds codexCredentials) (codexCredentials, *ErrorDetail, error) {
	body := map[string]string{
		"client_id":     codexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": creds.RefreshToken,
		"scope":         "openid profile email",
	}
	data, err := json.Marshal(body)
	if err != nil {
		return creds, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexRefreshURL, bytes.NewReader(data))
	if err != nil {
		return creds, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := usageHTTPClient.Do(req)
	if err != nil {
		return creds, nil, errors.New("Codex OAuth refresh request failed.")
	}
	defer resp.Body.Close()
	respData, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return creds, jsonShapeDetail("refresh", respData), fmt.Errorf("Codex OAuth refresh failed with HTTP %d.", resp.StatusCode)
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(respData, &raw); err != nil {
		return creds, jsonShapeDetail("refresh", respData), errors.New("Codex OAuth refresh response could not be decoded.")
	}
	if raw.AccessToken != "" {
		creds.AccessToken = raw.AccessToken
	}
	if raw.RefreshToken != "" {
		creds.RefreshToken = raw.RefreshToken
	}
	if raw.IDToken != "" {
		creds.IDToken = raw.IDToken
	}
	now := time.Now().UTC()
	creds.LastRefresh = &now
	return creds, nil, nil
}

func saveCodexAuth(path string, creds codexCredentials) error {
	raw := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &raw)
	}
	tokens := map[string]json.RawMessage{}
	if tokenData, ok := raw["tokens"]; ok {
		_ = json.Unmarshal(tokenData, &tokens)
	}
	patchTokenValue(tokens, "access_token", "accessToken", creds.AccessToken)
	patchTokenValue(tokens, "refresh_token", "refreshToken", creds.RefreshToken)
	if creds.IDToken != "" {
		patchTokenValue(tokens, "id_token", "idToken", creds.IDToken)
	}
	if creds.AccountID != "" {
		patchTokenValue(tokens, "account_id", "accountId", creds.AccountID)
	}
	tokenData, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	raw["tokens"] = tokenData
	if creds.LastRefresh != nil {
		lastRefresh, err := json.Marshal(creds.LastRefresh.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		raw["last_refresh"] = lastRefresh
	}
	return writeAtomicJSONFile(path, raw, 0o600)
}

func patchTokenValue(tokens map[string]json.RawMessage, snakeKey, camelKey, value string) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, hasSnake := tokens[snakeKey]
	_, hasCamel := tokens[camelKey]
	if hasSnake || !hasCamel {
		tokens[snakeKey] = data
	}
	if hasCamel {
		tokens[camelKey] = data
	}
}

type codexUsageRaw struct {
	PlanType  string             `json:"plan_type"`
	RateLimit codexRateLimitRaw  `json:"rate_limit"`
	Credits   codexCreditDetails `json:"credits"`
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

func fetchCodexUsage(ctx context.Context, creds codexCredentials) (codexUsageRaw, *ErrorDetail, Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return codexUsageRaw{}, nil, StatusFetchError, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Accept", "application/json")
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", creds.AccountID)
	}
	resp, err := usageHTTPClient.Do(req)
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
		return codexUsageRaw{}, jsonShapeDetail("usage", data), StatusFetchError, fmt.Errorf("Codex usage endpoint returned HTTP %d.", resp.StatusCode)
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
		reset := time.Unix(w.ResetAt, 0).UTC()
		windows = append(windows, Window{Key: key, PercentUsed: w.UsedPercent, ResetAt: &reset})
	}
	addWindow("rate_limit.primary_window", raw.RateLimit.PrimaryWindow)
	addWindow("rate_limit.secondary_window", raw.RateLimit.SecondaryWindow)
	if len(windows) == 0 {
		return Snapshot{}, detailOrNil(detail), errors.New("Codex usage response did not include recognized rate-limit windows.")
	}
	ordered := make([]Window, 0, len(windows))
	for _, want := range []string{"session", "weekly"} {
		for _, w := range windows {
			if w.Key == want {
				ordered = append(ordered, w)
			}
		}
	}
	snap := Snapshot{
		Provider: ProviderCodex,
		Status:   StatusOK,
		Plan:     firstNonEmpty(raw.PlanType, codexPlanFromIDToken(creds.IDToken)),
		Windows:  ordered,
	}
	if raw.Credits.Balance != nil {
		snap.Credits = &Credits{Label: "Credits", Balance: *raw.Credits.Balance}
	}
	if raw.Credits.BalanceDetail != "" {
		detail.Fields["credits.balance"] = raw.Credits.BalanceDetail
	}
	return snap, detailOrNil(detail), nil
}

func codexWindowKey(seconds int64) string {
	switch {
	case seconds > 0 && seconds <= int64((24*time.Hour)/time.Second):
		return "session"
	case seconds > int64((24*time.Hour)/time.Second) && seconds <= int64((31*24*time.Hour)/time.Second):
		return "weekly"
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
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
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
	return Snapshot{Provider: ProviderCodex, Status: status, Error: message, ErrorDetail: detail}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func fieldDetail(path, typ string) *ErrorDetail {
	return &ErrorDetail{Fields: map[string]string{path: typ}}
}

func detailOrNil(detail *ErrorDetail) *ErrorDetail {
	if detail == nil || len(detail.Fields) == 0 {
		return nil
	}
	return detail
}

func jsonShapeDetail(prefix string, data []byte) *ErrorDetail {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fieldDetail(prefix, "invalid_json")
	}
	fields := map[string]string{}
	if collectJSONTypes(prefix, value, fields, jsonShapeDetailMaxFields) {
		fields["_truncated"] = "true"
	}
	return &ErrorDetail{Fields: fields}
}

func collectJSONTypes(path string, value any, fields map[string]string, maxFields int) bool {
	if len(fields) >= maxFields {
		return true
	}
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			fields[path] = "object"
			return false
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := v[key]
			if collectJSONTypes(path+"."+key, child, fields, maxFields) {
				return true
			}
		}
		return false
	case []any:
		fields[path] = "array"
	case string:
		fields[path] = "string"
	case float64:
		fields[path] = "number"
	case bool:
		fields[path] = "boolean"
	case nil:
		fields[path] = "null"
	default:
		fields[path] = fmt.Sprintf("%T", value)
	}
	return false
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

func writeAtomicJSONFile(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	_ = fsyncDir(filepath.Dir(path))
	return os.Chmod(path, mode)
}
