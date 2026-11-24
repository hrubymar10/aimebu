package usages

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

// An admin's view keeps the limits and drops everything that names an account or how it signs in, and nothing is added to it by a field the snapshot
// gains later.
func TestForAdminKeepsLimitsAndDropsAccountIdentity(t *testing.T) {
	reset := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	full := Response{SwitcherEnabled: true, CredentialAlerts: []CredentialAlert{{Kind: "reauth_required", Tool: "claude", Profile: "main", Role: "store", Message: "log in as owner@example.com"}}}
	full.Settings.PercentDisplay, full.Settings.RefreshIntervalSec, full.Settings.WarmupSchedule = "left", 120, "secret-schedule"
	full.Settings.TokenAutoRefreshState = map[string]TokenAutoRefreshToolState{"claude": {}}
	full.Providers = []Provider{{ProviderName: "claude-code", Label: "Claude Code", OrderNumber: 1, Enabled: true, Available: true, EnterpriseHost: "https://ghe.example.com", TokenConfigured: true, CookieConfigured: true, APIKeyConfigured: true, AuthMode: "cookie",
		Profiles: []Profile{{ProfileName: "main", AlertIdentity: "id-1", AlertCredential: "cred-1", Active: true, HasCredentials: true, Email: "owner@example.com", Status: "ok", Plan: "max",
			Windows: []Window{{Key: "session", PercentUsed: 38, ResetAt: &reset}}, Credits: &Credits{Label: "credits", Balance: 4}, Error: "token sk-ant-leak was rejected", ErrorDetail: &providers.ErrorDetail{Fields: map[string]string{"header": "Authorization: Bearer leak"}}}}}}
	shown, err := json.Marshal(full.ForAdmin())
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"owner@example.com", "ghe.example.com", "sk-ant-leak", "Bearer leak", "secret-schedule", "reauth_required", "id-1", "cred-1", "cookie", "token_configured", "api_key", "auth_mode", "credential_alerts", "\"switcher_enabled\":true", "\"claude\":{"} {
		if strings.Contains(string(shown), leak) {
			t.Errorf("an admin's view holds %q: %s", leak, shown)
		}
	}
	for _, kept := range []string{"claude-code", "Claude Code", "main", "max", "\"has_credentials\":true", "percent_used", "session", "credits", "percent_display", "refresh_interval_sec"} {
		if !strings.Contains(string(shown), kept) {
			t.Errorf("an admin's view lost %q: %s", kept, shown)
		}
	}
}

// The admin view is an allow-list: the structs it copies from are pinned by field name, so a field added to any of them fails here until someone decides whether an
// admin may see it (and adds it to ForAdmin and to this list, or to the list of what is dropped).
func TestForAdminAllowListCoversEveryField(t *testing.T) {
	fields := func(value any) []string {
		var out []string
		for typ, i := reflect.TypeOf(value), 0; i < typ.NumField(); i++ {
			out = append(out, typ.Field(i).Name)
		}
		return out
	}
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"Response", fields(Response{}), []string{"Providers", "Settings", "SwitcherEnabled", "CredentialAlerts"}},
		{"Provider", fields(Provider{}), []string{"ProviderName", "Label", "OrderNumber", "Enabled", "Available", "EnterpriseHost", "TokenConfigured", "CookieConfigured", "APIKeyConfigured", "AuthMode", "Profiles"}},
		{"Profile", fields(Profile{}), []string{"ProfileName", "AlertIdentity", "AlertCredential", "ToolMigrated", "Active", "HasCredentials", "Email", "ExpiresAt", "Status", "Plan", "Windows", "Credits", "LastRefreshAt", "Stale", "ErrorKind", "RetryAt", "Error", "ErrorDetail", "TokenTail"}},
		{"Window", fields(Window{}), []string{"Key", "PercentUsed", "ResetAt", "WindowDurationSeconds", "Pace", "History", "RejectedAt"}},
		{"Pace", fields(Pace{}), []string{"ExpectedPercent", "DeltaPercent", "State", "EtaSeconds", "LastsToReset"}},
		{"Credits", fields(Credits{}), []string{"Label", "Balance", "SpendLimit"}},
		{"HistoryPoint", fields(HistoryPoint{}), []string{"T", "P"}},
	} {
		if c.want == nil {
			t.Errorf("%s has no pinned field list: a struct left out of the list is not checked", c.name)
			continue
		}
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s gained or lost a field: %v (was %v): decide what an admin may see of it in ForAdmin, then update this list", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name string
		got  []string
	}{{"Settings", func() []string {
		var out []string
		for typ, i := reflect.TypeOf(Response{}.Settings), 0; i < typ.NumField(); i++ {
			out = append(out, typ.Field(i).Name)
		}
		return out
	}()}} {
		want := []string{"PercentDisplay", "RefreshIntervalSec", "AlertWarningRemaining", "AlertCriticalRemaining", "AlertPaceEnabled", "AlertResetNoticeEnabled", "AlertExpiryEnabled", "TokenAutoRefresh", "ClaudeAutoRefresh", "TokenAutoRefreshAvailable", "TokenAutoRefreshState", "WarmupMode", "WarmupSchedule", "WarmupAvailable"}
		if !reflect.DeepEqual(c.got, want) {
			t.Errorf("%s gained or lost a field: %v: decide what an admin may see of it in ForAdmin, then update this list", c.name, c.got)
		}
	}
}

func TestErrorKindClassifiesRateLimitOnce(t *testing.T) {
	limited := &providers.ErrorDetail{Fields: map[string]string{"usage.http_status": "http_429"}}
	for _, c := range []struct {
		snap providers.Snapshot
		want string
	}{
		{providers.Snapshot{Status: providers.StatusFetchError, ErrorDetail: limited}, "rate_limited"},
		{providers.Snapshot{Status: providers.StatusFetchError, ErrorDetail: &providers.ErrorDetail{Fields: map[string]string{"usage.http_status": "http_500"}}}, "fetch_failed"},
		{providers.Snapshot{Status: providers.StatusTimeout}, "fetch_failed"},
		{providers.Snapshot{Status: providers.StatusOK}, ""},
	} {
		if got := errorKind(c.snap); got != c.want {
			t.Errorf("errorKind(%v) = %q, want %q", c.snap.Status, got, c.want)
		}
	}
	shown := Response{Providers: []Provider{{Profiles: []Profile{{Error: "secret", ErrorKind: "rate_limited"}}}}}.ForAdmin()
	if p := shown.Providers[0].Profiles[0]; p.ErrorKind != "rate_limited" || p.Error != "" {
		t.Errorf("an admin keeps error_kind and never the text: %+v", p)
	}
}
