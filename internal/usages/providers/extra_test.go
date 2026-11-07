package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestV1ConfigPreservedAndPrivate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	old := `{"refresh_interval_sec":120,"percent_display":"left","claude_auto_refresh":true,"warmup_mode":"smart","custom_future_flag":{"keep":1},"providers":{"github-copilot":{"enabled":true,"token":"copilot-secret","enterprise_host":"https://api.github.example.com"}}}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	cfg, err := store.UpdateConfig(func(cfg *Config) error {
		pc := cfg.Providers[ProviderMistral]
		pc.Cookie = "mistral-secret"
		pc.Enabled = true
		cfg.Providers[ProviderMistral] = pc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TokenAutoRefresh || cfg.WarmupMode != "smart" || cfg.Providers[ProviderGitHubCopilot].Token != "copilot-secret" {
		t.Fatalf("config lost v1 fields: %+v", cfg)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"custom_future_flag"`) || !strings.Contains(string(raw), `"copilot-secret"`) {
		t.Fatal("old config keys or token lost")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode %o", info.Mode().Perm())
	}
}

func TestMalformedConfigFallsBackAndCanBeRepaired(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":{"github-copilot":`), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	cfg, err := store.LoadConfig()
	if !errors.Is(err, ErrInvalidConfig) || cfg.RefreshIntervalSec != 120 || cfg.PercentDisplay != "left" {
		t.Fatalf("fallback = %+v, %v", cfg, err)
	}
	_, err = store.UpdateConfig(func(cfg *Config) error {
		cfg.PercentDisplay = "used"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := store.LoadConfig()
	if err != nil || config.PercentDisplay != "used" || config.RefreshIntervalSec != 120 {
		t.Fatalf("repaired config = %+v, %v", config, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("repaired mode = %v, %v", info, err)
	}
}

func TestOutOfRangeAlertThresholdsPreserveOtherConfig(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	data := []byte(`{"alert_warning_remaining":120,"alert_critical_remaining":10,"percent_display":"used","providers":{"github-copilot":{"enabled":true,"token":"saved-secret"}}}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewStore(root).LoadConfig()
	if err != nil || cfg.AlertWarningRemaining != 20 || cfg.AlertCriticalRemaining != 10 || cfg.PercentDisplay != "used" || !cfg.Providers["github-copilot"].Enabled {
		t.Fatalf("threshold fallback lost config: %+v %v", cfg, err)
	}
	if cfg.Providers["github-copilot"].Token != "saved-secret" {
		t.Fatal("credential was dropped")
	}
}

func TestWrongTypeConfigRefusesSaveWithoutLosingCredentials(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	original := []byte(`{"percent_display":{},"providers":{"github-copilot":{"enabled":true,"token":"saved-secret"}}}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(root).UpdateConfig(func(cfg *Config) error {
		cfg.RefreshIntervalSec = 30
		return nil
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("wrong-type config update error = %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(original) {
		t.Fatalf("config changed after refused save: %q, %v", current, err)
	}
}

func TestNullConfigIsInvalidAndCannotBeSavedOver(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, []byte("null\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	if _, err := store.LoadConfig(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("null config load = %v", err)
	}
	if _, err := store.UpdateConfig(func(cfg *Config) error { cfg.PercentDisplay = "used"; return nil }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("null config save = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "null\n" {
		t.Fatalf("null config changed to %q: %v", data, err)
	}
}

func TestPortedProviderShapes(t *testing.T) {
	var raw copilotUsageRaw
	if err := json.Unmarshal([]byte(`{"copilot_plan":"business","quota_reset_date":"2030-01-01T00:00:00Z","quota_snapshots":{"premium_interactions":{"entitlement":100,"remaining":73,"percent_remaining":73,"quota_id":"premium"}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	snap, _, err := normalizeCopilotUsage(raw)
	if err != nil || snap.Status != StatusOK || len(snap.Windows) != 1 || snap.Windows[0].PercentUsed != 27 {
		t.Fatalf("copilot %+v %v", snap, err)
	}
	mistral, _, err := parseMistralCookieHeader("Cookie: csrftoken=abcdef1234; ory_session_test=secret; unrelated=drop")
	if err != nil || strings.Contains(mistral.VibeCookieHeader(), "unrelated") {
		t.Fatalf("mistral cookie %q %v", mistral.VibeCookieHeader(), err)
	}
	ollama, err := NormalizeOllamaCookieHeader("Cookie: wos-session=abc; theme=dark")
	if err != nil || !strings.Contains(ollama, "wos-session=abc") {
		t.Fatalf("ollama cookie %q %v", ollama, err)
	}
}

func TestPortedMistralVibeAndOllamaWindows(t *testing.T) {
	oldNow := mistralNow
	mistralNow = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { mistralNow = oldNow })
	raw, detail, err := decodeMistralVibeUsage([]byte(`[{"result":{"data":{"json":{"usage_percentage":28.5,"payg_enabled":true,"reset_at":"2026-07-01T00:00:00Z"}}}}]`))
	if err != nil {
		t.Fatalf("Mistral decode detail=%+v err=%v", detail, err)
	}
	mistral, detail, err := normalizeMistralVibeUsage(raw)
	if err != nil || mistral.Status != StatusOK || len(mistral.Windows) != 1 || mistral.Windows[0].PercentUsed != 28.5 || mistral.Windows[0].Pace == nil {
		t.Fatalf("Mistral snapshot %+v detail=%+v err=%v", mistral, detail, err)
	}
	html := `<span>Cloud Usage</span><span>Pro</span><div>Session usage <span>42% used</span><time data-time="2026-10-15T22:00:00Z"></time></div>` +
		`<div>Weekly usage <span style="width: 12.5%"></span><time data-time="2026-10-20T22:00:00Z"></time></div>`
	ollama, detail, err := parseOllamaSettingsHTML([]byte(html))
	if err != nil || ollama.Plan != "Pro" || len(ollama.Windows) != 2 || ollama.Windows[0].PercentUsed != 42 || ollama.Windows[1].PercentUsed != 12.5 {
		t.Fatalf("Ollama snapshot %+v detail=%+v err=%v", ollama, detail, err)
	}
}

func TestCopilotDeviceFlowBoundedAndTokenPrivate(t *testing.T) {
	old := usageHTTPClient
	calls := 0
	usageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if strings.HasSuffix(req.URL.Path, "/device/code") {
			return response(`{"device_code":"device-secret","user_code":"ABCD","verification_uri":"https://github.com/login/device","expires_in":90,"interval":5}`), nil
		}
		if calls == 2 {
			return response(`{"error":"authorization_pending"}`), nil
		}
		return response(`{"access_token":"token-secret","token_type":"bearer"}`), nil
	})}
	t.Cleanup(func() { usageHTTPClient = old })
	flow := NewCopilotFlow()
	now := time.Now().UTC()
	flow.inner.now = func() time.Time { return now }
	start, err := flow.Start(context.Background(), "")
	if err != nil || start.UserCode != "ABCD" {
		t.Fatalf("start %+v %v", start, err)
	}
	encoded, _ := json.Marshal(start)
	if strings.Contains(string(encoded), "device-secret") {
		t.Fatal("device secret returned")
	}
	first, token, _, err := flow.Poll(context.Background(), start.FlowID)
	if err != nil || first.Status != "pending" || token != "" {
		t.Fatalf("first %+v %q %v", first, token, err)
	}
	second, token, _, err := flow.Poll(context.Background(), start.FlowID)
	if err != nil || second.RetryAfter <= 0 || token != "" || calls != 2 {
		t.Fatalf("early poll %+v %q %d %v", second, token, calls, err)
	}
	now = now.Add(6 * time.Second)
	third, token, host, err := flow.Poll(context.Background(), start.FlowID)
	if err != nil || third.Status != "success" || token != "token-secret" || host != copilotDefaultAPIHost {
		t.Fatalf("success %+v %q %v", third, token, err)
	}
	encoded, _ = json.Marshal(third)
	if strings.Contains(string(encoded), token) {
		t.Fatal("token returned through poll response")
	}
}

func TestCopilotStartSweepsExpiredFlows(t *testing.T) {
	flow := NewCopilotFlowWithClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(`{"device_code":"fresh-code","user_code":"NEW","verification_uri":"https://github.com/login/device","expires_in":90,"interval":5}`), nil
	})})
	now := time.Now().UTC()
	flow.inner.now = func() time.Time { return now }
	flow.inner.flows["expired"] = &copilotFlowState{ExpiresAt: now.Add(-time.Second)}
	flow.inner.flows["valid"] = &copilotFlowState{ExpiresAt: now.Add(time.Minute)}
	started, err := flow.Start(context.Background(), "")
	if err != nil || started.FlowID == "" {
		t.Fatalf("start = %+v, %v", started, err)
	}
	if flow.inner.flows["expired"] != nil || flow.inner.flows["valid"] == nil || flow.inner.flows[started.FlowID] == nil {
		t.Fatalf("flows after sweep: %+v", flow.inner.flows)
	}
}

func TestCopilotEnterpriseDeviceAndUsageHosts(t *testing.T) {
	cases := []struct {
		input, login, api string
	}{
		{"", "https://github.com", "https://api.github.com"},
		{"https://octocorp.ghe.com/login", "https://octocorp.ghe.com", "https://api.octocorp.ghe.com"},
		{"https://octocorp.ghe.com:8443/login", "https://octocorp.ghe.com:8443", "https://api.octocorp.ghe.com:8443"},
		{"octocorp.ghe.com", "https://octocorp.ghe.com", "https://api.octocorp.ghe.com"},
		{"https://api.octocorp.ghe.com", "https://octocorp.ghe.com", "https://api.octocorp.ghe.com"},
	}
	for _, tc := range cases {
		login, api, err := copilotHosts(tc.input)
		if err != nil || login != tc.login || api != tc.api {
			t.Errorf("hosts(%q) = %q, %q, %v", tc.input, login, api, err)
		}
		usageURL, err := copilotUsageURL(api)
		if err != nil || usageURL != tc.api+"/copilot_internal/user" {
			t.Errorf("usage URL(%q) = %q, %v", tc.input, usageURL, err)
		}
	}
	for _, input := range []string{"foo bar", "http://octocorp.ghe.com", "https://octocorp.ghe.com/other", "https://user@octocorp.ghe.com", "https://octocorp.ghe.com:99999/login"} {
		if _, _, err := copilotHosts(input); err == nil {
			t.Errorf("invalid host %q accepted", input)
		}
	}
}

func TestCopilotEmailUsesEnterpriseHost(t *testing.T) {
	store := NewStore(t.TempDir())
	_, err := store.UpdateConfig(func(cfg *Config) error {
		cfg.Providers[ProviderGitHubCopilot] = ProviderConfig{Token: "private-token", EnterpriseHost: "https://api.github.example.com", Enabled: true}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	old := usageHTTPClient
	usageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.github.example.com/user" || req.Header.Get("Authorization") != "token private-token" {
			t.Fatalf("GitHub identity request %s authorization=%q", req.URL, req.Header.Get("Authorization"))
		}
		return response(`{"email":"enterprise@example.com"}`), nil
	})}
	t.Cleanup(func() { usageHTTPClient = old })
	provider := NewCopilotProvider().(EmailFetcher)
	email, err := provider.FetchEmail(context.Background(), store)
	if err != nil || email != "enterprise@example.com" {
		t.Fatalf("email=%q err=%v", email, err)
	}
	if scope := NewCopilotProvider().(EmailScopeProvider).EmailScope(store); scope != "https://api.github.example.com" {
		t.Fatalf("scope=%q", scope)
	}
}

func TestCopilotAndOllamaTimeoutsReturnSnapshotErrors(t *testing.T) {
	old := usageHTTPClient
	oldOllama := ollamaHTTPClient
	usageHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	ollamaHTTPClient = usageHTTPClient
	t.Cleanup(func() { usageHTTPClient, ollamaHTTPClient = old, oldOllama })
	store := NewStore(t.TempDir())
	_, err := store.UpdateConfig(func(cfg *Config) error {
		cfg.Providers[ProviderGitHubCopilot] = ProviderConfig{Enabled: true, Token: "token"}
		cfg.Providers[ProviderOllamaCloud] = ProviderConfig{Enabled: true, AuthMode: OllamaAuthCookie, Cookie: "wos-session=abc", APIKey: "key"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		fetch func() (Snapshot, error)
	}{
		{"copilot", func() (Snapshot, error) { return NewCopilotProvider().Fetch(t.Context(), store) }},
		{"ollama cookie", func() (Snapshot, error) { return NewOllamaCloudProvider().Fetch(t.Context(), store) }},
		{"ollama API key", func() (Snapshot, error) { return fetchOllamaWithAPIKey(t.Context(), "key") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fetch()
			var snapshotErr *SnapshotError
			if !errors.As(err, &snapshotErr) || snapshotErr.Snapshot.Status != StatusTimeout {
				t.Fatalf("timeout result = %+v, %v", snapshotErr, err)
			}
		})
	}
}

func TestTokenAutoRefreshLegacyKeyAndMirror(t *testing.T) {
	for _, tc := range []struct {
		body        string
		want        bool
		wantManaged bool
	}{
		{`{"claude_auto_refresh":true}`, true, true},
		{`{"claude_auto_refresh":true,"token_auto_refresh":false}`, false, false},
		{`{"claude_auto_refresh":false,"token_auto_refresh":true}`, true, false},
	} {
		root := t.TempDir()
		path := filepath.Join(root, "config.json")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		store := NewStore(root)
		cfg, err := store.LoadConfig()
		if err != nil || cfg.TokenAutoRefresh != tc.want || cfg.TokenAutoRefreshManaged != tc.wantManaged {
			t.Fatalf("load %s = %+v %v, want on=%v managed=%v", tc.body, cfg, err, tc.want, tc.wantManaged)
		}
		if _, err := store.UpdateConfig(func(*Config) error { return nil }); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		var saved map[string]any
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"token_auto_refresh", "claude_auto_refresh"} {
			if saved[key] != tc.want {
				t.Fatalf("saved %s = %v, want %v (%s)", key, saved[key], tc.want, raw)
			}
		}
	}
}

// An omitempty field that turns false must not leave the old true in the file.
func TestUpdateConfigClearsTheManagedFlag(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	if _, err := store.UpdateConfig(func(c *Config) error { c.TokenAutoRefreshManaged = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateConfig(func(c *Config) error { c.TokenAutoRefreshManaged = false; return nil }); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig()
	if err != nil || cfg.TokenAutoRefreshManaged {
		t.Fatalf("managed flag survived clearing: %+v %v", cfg, err)
	}
}

// The address the person is sent to for the code must be https: anything else from the device-code endpoint fails the start.
func TestCopilotDeviceStartRefusesAVerificationAddressThatIsNotHTTPS(t *testing.T) {
	old := usageHTTPClient
	t.Cleanup(func() { usageHTTPClient = old })
	for _, body := range []string{
		`{"device_code":"d","user_code":"ABCD","verification_uri":"javascript:alert(1)","expires_in":90,"interval":5}`,
		`{"device_code":"d","user_code":"ABCD","verification_uri":"http://github.com/login/device","expires_in":90,"interval":5}`,
		`{"device_code":"d","user_code":"ABCD","verification_uri":"https://github.com/login/device","verification_uri_complete":"http://github.com/x","expires_in":90,"interval":5}`,
		`{"device_code":"d","user_code":"ABCD","verification_uri":"https://user:pw@github.com/login/device","expires_in":90,"interval":5}`,
	} {
		usageHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(body), nil })}
		if _, err := NewCopilotFlow().Start(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "not https") {
			t.Fatalf("%s: err=%v", body, err)
		}
	}
}
