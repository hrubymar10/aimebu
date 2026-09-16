package usages

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-json"
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
	if !strings.Contains(body, `"profiles":[{"profile_name":"local","active":true`) {
		t.Fatalf("GET body missing implicit local profile: %s", body)
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
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"warmup_schedule":"0 8 * * *\n30 12 * * *"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"warmup_schedule":"0 8 * * *\n30 12 * * *"`) {
		t.Fatalf("warmup schedule response = %d %s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"warmup_schedule":"*/5 * * * *"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid warmup schedule status = %d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"warmup_mode":"smart"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"warmup_mode":"smart"`) {
		t.Fatalf("warmup mode response = %d %s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"warmup_mode":"sometimes"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid warmup mode status = %d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"claude_model_flag":"--model claude-sonnet-4-6"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"claude_model_flag":"--model claude-sonnet-4-6"`) {
		t.Fatalf("Claude model flag response = %d %s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"claude_model_flag":"--model a b"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid Claude model flag status = %d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"codex_model_flag":"--model gpt-5.6-luna"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"codex_model_flag":"--model gpt-5.6-luna"`) {
		t.Fatalf("Codex model flag response = %d %s", resp.Code, resp.Body.String())
	}
	resp = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/usages/settings", strings.NewReader(`{"codex_model_flag":"--model bad value"}`))
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid Codex model flag status = %d", resp.Code)
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

func TestHTTPUsageShapeDoesNotDependOnSwitcherEnabled(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	m.SetProfileLister(func(tool string) []ProfileInfo {
		if tool == "codex" {
			return []ProfileInfo{
				{Tool: "codex", Name: "main", Active: true, HasCredentials: true},
				{Tool: "codex", Name: "backup", HasCredentials: true},
			}
		}
		return []ProfileInfo{}
	})
	enabled := false
	m.SetSwitcherSettingsProvider(func() bool { return enabled })
	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)

	read := func() UsagesResponse {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/usages", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET status = %d body=%s", recorder.Code, recorder.Body.String())
		}
		var response UsagesResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	off := read()
	enabled = true
	on := read()
	if off.SwitcherEnabled || !on.SwitcherEnabled {
		t.Fatalf("switcher flags off=%v on=%v", off.SwitcherEnabled, on.SwitcherEnabled)
	}
	off.SwitcherEnabled = false
	on.SwitcherEnabled = false
	if !reflect.DeepEqual(off, on) {
		t.Fatalf("usage response changed with display flag:\noff=%+v\non=%+v", off, on)
	}
	for _, provider := range on.Providers {
		if len(provider.Profiles) == 0 {
			t.Fatalf("provider %s has no profiles", provider.ProviderName)
		}
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
