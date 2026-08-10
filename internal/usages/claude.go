package usages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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

func NewClaudeCodeProvider() UsageProvider { return claudeCodeProvider{} }

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

// ClaudeConfigDir resolves claude-code's config directory: CLAUDE_CONFIG_DIR
// when set, otherwise <home>/.claude. Pass home explicitly so callers that
// override HOME (tests, the switcher) stay consistent.
func ClaudeConfigDir(home string) string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(home, ".claude")
}

// ClaudeLiveCredPath is the live credentials file: always
// <config dir>/.credentials.json, which collapses both layouts into one rule.
func ClaudeLiveCredPath(home string) string {
	return filepath.Join(ClaudeConfigDir(home), ".credentials.json")
}

// ClaudeAccountFilePath is the account-state file holding oauthAccount. It does
// NOT follow the credentials: with CLAUDE_CONFIG_DIR set it sits beside them in
// that directory, but with the variable unset it lives at <home>/.claude.json —
// the home root, not inside <home>/.claude. That asymmetry is claude-code's,
// not ours; encode it once here rather than probing candidate paths.
func ClaudeAccountFilePath(home string) string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

func claudeAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return ClaudeLiveCredPath(home), nil
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

// ValidateClaudeCredentials validates path as a claude .credentials.json file.
// Returns the credential expiry (for Profile.ExpiresAt) and nil on success.
// An error is returned when the file is absent, unparseable, missing an access
// token, or has a zero expiresAt — the last case is a real observed corruption
// (expiresAt == 0 means malformed, not merely expired; a past but non-zero
// value is structurally valid and captures fine).
func ValidateClaudeCredentials(path string) (*time.Time, error) {
	creds, _, err := loadClaudeAuth(path)
	if err != nil {
		return nil, err
	}
	// ExpiresAt is nil only when the raw expiresAt field was zero — malformed.
	// A past non-zero value produces a non-nil time, which is valid to capture.
	if creds.ExpiresAt == nil {
		return nil, errors.New("claude credentials: expiresAt is zero (malformed)")
	}
	return creds.ExpiresAt, nil
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
	cmd := exec.CommandContext(ctx, path, "--allowed-tools", "", "--version")
	cmd.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1")
	out, err := cmd.CombinedOutput()
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
	Limits            []claudeLimitRaw     `json:"limits"`
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

type claudeLimitRaw struct {
	Kind     string              `json:"kind"`
	Group    string              `json:"group"`
	Percent  *float64            `json:"percent"`
	ResetsAt string              `json:"resets_at"`
	Scope    claudeLimitScopeRaw `json:"scope"`
	IsActive *bool               `json:"is_active"`
}

type claudeLimitScopeRaw struct {
	Model *claudeLimitModelRaw `json:"model"`
}

type claudeLimitModelRaw struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
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
	for _, limit := range raw.Limits {
		if limit.Kind == "weekly_scoped" && limit.Group == "weekly" && limit.Percent != nil {
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
		var durationSec int64
		switch key {
		case "session":
			durationSec = 5 * 3600
		case "weekly":
			durationSec = 7 * 24 * 3600
		}
		win := normalizedWindow(key, *w.Utilization, reset, durationSec)
		if durationSec > 0 {
			win.Pace = computeWindowPace(win, time.Now())
		}
		windows = append(windows, win)
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
	windows = append(windows, claudeScopedWeeklyWindows(raw.Limits, detail)...)
	ordered := orderWindows(windows, append([]string{"session", "weekly", "weekly_opus", "weekly_sonnet"}, claudeScopedWeeklyOrder(windows)...))
	snap := Snapshot{
		Status:  StatusOK,
		Plan:    claudePlan(creds),
		Windows: ordered,
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

func claudeScopedWeeklyWindows(limits []claudeLimitRaw, detail *ErrorDetail) []Window {
	if len(limits) == 0 {
		return nil
	}
	out := make([]Window, 0)
	seen := map[string]bool{}
	for i, limit := range limits {
		if limit.Kind != "weekly_scoped" || limit.Group != "weekly" {
			continue
		}
		path := fmt.Sprintf("limits.%d", i)
		if limit.Percent == nil {
			detail.Fields[path+".percent"] = "missing"
			continue
		}
		if math.IsNaN(*limit.Percent) || math.IsInf(*limit.Percent, 0) {
			detail.Fields[path+".percent"] = "number_out_of_range"
			continue
		}
		if limit.Scope.Model == nil {
			detail.Fields[path+".scope.model"] = "missing"
			continue
		}
		modelName := strings.TrimSpace(limit.Scope.Model.DisplayName)
		if modelName == "" {
			detail.Fields[path+".scope.model.display_name"] = "missing"
			continue
		}
		slug := claudeScopedWeeklySlug(firstNonEmpty(limit.Scope.Model.ID, modelName))
		if slug == "" {
			detail.Fields[path+".scope.model"] = "invalid"
			continue
		}
		key := "weekly_scoped:" + slug
		if seen[key] {
			continue
		}
		seen[key] = true
		var reset *time.Time
		if strings.TrimSpace(limit.ResetsAt) != "" {
			t, ok := parseClaudeTime(limit.ResetsAt)
			if !ok {
				detail.Fields[path+".resets_at"] = "string"
			} else {
				reset = &t
			}
		}
		win := normalizedWindow(key, *limit.Percent, reset, 7*24*3600)
		win.Pace = computeWindowPace(win, time.Now())
		out = append(out, win)
	}
	return out
}

func claudeScopedWeeklySlug(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func claudeScopedWeeklyOrder(windows []Window) []string {
	keys := make([]string, 0)
	for _, w := range windows {
		if strings.HasPrefix(w.Key, "weekly_scoped:") {
			keys = append(keys, w.Key)
		}
	}
	return keys
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
	return Snapshot{Status: status, Error: message, ErrorDetail: detail}
}
