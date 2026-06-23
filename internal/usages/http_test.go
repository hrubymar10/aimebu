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
	if !strings.Contains(resp.Body.String(), `"snapshots":{}`) {
		t.Fatalf("GET body = %s", resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `{"key":"codex","label":"Codex","enabled":false,"available":true}`) {
		t.Fatalf("GET providers missing codex availability: %s", resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `{"key":"claude-code","label":"Claude Code","enabled":false,"available":true}`) {
		t.Fatalf("GET providers missing claude-code availability: %s", resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `{"key":"github-copilot","label":"GitHub Copilot","enabled":false,"available":true}`) {
		t.Fatalf("GET providers missing github-copilot availability: %s", resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `{"key":"mistral","label":"Mistral","enabled":false,"available":true}`) {
		t.Fatalf("GET providers missing mistral availability: %s", resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `{"key":"ollama-cloud","label":"Ollama Cloud","enabled":false,"available":true}`) {
		t.Fatalf("GET providers missing ollama-cloud availability: %s", resp.Body.String())
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := string(body)
	ollama := strings.Index(got, `"key":"ollama-cloud"`)
	codex := strings.Index(got, `"key":"codex"`)
	claude := strings.Index(got, `"key":"claude-code"`)
	copilot := strings.Index(got, `"key":"github-copilot"`)
	mistral := strings.Index(got, `"key":"mistral"`)
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
	body := resp.Body.String()
	if strings.Contains(body, "csrf-secret") || strings.Contains(body, "session-secret") || strings.Contains(body, "hidden") {
		t.Fatalf("mistral config leaked secret: %s", body)
	}
	if !strings.Contains(body, `"mistral":{"enabled":true}`) {
		t.Fatalf("mistral config body = %s", body)
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
