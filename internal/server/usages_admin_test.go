package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/usages"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

// The owner reads the whole snapshot of the usages, an admin the limits without anything that names an account or how it signs in, and nobody else
// reads it. Every write stays the owner's: an admin is refused each of them, whatever it says.
func TestUsagesAreReadOnlyForAnAdminAndHoldNoSecrets(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	var err error
	w.f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := w.f.srv.Usages.ExtraStore()
	store.Client = &http.Client{Transport: usageFlowTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"email":"owner-login@example.com"}`
		if strings.HasSuffix(req.URL.Path, "/copilot_internal/user") {
			body = `{"copilot_plan":"business","quota_snapshots":{"premium_interactions":{"entitlement":100,"remaining":50}}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	if _, err := store.UpdateConfig(func(cfg *providers.Config) error {
		cfg.Providers[providers.ProviderGitHubCopilot] = providers.ProviderConfig{Enabled: true, Token: "ghu_SECRET_TOKEN", EnterpriseHost: "https://ghe.secret.example.com"}
		cfg.Providers[providers.ProviderMistral] = providers.ProviderConfig{Enabled: true, Cookie: "session=SECRET_COOKIE"}
		cfg.Providers[providers.ProviderOllamaCloud] = providers.ProviderConfig{Enabled: true, APIKey: "SECRET_API_KEY", AuthMode: "api_key"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.f.srv.Usages.RefreshProvider(t.Context(), providers.ProviderGitHubCopilot)
	read := func(who adminActor) (int, string) {
		resp := userRequest(t, w.f, who.cookie, who.csrf, "GET", "/api/usages", nil)
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	secrets := []string{"ghu_SECRET_TOKEN", "SECRET_COOKIE", "SECRET_API_KEY", "ghe.secret.example.com", "owner-login@example.com", "token_configured", "cookie_configured", "api_key_configured", "auth_mode", "enterprise_host", "\"email\"", "credential_alerts", "error_detail"}
	code, whole := read(w.owner)
	if code != 200 || !strings.Contains(whole, "provider_name") {
		t.Fatalf("the owner reads %d %s", code, whole)
	}
	for _, secret := range []string{"ghu_SECRET_TOKEN", "SECRET_COOKIE", "SECRET_API_KEY"} {
		if strings.Contains(whole, secret) {
			t.Errorf("the owner's own snapshot holds the secret %q", secret)
		}
	}
	code, shown := read(w.admin)
	if code != 200 || !strings.Contains(shown, "provider_name") || !strings.Contains(shown, "percent_display") {
		t.Fatalf("an admin reads the limits: %d %s", code, shown)
	}
	for _, secret := range secrets {
		if strings.Contains(shown, secret) {
			t.Errorf("an admin's view holds %q: %s", secret, shown)
		}
	}
	for _, who := range []adminActor{w.pOwner, w.oper, w.viewer, w.out} {
		if code, _ := read(who); code != 403 {
			t.Errorf("%s reads the usages: %d", who.name, code)
		}
	}
	for _, write := range []struct{ method, path string }{
		{"POST", "/api/usages/refresh"}, {"POST", "/api/usages/providers"}, {"POST", "/api/usages/settings"}, {"POST", "/api/usages/mistral/config"}, {"POST", "/api/usages/ollama/config"},
		{"POST", "/api/usages/copilot/login/start"}, {"POST", "/api/usages/copilot/login/poll"}, {"POST", "/api/usages/copilot/login/logout"},
		{"GET", "/api/switcher"}, {"PUT", "/api/switcher"}, {"GET", "/api/quota-switcher"}, {"GET", "/api/limit-guard"}, {"PUT", "/api/limit-guard/claude"}, {"PUT", "/api/quota-switcher/claude"},
	} {
		if code := w.call(t, w.admin, write.method, write.path, map[string]any{}); code != 403 {
			t.Errorf("an admin %s %s: %d, want 403", write.method, write.path, code)
		}
	}
	config, err := store.LoadConfig()
	if err != nil || config.Providers[providers.ProviderMistral].Cookie != "session=SECRET_COOKIE" {
		t.Fatalf("a refused write changed the configuration: %+v %v", config.Providers[providers.ProviderMistral], err)
	}
}
