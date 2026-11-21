package providers

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func ptr(value float64) *float64 { return &value }

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

// ── Ollama API-key balance (GET /api/balance) ───────────────────────────────

func TestOllamaAPIKeyBalanceIncludedAllowanceAndPurchasedCredits(t *testing.T) {
	snap, detail, err := normalizeOllamaBalance([]byte(`{"included":{"allowance_usd":60,"balance_usd":"46.50","period":{"until":"2026-12-01T00:00:00Z"}},"purchased":{"balance_usd":12.34}}`))
	if err != nil || detail != nil {
		t.Fatalf("balance %+v detail=%+v err=%v", snap, detail, err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Key != "monthly" || snap.Windows[0].PercentUsed != 22.5 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	window := snap.Windows[0]
	if window.ResetAt == nil || !window.ResetAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) || window.WindowDurationSeconds != ollamaMonthlyWindowSeconds || window.Pace == nil {
		t.Fatalf("window = %+v", window)
	}
	if snap.Credits == nil || snap.Credits.Label != "Credit balance" || snap.Credits.Balance != 12.34 {
		t.Fatalf("credits = %+v", snap.Credits)
	}
}

func TestOllamaAPIKeyBalanceEdgeShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		windows int
		credits *float64
	}{
		{"purchased only", `{"purchased":{"balance_usd":5}}`, 0, ptr(5)},
		{"zero allowance", `{"included":{"allowance_usd":0,"balance_usd":0}}`, 0, nil},
		{"allowance over balance", `{"included":{"allowance_usd":10,"balance_usd":12}}`, 1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, _, err := normalizeOllamaBalance([]byte(tc.body))
			if err != nil || snap.Status != StatusOK {
				t.Fatalf("balance %+v err=%v", snap, err)
			}
			if len(snap.Windows) != tc.windows {
				t.Fatalf("windows = %+v", snap.Windows)
			}
			switch {
			case tc.credits == nil && snap.Credits != nil:
				t.Fatalf("unexpected credits %+v", snap.Credits)
			case tc.credits != nil && (snap.Credits == nil || snap.Credits.Balance != *tc.credits):
				t.Fatalf("credits = %+v", snap.Credits)
			}
		})
	}
}

func TestOllamaAPIKeyBalanceLegacyRemainingPercentWindows(t *testing.T) {
	snap, _, err := normalizeOllamaBalance([]byte(`{"included":{"session":{"remaining_percent":100,"resets_at":"2026-11-06T18:00:00Z"},"weekly":{"remaining_percent":"84.96","resets_at":"2026-11-09T00:00:00Z"}},"purchased":{"balance_usd":0}}`))
	if err != nil {
		t.Fatalf("legacy balance err=%v", err)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].Key != "session" || snap.Windows[0].PercentUsed != 0 || snap.Windows[0].WindowDurationSeconds != ollamaSessionWindowSeconds {
		t.Fatalf("session window = %+v", snap.Windows)
	}
	if snap.Windows[1].Key != "weekly" || math.Abs(snap.Windows[1].PercentUsed-15.04) > 1e-9 || snap.Windows[1].WindowDurationSeconds != ollamaWeeklyWindowSeconds {
		t.Fatalf("weekly window = %+v", snap.Windows)
	}
	if snap.Credits == nil || snap.Credits.Balance != 0 {
		t.Fatalf("credits = %+v", snap.Credits)
	}
	// A weekly-only response invents no session window and no credit row.
	snap, _, err = normalizeOllamaBalance([]byte(`{"included":{"weekly":{"remaining_percent":58}}}`))
	if err != nil || len(snap.Windows) != 1 || snap.Windows[0].Key != "weekly" || snap.Windows[0].PercentUsed != 42 || snap.Credits != nil {
		t.Fatalf("weekly-only = %+v err=%v", snap, err)
	}
}

