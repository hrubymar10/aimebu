package usages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

const (
	claudeCredentialsRelPath  = ".claude/.credentials.json"
	claudeUsageURL            = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBetaHeader     = "oauth-2025-04-20"
	claudeCodeUserAgentPrefix = "claude-code/"
	claudeCodeVersionFallback = "2.1.0"
)

var (
	claudeCodeANSIPattern   = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	claudeCodeVersionOnce   sync.Once
	claudeCodeVersionCached string
)

type claudeCodeProvider struct{}

func NewClaudeCodeProvider() Provider { return claudeCodeProvider{} }

func (claudeCodeProvider) Key() string { return ProviderClaudeCode }

func (claudeCodeProvider) Fetch(ctx context.Context, store *Store) (Snapshot, error) {
	authPath, err := claudeAuthPath()
	if err != nil {
		return claudeStatus(StatusAuthMissing, "Claude credentials path unavailable.", nil), nil
	}
	creds, detail, err := loadClaudeAuth(authPath)
	if err != nil {
		return claudeStatus(StatusAuthMissing, err.Error(), detail), nil
	}
	raw, detail, status, unauthorized, err := fetchClaudeUsage(ctx, creds)
	if unauthorized {
		return claudeStatus(status, "Claude usage endpoint rejected the OAuth token. Run `claude` to refresh the login.", detail), nil
	}
	if err != nil {
		snap := claudeStatus(status, err.Error(), detail)
		if status == StatusFetchError {
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		return snap, nil
	}
	snap, detail, err := normalizeClaudeUsage(raw, creds)
	if err != nil {
		snap = claudeStatus(StatusFetchError, err.Error(), detail)
		return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
	}
	if detail != nil && len(detail.Fields) > 0 {
		snap.ErrorDetail = detail
	}
	return snap, nil
}

type claudeAuthFile struct {
	ClaudeAiOAuth  claudeAuthTokens `json:"claudeAiOauth"`
	ClaudeAiOAuthS claudeAuthTokens `json:"claude_ai_oauth"`
}

type claudeAuthTokens struct {
	AccessToken       string   `json:"accessToken,omitempty"`
	AccessTokenS      string   `json:"access_token,omitempty"`
	RefreshToken      string   `json:"refreshToken,omitempty"`
	RefreshTokenS     string   `json:"refresh_token,omitempty"`
	ExpiresAt         float64  `json:"expiresAt,omitempty"`
	ExpiresAtS        float64  `json:"expires_at,omitempty"`
	Scopes            []string `json:"scopes,omitempty"`
	RateLimitTier     string   `json:"rateLimitTier,omitempty"`
	RateLimitTierS    string   `json:"rate_limit_tier,omitempty"`
	SubscriptionType  string   `json:"subscriptionType,omitempty"`
	SubscriptionTypeS string   `json:"subscription_type,omitempty"`
}

type claudeCredentials struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        *time.Time
	Scopes           []string
	RateLimitTier    string
	SubscriptionType string
}

func claudeAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, claudeCredentialsRelPath), nil
}

func loadClaudeAuth(path string) (claudeCredentials, *ErrorDetail, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return claudeCredentials{}, fieldDetail("credentials.json", "missing"), errors.New("Claude credentials not found. Run `claude` to authenticate.")
		}
		return claudeCredentials{}, nil, errors.New("Claude credentials could not be read.")
	}
	var raw claudeAuthFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return claudeCredentials{}, fieldDetail("credentials.json", "invalid_json"), errors.New("Claude credentials could not be decoded.")
	}
	tokens := raw.ClaudeAiOAuth
	if firstNonEmpty(tokens.AccessToken, tokens.AccessTokenS) == "" && firstNonEmpty(raw.ClaudeAiOAuthS.AccessToken, raw.ClaudeAiOAuthS.AccessTokenS) != "" {
		tokens = raw.ClaudeAiOAuthS
	}
	creds := claudeCredentials{
		AccessToken:      firstNonEmpty(tokens.AccessToken, tokens.AccessTokenS),
		RefreshToken:     firstNonEmpty(tokens.RefreshToken, tokens.RefreshTokenS),
		Scopes:           tokens.Scopes,
		RateLimitTier:    firstNonEmpty(tokens.RateLimitTier, tokens.RateLimitTierS),
		SubscriptionType: firstNonEmpty(tokens.SubscriptionType, tokens.SubscriptionTypeS),
	}
	if tokens.ExpiresAt != 0 {
		t := time.UnixMilli(int64(tokens.ExpiresAt)).UTC()
		creds.ExpiresAt = &t
	} else if tokens.ExpiresAtS != 0 {
		t := time.UnixMilli(int64(tokens.ExpiresAtS)).UTC()
		creds.ExpiresAt = &t
	}
	if creds.AccessToken == "" {
		return claudeCredentials{}, fieldDetail("credentials.json.claudeAiOauth.accessToken", "missing"), errors.New("Claude OAuth access token missing. Run `claude` to authenticate.")
	}
	return creds, nil, nil
}

