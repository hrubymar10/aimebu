package server

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

type usageFlowTransport func(*http.Request) (*http.Response, error)

func (f usageFlowTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCopilotHostAndTokenChangeTogetherAfterDeviceSignIn(t *testing.T) {
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := f.srv.Usages.ExtraStore()
	usageCalls := 0
	store.Client = &http.Client{Transport: usageFlowTransport(func(req *http.Request) (*http.Response, error) {
		usageCalls++
		if req.URL.Host != "api.old.example.com" || req.Header.Get("Authorization") != "token old-token" {
			t.Errorf("pending sign-in sent old token to %s with auth %q", req.URL.Host, req.Header.Get("Authorization"))
		}
		body := `{"email":"old@example.com"}`
		if strings.HasSuffix(req.URL.Path, "/copilot_internal/user") {
			body = `{"copilot_plan":"business","quota_snapshots":{"premium_interactions":{"entitlement":100,"remaining":50}}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	_, err = store.UpdateConfig(func(cfg *providers.Config) error {
		cfg.Providers[providers.ProviderGitHubCopilot] = providers.ProviderConfig{Enabled: true, Token: "old-token", EnterpriseHost: "https://api.old.example.com"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.CopilotFlow = providers.NewCopilotFlowWithClient(&http.Client{Transport: usageFlowTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "new.example.com" || req.Header.Get("Authorization") != "" {
			t.Errorf("device flow sent credential to %s", req.URL.Host)
		}
		body := `{"access_token":"new-token"}`
		if strings.HasSuffix(req.URL.Path, "/device/code") {
			if req.URL.Path != "/login/device/code" {
				t.Errorf("device-code path = %q", req.URL.Path)
			}
			body = `{"device_code":"device-secret","user_code":"ABCD","verification_uri":"https://new.example.com/login/device","expires_in":90,"interval":5}`
		} else if req.URL.Path != "/login/oauth/access_token" {
			t.Errorf("token path = %q", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})})
	resp := f.request("POST", "/api/usages/copilot/login/start", map[string]string{"enterprise_host": "https://new.example.com"}, true)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("start status=%d body=%s", resp.StatusCode, data)
	}
	var started struct {
		FlowID string `json:"flow_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil || started.FlowID == "" {
		t.Fatalf("start response %+v: %v", started, err)
	}
	before, err := store.LoadConfig()
	if err != nil || before.Providers[providers.ProviderGitHubCopilot].Token != "old-token" || before.Providers[providers.ProviderGitHubCopilot].EnterpriseHost != "https://api.old.example.com" {
		t.Fatalf("pending sign-in changed saved token/host: %+v, %v", before.Providers[providers.ProviderGitHubCopilot], err)
	}
	f.srv.Usages.RefreshProvider(t.Context(), providers.ProviderGitHubCopilot)
	if usageCalls == 0 {
		t.Fatal("pending sign-in did not exercise usage refresh")
	}
	resp = f.request("POST", "/api/usages/copilot/login/poll", map[string]string{"flow_id": started.FlowID}, true)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("poll status=%d body=%s", resp.StatusCode, data)
	}
	after, err := store.LoadConfig()
	if err != nil || after.Providers[providers.ProviderGitHubCopilot].Token != "new-token" || after.Providers[providers.ProviderGitHubCopilot].EnterpriseHost != "https://api.new.example.com" {
		t.Fatalf("completed sign-in did not switch token/host together: %+v, %v", after.Providers[providers.ProviderGitHubCopilot], err)
	}
}

func TestUsageCredentialRoutesKeepSecretsPrivate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, body any, want int) string {
		t.Helper()
		resp := f.request("POST", path, body, true)
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, data)
		}
		return string(data)
	}
	mistral := post("/api/usages/mistral/config", map[string]string{"cookie": "Cookie: csrftoken=abc; ory_session_test=mistral-secret"}, 200)
	if strings.Contains(mistral, "mistral-secret") {
		t.Fatal("Mistral cookie returned")
	}
	ollama := post("/api/usages/ollama/config", map[string]string{"auth_mode": "api_key", "api_key": "'ollama-secret'"}, 200)
	if strings.Contains(ollama, "ollama-secret") {
		t.Fatal("Ollama key returned")
	}
	resp := f.request("GET", "/api/usages", nil, false)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(data), "mistral-secret") || strings.Contains(string(data), "ollama-secret") {
		t.Fatalf("usage response %d %s", resp.StatusCode, data)
	}
	var parsed struct {
		Providers []struct {
			ProviderName     string `json:"provider_name"`
			CookieConfigured bool   `json:"cookie_configured"`
			APIKeyConfigured bool   `json:"api_key_configured"`
			Enabled          bool   `json:"enabled"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Providers) != 5 || !parsed.Providers[3].CookieConfigured || !parsed.Providers[4].APIKeyConfigured {
		t.Fatalf("provider flags %+v", parsed.Providers)
	}
	raw, err := os.ReadFile(f.srv.Usages.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "mistral-secret") || !strings.Contains(string(raw), "ollama-secret") {
		t.Fatal("credentials not stored")
	}
	if strings.Contains(string(raw), "'ollama-secret'") {
		t.Fatal("quoted API key was not normalized")
	}
	post("/api/usages/ollama/config", map[string]string{"auth_mode": "cookie"}, 200)
	if f.srv.Usages.Snapshot().Providers[4].Enabled {
		t.Fatal("cookie mode enabled without a cookie")
	}
	info, err := os.Stat(f.srv.Usages.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	post("/api/usages/mistral/config", map[string]string{"cookie": ""}, 200)
	post("/api/usages/ollama/config", map[string]string{"auth_mode": "auto", "api_key": "", "cookie": ""}, 200)
	resp = f.request("GET", "/api/usages", nil, false)
	var cleared usages.Response
	if err := json.NewDecoder(resp.Body).Decode(&cleared); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || cleared.Providers[3].Profiles[0].Status != "disabled" || cleared.Providers[4].Profiles[0].Status != "disabled" || cleared.Providers[3].Profiles[0].HasCredentials || cleared.Providers[4].Profiles[0].HasCredentials {
		t.Fatalf("cleared provider status: HTTP %d, Mistral %q, Ollama %q", resp.StatusCode, cleared.Providers[3].Profiles[0].Status, cleared.Providers[4].Profiles[0].Status)
	}
	raw, err = os.ReadFile(f.srv.Usages.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mistral-secret") || strings.Contains(string(raw), "ollama-secret") {
		t.Fatal("credentials survived clear")
	}
}

func TestUsageSettingsRequireOwnerSessionAndCSRF(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"refresh_interval_sec": 15, "percent_display": "used", "provider_order": []string{"mistral", "codex"}}
	resp := f.request("POST", "/api/usages/settings", body, false)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF status=%d", resp.StatusCode)
	}
	cookie := f.cookie
	f.cookie = nil
	resp = f.request("POST", "/api/usages/settings", body, true)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("missing session status=%d", resp.StatusCode)
	}
	f.cookie = cookie
	resp = f.request("POST", "/api/usages/settings", body, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("settings status=%d", resp.StatusCode)
	}
	snapshot := f.srv.Usages.Snapshot()
	if snapshot.Settings.RefreshIntervalSec != 15 || snapshot.Settings.PercentDisplay != "used" || snapshot.Providers[3].OrderNumber != 0 {
		t.Fatalf("settings %+v", snapshot)
	}
	for _, provider := range snapshot.Providers {
		if provider.Profiles[0].Status == "refreshing" {
			t.Fatalf("display-only settings reset %s", provider.ProviderName)
		}
	}
	resp = f.request("POST", "/api/usages/providers", map[string]any{"provider": "mistral", "enabled": true}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("enable without cookie status=%d", resp.StatusCode)
	}
}
