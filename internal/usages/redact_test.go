package usages

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
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
		{"Profile", fields(Profile{}), []string{"ProfileName", "AlertIdentity", "AlertCredential", "Active", "HasCredentials", "Email", "ExpiresAt", "Status", "Plan", "Windows", "Credits", "LastRefreshAt", "Stale", "Error", "ErrorDetail"}},
		{"Window", fields(Window{}), []string{"Key", "PercentUsed", "ResetAt", "WindowDurationSeconds", "Pace", "History"}},
		{"Pace", fields(Pace{}), []string{"ExpectedPercent", "DeltaPercent", "State", "EtaSeconds", "LastsToReset"}},
		{"Credits", fields(Credits{}), []string{"Label", "Balance", "SpendLimit"}},
		{"HistoryPoint", fields(HistoryPoint{}), nil},
	} {
		if c.want == nil {
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
		want := []string{"PercentDisplay", "RefreshIntervalSec", "AlertWarningRemaining", "AlertCriticalRemaining", "TokenAutoRefresh", "ClaudeAutoRefresh", "TokenAutoRefreshAvailable", "TokenAutoRefreshState", "WarmupMode", "WarmupSchedule", "WarmupAvailable"}
		if !reflect.DeepEqual(c.got, want) {
			t.Errorf("%s gained or lost a field: %v: decide what an admin may see of it in ForAdmin, then update this list", c.name, c.got)
		}
	}
}
