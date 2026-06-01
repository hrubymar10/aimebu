package usages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeClaudeAuthFixture(t *testing.T, home string, body string) string {
	t.Helper()
	path := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func resetClaudeCodeVersionCache(t *testing.T) {
	t.Helper()
	claudeCodeVersionOnce = sync.Once{}
	claudeCodeVersionCached = ""
}

func TestClaudeCodeUAVersionFallback(t *testing.T) {
	resetClaudeCodeVersionCache(t)
	t.Setenv("PATH", t.TempDir())

	if got := claudeCodeUAVersion(); got != claudeCodeVersionFallback {
		t.Fatalf("claudeCodeUAVersion = %q, want %q", got, claudeCodeVersionFallback)
	}
	if got := claudeCodeUserAgent(); got != claudeCodeUserAgentPrefix+claudeCodeVersionFallback {
		t.Fatalf("claudeCodeUserAgent = %q", got)
	}
}

func TestClaudeCodeUAVersionFromCLI(t *testing.T) {
	resetClaudeCodeVersionCache(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(`#!/bin/sh
if [ "$1" != "--allowed-tools" ] || [ "$2" != "" ] || [ "$3" != "--version" ]; then
  echo "unexpected args: $*" >&2
  exit 7
fi
printf '\033[32m2.3.4 (Claude Code)\033[0m\nignored\n'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if got := claudeCodeUAVersion(); got != "2.3.4" {
		t.Fatalf("claudeCodeUAVersion = %q, want 2.3.4", got)
	}
	if got := claudeCodeUserAgent(); got != "claude-code/2.3.4" {
		t.Fatalf("claudeCodeUserAgent = %q", got)
	}
}

func TestClaudeProviderFetchesAndNormalizesUsage(t *testing.T) {
	resetClaudeCodeVersionCache(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	writeClaudeAuthFixture(t, home, `{
  "claudeAiOauth": {
    "accessToken": "access-secret",
    "refreshToken": "refresh-secret",
    "expiresAt": 1893456000000,
    "scopes": ["user:profile"],
    "rateLimitTier": "pro",
    "subscriptionType": "team"
  }
}`)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != claudeUsageURL {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer access-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("anthropic-beta"); got != claudeOAuthBetaHeader {
			t.Fatalf("anthropic-beta = %q", got)
		}
		if got := req.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q", got)
		}
		if got := req.Header.Get("User-Agent"); got != claudeCodeUserAgentPrefix+claudeCodeVersionFallback {
			t.Fatalf("User-Agent = %q", got)
		}
		return httpJSON(200, `{
  "five_hour": {"utilization": 47.5, "resets_at": "2030-01-01T00:00:00Z"},
  "seven_day": {"utilization": 50, "resets_at": "2030-01-07T00:00:00Z"},
  "seven_day_opus": {"utilization": 61, "resets_at": "2030-01-07T00:00:00Z"},
  "seven_day_sonnet": {"utilization": 72, "resets_at": "2030-01-07T00:00:00Z"},
  "extra_usage": {"is_enabled": true, "used_credits": 1234, "monthly_limit": 5000, "utilization": 24.68, "currency": "USD"}
}`), nil
	})
	snap, err := NewClaudeCodeProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if snap.Status != StatusOK || snap.Plan != "team" {
		t.Fatalf("snapshot = %+v", snap)
	}
	gotKeys := make([]string, 0, len(snap.Windows))
	for _, w := range snap.Windows {
		gotKeys = append(gotKeys, w.Key)
	}
	wantKeys := []string{"session", "weekly", "weekly_opus", "weekly_sonnet"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("window keys = %v, want %v", gotKeys, wantKeys)
	}
	if snap.Windows[0].PercentUsed != 47.5 {
		t.Fatalf("session percent = %v, want 47.5", snap.Windows[0].PercentUsed)
	}
	if snap.Credits == nil || snap.Credits.Label != "Extra usage monthly cap" || snap.Credits.Balance != 12.34 || snap.Credits.SpendLimit != 50 {
		t.Fatalf("credits = %+v", snap.Credits)
	}
}

func TestClaudeUsageRequestTimeout(t *testing.T) {
	resetClaudeCodeVersionCache(t)
	t.Setenv("PATH", t.TempDir())
	withTimeoutHTTPTransport(t, timeoutRoundTrip)
	_, _, status, _, err := fetchClaudeUsage(context.Background(), claudeCredentials{AccessToken: "access-secret"})
	if err == nil {
		t.Fatal("fetchClaudeUsage succeeded")
	}
	if status != StatusTimeout {
		t.Fatalf("status = %s, want %s", status, StatusTimeout)
	}
}

func TestClaudeUsageRetriesEmptyValuesOnce(t *testing.T) {
	resetClaudeCodeVersionCache(t)
	t.Setenv("PATH", t.TempDir())
	calls := 0
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return httpJSON(200, `{}`), nil
		}
		return httpJSON(200, `{"five_hour":{"utilization":4,"resets_at":"2030-01-01T00:00:00Z"}}`), nil
	})
	raw, _, _, _, err := fetchClaudeUsage(context.Background(), claudeCredentials{AccessToken: "access-secret"})
	if err != nil {
		t.Fatalf("fetchClaudeUsage: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if raw.FiveHour == nil || raw.FiveHour.Utilization == nil || *raw.FiveHour.Utilization != 4 {
		t.Fatalf("raw = %+v", raw)
	}
}

func TestClaudeProviderDoesNotRefreshCLIStoredCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	original := `{
  "futureTop": "preserved-root",
  "claudeAiOauth": {
    "futureToken": "preserved-token",
    "accessToken": "old-access",
    "refreshToken": "old-refresh",
    "expiresAt": 1000,
    "rateLimitTier": "pro",
    "subscriptionType": "team"
  }
}`
	path := writeClaudeAuthFixture(t, home, original)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case claudeUsageURL:
			if got := req.Header.Get("Authorization"); got != "Bearer old-access" {
				t.Fatalf("Authorization = %q", got)
			}
			return httpJSON(http.StatusUnauthorized, `{"error":"expired-secret"}`), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})
	snap, err := NewClaudeCodeProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil {
		t.Fatalf("Fetch = %+v err=%v", snap, err)
	}
	if snap.Status != StatusAuthMissing {
		t.Fatalf("status = %s, want %s", snap.Status, StatusAuthMissing)
	}
	if !strings.Contains(snap.Error, "Run `claude`") {
		t.Fatalf("error = %q", snap.Error)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("credentials file changed:\n%s", data)
	}
	assertMode(t, path, 0o600)
}

func TestClaudeAuthMissingAndScopeMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	snap, err := NewClaudeCodeProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if snap.Status != StatusAuthMissing {
		t.Fatalf("missing auth status = %s", snap.Status)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeAuthFixture(t, home, `{"claudeAiOauth":{"accessToken":"access-secret","refreshToken":"refresh-secret"}}`)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		return httpJSON(http.StatusForbidden, `{"error":"scope-secret"}`), nil
	})
	snap, err = NewClaudeCodeProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if snap.Status != StatusScopeMissing {
		t.Fatalf("scope status = %+v", snap)
	}
	assertNotContains(t, jsonString(t, snap), "scope-secret", "access-secret", "refresh-secret")
}

func TestNormalizeClaudeUsageKeepsPercentScale(t *testing.T) {
	value := 47.5
	snap, detail, err := normalizeClaudeUsage(claudeUsageRaw{
		FiveHour: &claudeWindowRaw{Utilization: &value, ResetsAt: "2030-01-01T00:00:00Z"},
	}, claudeCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if detail != nil {
		t.Fatalf("detail = %+v", detail)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Key != "session" || snap.Windows[0].PercentUsed != 47.5 {
		t.Fatalf("windows = %+v, want session=47.5", snap.Windows)
	}
}

func TestNormalizeClaudeUsageUsesOAuthAppsWeeklyFallback(t *testing.T) {
	session := 12.0
	weekly := 34.0
	snap, detail, err := normalizeClaudeUsage(claudeUsageRaw{
		FiveHour:          &claudeWindowRaw{Utilization: &session, ResetsAt: "2030-01-01T00:00:00Z"},
		SevenDayOAuthApps: &claudeWindowRaw{Utilization: &weekly, ResetsAt: "2030-01-07T00:00:00Z"},
	}, claudeCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if detail != nil {
		t.Fatalf("detail = %+v", detail)
	}
	gotKeys := []string{snap.Windows[0].Key, snap.Windows[1].Key}
	if !reflect.DeepEqual(gotKeys, []string{"session", "weekly"}) {
		t.Fatalf("keys = %v", gotKeys)
	}
	if snap.Windows[1].PercentUsed != 34 {
		t.Fatalf("weekly percent = %v, want 34", snap.Windows[1].PercentUsed)
	}
}

func TestNormalizeClaudeUsagePrefersSevenDayOverOAuthApps(t *testing.T) {
	session := 12.0
	weekly := 34.0
	oauthApps := 56.0
	snap, detail, err := normalizeClaudeUsage(claudeUsageRaw{
		FiveHour:          &claudeWindowRaw{Utilization: &session, ResetsAt: "2030-01-01T00:00:00Z"},
		SevenDay:          &claudeWindowRaw{Utilization: &weekly, ResetsAt: "2030-01-07T00:00:00Z"},
		SevenDayOAuthApps: &claudeWindowRaw{Utilization: &oauthApps, ResetsAt: "2030-01-07T00:00:00Z"},
	}, claudeCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if detail != nil {
		t.Fatalf("detail = %+v", detail)
	}
	gotKeys := []string{snap.Windows[0].Key, snap.Windows[1].Key}
	if !reflect.DeepEqual(gotKeys, []string{"session", "weekly"}) {
		t.Fatalf("keys = %v", gotKeys)
	}
	if snap.Windows[1].PercentUsed != 34 {
		t.Fatalf("weekly percent = %v, want seven_day 34", snap.Windows[1].PercentUsed)
	}
}

func TestNormalizeClaudeUsageDetailsMissingWeeklyFallback(t *testing.T) {
	session := 12.0
	sonnet := 56.0
	snap, detail, err := normalizeClaudeUsage(claudeUsageRaw{
		FiveHour:       &claudeWindowRaw{Utilization: &session, ResetsAt: "2030-01-01T00:00:00Z"},
		SevenDaySonnet: &claudeWindowRaw{Utilization: &sonnet, ResetsAt: "2030-01-07T00:00:00Z"},
	}, claudeCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].Key != "session" || snap.Windows[1].Key != "weekly_sonnet" {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if detail == nil || detail.Fields["seven_day"] != "missing" || detail.Fields["seven_day_oauth_apps"] != "missing" {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestClaudeExtraUsageAmountUsesMinorUnits(t *testing.T) {
	if got := claudeExtraUsageAmount(1234); got != 12.34 {
		t.Fatalf("amount = %v, want 12.34", got)
	}
}

func TestNormalizeClaudeUsageSupportsSpendLimitOnly(t *testing.T) {
	used := 763.0
	limit := 2000.0
	enabled := true
	snap, detail, err := normalizeClaudeUsage(claudeUsageRaw{
		ExtraUsage: &claudeExtraUsageRaw{
			IsEnabled:    &enabled,
			UsedCredits:  &used,
			MonthlyLimit: &limit,
		},
	}, claudeCredentials{SubscriptionType: "team"})
	if err != nil {
		t.Fatal(err)
	}
	if detail != nil {
		t.Fatalf("detail = %+v", detail)
	}
	if len(snap.Windows) != 0 {
		t.Fatalf("windows = %+v, want none", snap.Windows)
	}
	if snap.Credits == nil || snap.Credits.Balance != 7.63 || snap.Credits.SpendLimit != 20 {
		t.Fatalf("credits = %+v", snap.Credits)
	}
}

func TestClaudeFetchErrorRedactsSecretsThroughManagerAndHTTP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeAuthFixture(t, home, `{"claudeAiOauth":{"accessToken":"access-secret","refreshToken":"refresh-secret"}}`)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		return httpJSON(http.StatusInternalServerError, `{"message":"raw-live-value"}`), nil
	})
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderClaudeCode] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, DefaultRegistry())
	resp, err := m.Snapshot(context.Background(), ProviderClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	body := jsonString(t, resp)
	assertNotContains(t, body, "access-secret", "refresh-secret", "raw-live-value")
	cacheData, err := os.ReadFile(store.CachePath())
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, string(cacheData), "access-secret", "refresh-secret", "raw-live-value")

	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/usages?provider=claude-code", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rr.Code)
	}
	assertNotContains(t, rr.Body.String(), "access-secret", "refresh-secret", "raw-live-value")
}

func TestClaudeSnakeCaseCredentials(t *testing.T) {
	home := t.TempDir()
	path := writeClaudeAuthFixture(t, home, `{
  "claude_ai_oauth": {
    "access_token": "access-secret",
    "refresh_token": "refresh-secret",
    "expires_at": 1893456000000,
    "rate_limit_tier": "pro",
    "subscription_type": "team"
  }
}`)
	creds, detail, err := loadClaudeAuth(path)
	if err != nil {
		t.Fatalf("loadClaudeAuth error: %v detail=%+v", err, detail)
	}
	if creds.AccessToken != "access-secret" || creds.RefreshToken != "refresh-secret" || creds.RateLimitTier != "pro" || creds.SubscriptionType != "team" {
		t.Fatalf("creds = %+v", creds)
	}
	if creds.ExpiresAt == nil || !creds.ExpiresAt.Equal(time.UnixMilli(1893456000000).UTC()) {
		t.Fatalf("expiresAt = %+v", creds.ExpiresAt)
	}
}