func TestOllamaAPIKeyBalanceMalformedResponsesFailSafely(t *testing.T) {
	for _, body := range []string{
		`{"included":{"session":{"remaining_percent":-1,"resets_at":"2026-11-06T18:00:00Z"}}}`,
		`{"included":{"session":{"remaining_percent":101,"resets_at":"2026-11-06T18:00:00Z"}}}`,
		`{"included":{"session":{"remaining_percent":"NaN","resets_at":"2026-11-06T18:00:00Z"}}}`,
		`{"included":{"session":{"remaining_percent":null,"resets_at":"2026-11-06T18:00:00Z"}}}`,
		`{"included":{"session":{"remaining_percent":true,"resets_at":"2026-11-06T18:00:00Z"}}}`,
		`{"included":{"session":{"remaining_percent":50,"resets_at":"2026-02-31T00:00:00Z"}}}`,
		`{"included":{"session":[]}}`,
		`{"included":{"allowance_usd":5}}`,
		`{"included":{"allowance_usd":-5,"balance_usd":1}}`,
		`{"included":{"allowance_usd":5,"balance_usd":1,"period":{"until":"not-a-date"}}}`,
		`{"included":{"allowance_usd":5,"balance_usd":1,"period":[]}}`,
		`{"purchased":{}}`,
		`{}`,
		`not json`,
	} {
		snap, _, err := normalizeOllamaBalance([]byte(body))
		var wrapped *SnapshotError
		if !errors.As(fetchOllamaNormalizeAsError(snap, err), &wrapped) || wrapped.Snapshot.Status != StatusFetchError {
			t.Errorf("%s: expected a parse failure, got %+v %v", body, snap, err)
		}
	}
}

// fetchOllamaNormalizeAsError mirrors how the fetcher reports a normalize failure.
func fetchOllamaNormalizeAsError(snap Snapshot, err error) error {
	if err == nil {
		return nil
	}
	return &SnapshotError{Snapshot: ollamaStatus(StatusFetchError, err.Error(), nil), Err: err}
}