func claudeCodeUserAgent() string {
	return claudeCodeUserAgentPrefix + claudeCodeUAVersion()
}

func claudeCodeUAVersion() string {
	claudeCodeVersionOnce.Do(func() {
		claudeCodeVersionCached = detectClaudeCodeVersion()
		if claudeCodeVersionCached == "" {
			claudeCodeVersionCached = claudeCodeVersionFallback
		}
	})
	return claudeCodeVersionCached
}

func detectClaudeCodeVersion() string {
	path, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--allowed-tools", "", "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	clean := claudeCodeANSIPattern.ReplaceAllString(string(out), "")
	for _, line := range strings.Split(clean, "\n") {
		line = strings.TrimSpace(line)
		if fields := strings.Fields(line); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

type claudeUsageRaw struct {
	FiveHour          *claudeWindowRaw     `json:"five_hour"`
	SevenDay          *claudeWindowRaw     `json:"seven_day"`
	SevenDayOAuthApps *claudeWindowRaw     `json:"seven_day_oauth_apps"`
	SevenDayOpus      *claudeWindowRaw     `json:"seven_day_opus"`
	SevenDaySonnet    *claudeWindowRaw     `json:"seven_day_sonnet"`
	ExtraUsage        *claudeExtraUsageRaw `json:"extra_usage"`
}

type claudeWindowRaw struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type claudeExtraUsageRaw struct {
	IsEnabled    *bool    `json:"is_enabled"`
	UsedCredits  *float64 `json:"used_credits"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	Utilization  *float64 `json:"utilization"`
	Currency     string   `json:"currency"`
}

func fetchClaudeUsage(ctx context.Context, creds claudeCredentials) (claudeUsageRaw, *ErrorDetail, Status, bool, error) {
	var lastRaw claudeUsageRaw
	var lastDetail *ErrorDetail
	for attempt := 0; attempt < 2; attempt++ {
		raw, detail, status, unauthorized, err := fetchClaudeUsageOnce(ctx, creds)
		if err != nil || unauthorized || raw.hasValues() {
			return raw, detail, status, unauthorized, err
		}
		lastRaw, lastDetail = raw, detail
	}
	return lastRaw, lastDetail, "", false, nil
}

func fetchClaudeUsageOnce(ctx context.Context, creds claudeCredentials) (claudeUsageRaw, *ErrorDetail, Status, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return claudeUsageRaw{}, nil, StatusFetchError, false, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	req.Header.Set("User-Agent", claudeCodeUserAgent())
	resp, err := doWithRetry(ctx, usageHTTPClient, req, RetryPolicy{MaxRetries: 1})
	if err != nil {
		return claudeUsageRaw{}, nil, usageRequestStatus(err), false, fmt.Errorf("Claude usage request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
	case resp.StatusCode == http.StatusUnauthorized:
		return claudeUsageRaw{}, jsonShapeDetail("usage", data), StatusAuthMissing, true, fmt.Errorf("Claude usage endpoint rejected the OAuth token with HTTP %d.", resp.StatusCode)
	case resp.StatusCode == http.StatusForbidden:
		return claudeUsageRaw{}, jsonShapeDetail("usage", data), StatusScopeMissing, false, fmt.Errorf("Claude usage endpoint rejected the OAuth scope with HTTP %d.", resp.StatusCode)
	default:
		return claudeUsageRaw{}, jsonShapeDetail("usage", data), StatusFetchError, false, fmt.Errorf("Claude usage endpoint returned HTTP %d.", resp.StatusCode)
	}
	var raw claudeUsageRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return claudeUsageRaw{}, jsonShapeDetail("usage", data), StatusFetchError, false, errors.New("Claude usage response could not be decoded.")
	}
	return raw, nil, "", false, nil
}

func (raw claudeUsageRaw) hasValues() bool {
	for _, w := range []*claudeWindowRaw{raw.FiveHour, raw.SevenDay, raw.SevenDayOAuthApps, raw.SevenDayOpus, raw.SevenDaySonnet} {
		if w != nil && w.Utilization != nil {
			return true
		}
	}
	return raw.ExtraUsage != nil &&
		raw.ExtraUsage.IsEnabled != nil &&
		*raw.ExtraUsage.IsEnabled &&
		(raw.ExtraUsage.UsedCredits != nil || raw.ExtraUsage.MonthlyLimit != nil)
}

func normalizeClaudeUsage(raw claudeUsageRaw, creds claudeCredentials) (Snapshot, *ErrorDetail, error) {
	detail := &ErrorDetail{Fields: map[string]string{}}
	windows := make([]Window, 0, 4)
	addWindow := func(key, path string, w *claudeWindowRaw) {
		if w == nil {
			return
		}
		if w.Utilization == nil {
			detail.Fields[path+".utilization"] = "missing"
			return
		}
		var reset *time.Time
		if strings.TrimSpace(w.ResetsAt) != "" {
			t, ok := parseClaudeTime(w.ResetsAt)
			if !ok {
				detail.Fields[path+".resets_at"] = "string"
			} else {
				reset = &t
			}
		}
		windows = append(windows, Window{Key: key, PercentUsed: *w.Utilization, ResetAt: reset})
	}
	addWindow("session", "five_hour", raw.FiveHour)
	weeklyWindow, weeklyPath := raw.SevenDay, "seven_day"
	if weeklyWindow == nil {
		weeklyWindow, weeklyPath = raw.SevenDayOAuthApps, "seven_day_oauth_apps"
	}
	if weeklyWindow == nil && (raw.SevenDayOpus != nil || raw.SevenDaySonnet != nil) {
		detail.Fields["seven_day"] = "missing"
		detail.Fields["seven_day_oauth_apps"] = "missing"
	}
	addWindow("weekly", weeklyPath, weeklyWindow)
	addWindow("weekly_opus", "seven_day_opus", raw.SevenDayOpus)
	addWindow("weekly_sonnet", "seven_day_sonnet", raw.SevenDaySonnet)
	ordered := orderWindows(windows, []string{"session", "weekly", "weekly_opus", "weekly_sonnet"})
	snap := Snapshot{
		Provider: ProviderClaudeCode,
		Status:   StatusOK,
		Plan:     claudePlan(creds),
		Windows:  ordered,
	}
	if raw.ExtraUsage != nil && raw.ExtraUsage.IsEnabled != nil && *raw.ExtraUsage.IsEnabled {
		credits := &Credits{Label: "Extra usage monthly cap"}
		if raw.ExtraUsage.UsedCredits != nil {
			credits.Balance = claudeExtraUsageAmount(*raw.ExtraUsage.UsedCredits)
		}
		if raw.ExtraUsage.MonthlyLimit != nil {
			credits.SpendLimit = claudeExtraUsageAmount(*raw.ExtraUsage.MonthlyLimit)
		}
		if raw.ExtraUsage.UsedCredits != nil || raw.ExtraUsage.MonthlyLimit != nil {
			snap.Credits = credits
		}
	}
	if len(ordered) == 0 && snap.Credits == nil {
		return Snapshot{}, detailOrNil(detail), errors.New("Claude usage response did not include recognized rate-limit windows.")
	}
	return snap, detailOrNil(detail), nil
}

func claudeExtraUsageAmount(value float64) float64 {
	return value / 100
}

func orderWindows(windows []Window, keys []string) []Window {
	ordered := make([]Window, 0, len(windows))
	for _, want := range keys {
		for _, w := range windows {
			if w.Key == want {
				ordered = append(ordered, w)
			}
		}
	}
	return ordered
}

func parseClaudeTime(value string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func claudePlan(creds claudeCredentials) string {
	return firstNonEmpty(creds.SubscriptionType, creds.RateLimitTier)
}

func claudeStatus(status Status, message string, detail *ErrorDetail) Snapshot {
	return Snapshot{Provider: ProviderClaudeCode, Status: status, Error: message, ErrorDetail: detail}
}
