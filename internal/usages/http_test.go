package usages

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPEmptyShapeAndSettingsValidation(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), DefaultRegistry())
	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/usages", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status = %d", resp.Code)
	}
	body := resp.Body.String()
	if !strings.Contains(body, `"providers":[`) {
		t.Fatalf("GET body missing providers array: %s", body)
	}
	if !strings.Contains(body, `"provider_name":"codex"`) {
		t.Fatalf("GET body missing codex provider: %s", body)
	}
	if !strings.Contains(body, `"available":true`) {
		t.Fatalf("GET body missing available provider: %s", body)
	}
	if !strings.Contains(body, `"profiles":[]`) {
		t.Fatalf("GET body missing empty profiles: %s", body)
	}
	if !strings.Contains(body, `"switcher_enabled":false`) {
		t.Fatalf("GET body missing switcher_enabled: %s", body)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"refresh_interval_sec":1}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("settings status = %d", resp.Code)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"percent_display":"used"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("percent display status = %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"percent_display":"used"`) {
		t.Fatalf("percent display body = %s", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"provider_order":["ollama-cloud","codex"]}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("provider order status = %d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/usages", nil)
	mux.ServeHTTP(resp, req)
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := string(bodyBytes)
	ollama := strings.Index(got, `"provider_name":"ollama-cloud"`)
	codex := strings.Index(got, `"provider_name":"codex"`)
	claude := strings.Index(got, `"provider_name":"claude-code"`)
	copilot := strings.Index(got, `"provider_name":"github-copilot"`)
	mistral := strings.Index(got, `"provider_name":"mistral"`)
	if !(ollama >= 0 && codex > ollama && claude > codex && copilot > claude && mistral > copilot) {
		t.Fatalf("providers not in configured order: %s", got)
	}
}

func TestHTTPMistralConfigRedactsSecret(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), DefaultRegistry())
	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/usages/mistral/config", strings.NewReader(`{"cookie":"Cookie: csrftoken=csrf-secret; ory_session_abc=session-secret; admin_secret=hidden"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("mistral config status = %d body=%s", resp.Code, resp.Body.String())
	}
	respBody := resp.Body.String()
	if strings.Contains(respBody, "csrf-secret") || strings.Contains(respBody, "session-secret") || strings.Contains(respBody, "hidden") {
		t.Fatalf("mistral config leaked secret: %s", respBody)
	}
	if !strings.Contains(respBody, `"mistral":{"enabled":true}`) {
		t.Fatalf("mistral config body = %s", respBody)
	}
}

func TestHTTPProviderToggleAndForceCooldown(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/usages/providers", strings.NewReader(`{"provider":"codex","enabled":true}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("provider toggle status = %d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/providers", strings.NewReader(`{"provider":"bogus","enabled":true}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider status = %d", resp.Code)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/refresh", strings.NewReader(`{"provider":"codex"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("first refresh status = %d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/refresh", strings.NewReader(`{"provider":"codex"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("cooldown status = %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "retry_after_sec") {
		t.Fatalf("cooldown body = %s", resp.Body.String())
	}
}

func TestHTTPCopilotLoginStartRejectsInvalidHost(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)
	resp := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/usages/copilot/login/start", strings.NewReader(`{"enterprise_host":"http://github.example.com"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "https") {
		t.Fatalf("body = %s", resp.Body.String())
	}
}