func TestOllamaAPIKeyBalanceRejectsARevokedKey(t *testing.T) {
	old := ollamaHTTPClient
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"invalid"}`)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { ollamaHTTPClient = old })
	snap, err := fetchOllamaWithAPIKey(context.Background(), "key")
	if err != nil || snap.Status != StatusAuthMissing {
		t.Fatalf("401 = %+v %v", snap, err)
	}
}

// ── Ollama cookie mode: Free usage meter and text-node label matching ───────

func TestOllamaFreeUsageMeterMapsToTheMonthlyWindow(t *testing.T) {
	// Captured free-plan shape: the included-credit meter is labelled "Free usage", and an
	// info box mentions it in prose before the meter.
	html := `<span>Cloud Usage</span><span>Free</span>` +
		`<p>Free usage credits can be used with the following cloud models:</p>` +
		`<div>Free usage<span>69.5% used</span><time data-time="2026-10-26T15:19:11Z"></time></div>`
	snap, detail, err := parseOllamaSettingsHTML([]byte(html))
	if err != nil || detail != nil {
		t.Fatalf("free plan %+v detail=%+v err=%v", snap, detail, err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Key != "monthly" || snap.Windows[0].PercentUsed != 69.5 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if snap.Windows[0].ResetAt == nil || !snap.Windows[0].ResetAt.Equal(time.Date(2026, 10, 26, 15, 19, 11, 0, time.UTC)) {
		t.Fatalf("reset = %+v", snap.Windows[0].ResetAt)
	}
}

func TestOllamaProseContainingALabelDoesNotTruncateThePreviousBlock(t *testing.T) {
	// The prose sentence sits between the weekly and free meters; as a substring it used to
	// act as a spurious boundary and cut the weekly reset timestamp off.
	html := `<div>Weekly usage<span>12.5% used</span><time data-time="2026-10-20T22:00:00Z"></time></div>` +
		`<p>Free usage credits can be used with the following cloud models:</p>` +
		`<div>Free usage<span>30% used</span><time data-time="2026-10-26T15:19:11Z"></time></div>`
	snap, _, err := parseOllamaSettingsHTML([]byte(html))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].Key != "weekly" || snap.Windows[0].ResetAt == nil {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if snap.Windows[1].Key != "monthly" || snap.Windows[1].PercentUsed != 30 || snap.Windows[1].ResetAt == nil {
		t.Fatalf("free window = %+v", snap.Windows[1])
	}
}

// ── Ollama credit wallet ────────────────────────────────────────────────────

func TestOllamaCreditWalletShowsCreditsAndNeverAPercent(t *testing.T) {
	html := `<h2>Usage credits <span>Pro</span></h2>` +
		`<div><span>$1,234.56</span></div>` +
		`<div><span>Monthly credits used</span><span>$7.50</span></div>` +
		`<p>Refills to $60.00 in 12 days.</p>` +
		`</section><h2>Something else</h2>`
	snap, detail, err := parseOllamaSettingsHTML([]byte(html))
	if err != nil || detail != nil {
		t.Fatalf("wallet %+v detail=%+v err=%v", snap, detail, err)
	}
	if snap.Plan != "Pro" || len(snap.Windows) != 0 {
		t.Fatalf("plan=%q windows=%+v", snap.Plan, snap.Windows)
	}
	if snap.Credits == nil || snap.Credits.Label != "Credit balance" || snap.Credits.Balance != 1234.56 {
		t.Fatalf("credits = %+v", snap.Credits)
	}
	// A wallet page that reports no balance still counts as usage data, and never invents a percentage.
	snap, _, err = parseOllamaSettingsHTML([]byte(`<h2>Usage credits</h2><div><span>Monthly credits used</span><span>$7.50</span></div></section>`))
	if err != nil || len(snap.Windows) != 0 || snap.Credits != nil {
		t.Fatalf("spend-only wallet = %+v err=%v", snap, err)
	}
}

func TestOllamaWalletDetailsCoexistWithQuotaMeters(t *testing.T) {
	html := `<h2>Usage credits <span>Pro</span></h2><div><span>$12.00</span></div></section>` +
		`<div>Session usage <span>42% used</span><time data-time="2026-10-15T22:00:00Z"></time></div>`
	snap, _, err := parseOllamaSettingsHTML([]byte(html))
	if err != nil || len(snap.Windows) != 1 || snap.Credits == nil || snap.Credits.Balance != 12 {
		t.Fatalf("wallet beside meters = %+v %+v err=%v", snap.Windows, snap.Credits, err)
	}
}

// ── Copilot subscription_ended ──────────────────────────────────────────────

func TestCopilotSubscriptionEndedReportsAClearErrorWithoutStaleData(t *testing.T) {
	old := usageHTTPClient
	usageHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"login":"fixture-user","access_type_sku":"subscription_ended","copilot_plan":"individual","chat_enabled":false,"cli_enabled":false,"can_signup_for_limited":true}`), nil
	})}
	t.Cleanup(func() { usageHTTPClient = old })
	store := NewStore(t.TempDir())
	if _, err := store.UpdateConfig(func(cfg *Config) error {
		cfg.Providers[ProviderGitHubCopilot] = ProviderConfig{Enabled: true, Token: "token"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := NewCopilotProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch err=%v", err)
	}
	if snap.Status != StatusAuthMissing || snap.Windows != nil || snap.Credits != nil {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !strings.Contains(snap.Error, "subscription has ended") {
		t.Fatalf("error = %q", snap.Error)
	}
	if snap.ErrorDetail == nil || snap.ErrorDetail.Fields["usage"] != "subscription_ended" {
		t.Fatalf("detail = %+v", snap.ErrorDetail)
	}
}

// ── Mistral price key ───────────────────────────────────────────────────────

func TestMistralPriceKeyMatchesEveryDimensionWithALegacyFallback(t *testing.T) {
	prices := mistralPriceIndex([]mistralPrice{
		{BillingMetric: "tokens", BillingGroup: "completion", Price: "2"},
		{BillingMetric: "audio.seconds", BillingGroup: "audio", EventType: "realtime", APIZone: "eu", ServiceTier: "priority", Price: "1000"},
	})
	model := mistralModelUsageData{
		Input: []mistralUsageEntry{
			{BillingMetric: "tokens", BillingGroup: "completion", Value: 10},
			{BillingMetric: "audio.seconds", BillingGroup: "audio", EventType: "realtime", APIZone: "eu", ServiceTier: "priority", Value: 2},
			{BillingMetric: "audio.seconds", BillingGroup: "audio", EventType: "realtime", APIZone: "us", ServiceTier: "priority", Value: 1},
			{BillingMetric: "audio.seconds", BillingGroup: "audio", EventType: "realtime", Value: 3},
		},
	}
	_, _, _, cost := aggregateMistralModel(model, prices)
	// 10 tokens at the unqualified price, 2 audio seconds at the exact zone+tier price; the
	// unmatched zone and the tier-less entry price at zero rather than at another tier's rate.
	if cost != 10*2+2*1000 {
		t.Fatalf("cost = %v", cost)
	}
}

func TestMistralSpendUsesTheDimensionedPriceKey(t *testing.T) {
	raw := mistralBillingResponse{
		Completion: &mistralModelUsageCategory{Models: map[string]mistralModelUsageData{
			"model": {Input: []mistralUsageEntry{{BillingMetric: "tokens", BillingGroup: "completion", APIZone: "eu", ServiceTier: "priority", Value: 5}}},
		}},
		Currency: "EUR",
		Prices: []mistralPrice{
			{BillingMetric: "tokens", BillingGroup: "completion", Price: "1"},
			{BillingMetric: "tokens", BillingGroup: "completion", APIZone: "eu", ServiceTier: "priority", Price: "4"},
		},
	}
	credits, ok := normalizeMistralSpend(raw)
	if !ok || credits == nil || credits.Balance != 5*4 || credits.Label != "Monthly spend (EUR)" {
		t.Fatalf("credits = %+v ok=%v", credits, ok)
	}
}

// ── Mistral login redirect ──────────────────────────────────────────────────

func TestMistralLoginRedirectReportsAuthMissing(t *testing.T) {
	cookies, _, err := parseMistralCookieHeader("csrftoken=abcdef1234; ory_session_test=secret")
	if err != nil {
		t.Fatal(err)
	}
	loginURL := &url.URL{Scheme: "https", Host: "auth.mistral.ai", Path: "/self-service/login/browser"}
	cases := []struct {
		name     string
		response *http.Response
		want     Status
	}{
		{"followed redirect lands on the login page", func() *http.Response {
			resp := response(`{"result":{"data":{"json":{"usage_percentage":10}}}}`)
			resp.Request = &http.Request{URL: loginURL}
			return resp
		}(), StatusAuthMissing},
		{"an unfollowed 302 carries the login location", func() *http.Response {
			resp := &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://auth.mistral.ai/self-service/login?return_to=%2F"}}, Body: io.NopCloser(strings.NewReader(""))}
			return resp
		}(), StatusAuthMissing},
		{"an unrelated redirect stays a fetch error", func() *http.Response {
			resp := &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://console.mistral.ai/"}}, Body: io.NopCloser(strings.NewReader(""))}
			return resp
		}(), StatusFetchError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := mistralHTTPClient
			mistralHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return tc.response, nil }), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			t.Cleanup(func() { mistralHTTPClient = old })
			_, detail, status, err := fetchMistralVibeUsage(context.Background(), cookies)
			if status != tc.want {
				t.Fatalf("status = %s (err=%v detail=%+v)", status, err, detail)
			}
		})
	}
}

func TestAlertTogglesPersistOnAndOff(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	if _, err := store.UpdateConfig(func(cfg *Config) error {
		cfg.AlertPaceEnabled, cfg.AlertResetNoticeEnabled, cfg.AlertExpiryEnabled = true, true, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"alert_pace_enabled": true`, `"alert_reset_notice_enabled": true`, `"alert_expiry_enabled": true`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("an on toggle must be stored: %s in %s", key, raw)
		}
	}
	// Off again: the saved true must not survive the merge.
	if _, err := store.UpdateConfig(func(cfg *Config) error {
		cfg.AlertPaceEnabled, cfg.AlertResetNoticeEnabled, cfg.AlertExpiryEnabled = false, false, false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AlertPaceEnabled || cfg.AlertResetNoticeEnabled || cfg.AlertExpiryEnabled {
		t.Fatalf("an off toggle must not stay on after a reload: %+v", cfg)
	}
	for _, key := range []string{`"alert_pace_enabled": false`, `"alert_reset_notice_enabled": false`, `"alert_expiry_enabled": false`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("an off toggle must be stored: %s in %s", key, raw)
		}
	}
}
