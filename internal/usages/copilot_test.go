package usages

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestCopilotEnterpriseHostNormalization(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", copilotDefaultAPIHost, false},
		{"https://github.example.com", "https://api.github.example.com", false},
		{"https://api.github.example.com/", "https://api.github.example.com", false},
		{"https://GITHUB.EXAMPLE.COM.", "https://api.github.example.com", false},
		{"http://github.example.com", "", true},
		{"github.example.com", "", true},
		{"https://github.example.com/path", "", true},
		{"https://github.example.com?x=1", "", true},
		{"https://user@github.example.com", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizeCopilotEnterpriseHost(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalize = %q, want error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("normalize = %q err=%v, want %q", got, err, tc.want)
			}
			usageURL, err := copilotUsageURL(got)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(usageURL)
			if err != nil {
				t.Fatal(err)
			}
			if u.Scheme != "https" || u.Path != copilotUsagePath || u.RawQuery != "" || u.Fragment != "" {
				t.Fatalf("usage URL = %s", usageURL)
			}
		})
	}
}

func TestCopilotProviderFetchesAndNormalizesUsage(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderGitHubCopilot] = ProviderConfig{Enabled: true, Token: "access-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != copilotDefaultAPIHost+copilotUsagePath {
			t.Fatalf("URL = %s", req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "token access-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("Editor-Version"); got != copilotEditorVersion {
			t.Fatalf("Editor-Version = %q", got)
		}
		if got := req.Header.Get("Editor-Plugin-Version"); got != copilotPluginVersion {
			t.Fatalf("Editor-Plugin-Version = %q", got)
		}
		if got := req.Header.Get("User-Agent"); got != copilotUserAgent {
			t.Fatalf("User-Agent = %q", got)
		}
		if got := req.Header.Get("X-Github-Api-Version"); got != copilotGitHubAPIVersion {
			t.Fatalf("X-Github-Api-Version = %q", got)
		}
		return httpJSON(200, `{
  "copilot_plan": "business",
  "quota_reset_date": "2030-01-01T00:00:00Z",
  "quota_snapshots": {
    "premium_interactions": {"entitlement": 100, "remaining": 73, "percent_remaining": 73, "quota_id": "premium"},
    "chat": {"entitlement": 200, "remaining": 100, "percent_remaining": 50, "quota_id": "chat"}
  }
}`), nil
	})
	snap, err := NewCopilotProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != StatusOK || snap.Plan != "business" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].Key != "premium" || snap.Windows[1].Key != "chat" {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if snap.Windows[0].PercentUsed != 27 || snap.Windows[1].PercentUsed != 50 {
		t.Fatalf("windows = %+v, want percent inversion", snap.Windows)
	}
}

func TestCopilotUsageRequestTimeout(t *testing.T) {
	withTimeoutHTTPTransport(t, timeoutRoundTrip)
	_, _, status, err := fetchCopilotUsage(context.Background(), "access-secret", copilotDefaultAPIHost)
	if err == nil {
		t.Fatal("fetchCopilotUsage succeeded")
	}
	if status != StatusTimeout {
		t.Fatalf("status = %s, want %s", status, StatusTimeout)
	}
}

func TestCopilotQuotaFallbackShapes(t *testing.T) {
	var raw copilotUsageRaw
	if err := json.Unmarshal([]byte(`{
  "copilot_plan": "individual",
  "monthly_quotas": {"completions": "100", "chat": 50},
  "limited_user_quotas": {"completions": 25, "chat": "10"}
}`), &raw); err != nil {
		t.Fatal(err)
	}
	snap, _, err := normalizeCopilotUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].PercentUsed != 75 || snap.Windows[1].PercentUsed != 80 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

func TestCopilotUnlimitedQuotaShape(t *testing.T) {
	var raw copilotUsageRaw
	if err := json.Unmarshal([]byte(`{
  "copilot_plan": "business",
  "quota_snapshots": {
    "chat": {"unlimited": true}
  }
}`), &raw); err != nil {
		t.Fatal(err)
	}
	snap, _, err := normalizeCopilotUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != StatusOK || snap.Plan != "business" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 0 {
		t.Fatalf("windows = %+v, want none for unlimited quota", snap.Windows)
	}
}

func TestCopilotSkipsTokenBillingPlaceholderQuota(t *testing.T) {
	var raw copilotUsageRaw
	if err := json.Unmarshal([]byte(`{
  "copilot_plan": "business",
  "quota_snapshots": {
    "premium_interactions": {"entitlement": 0, "remaining": 0, "percent_remaining": 0},
    "chat": {"entitlement": 120, "remaining": 90, "percent_remaining": 75, "quota_id": "chat"}
  }
}`), &raw); err != nil {
		t.Fatal(err)
	}
	snap, _, err := normalizeCopilotUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Key != "chat" || snap.Windows[0].PercentUsed != 25 {
		t.Fatalf("windows = %+v, want only usable chat quota", snap.Windows)
	}
}

func TestCopilotProviderStatusMappingAndRedaction(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderGitHubCopilot] = ProviderConfig{Enabled: true, Token: "access-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		return httpJSON(http.StatusForbidden, `{"message":"raw-live-value"}`), nil
	})
	snap, err := NewCopilotProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != StatusScopeMissing {
		t.Fatalf("status = %+v", snap)
	}
	assertNotContains(t, jsonString(t, snap), "access-secret", "raw-live-value")
}

func TestCopilotDeviceFlowStatesAndPersistence(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	m := NewManager(store, DefaultRegistry())
	flows := newCopilotFlowStore()
	now := time.Date(2026, 5, 16, 21, 0, 0, 0, time.UTC)
	flows.now = func() time.Time { return now }
	mux := http.NewServeMux()
	Routes{Manager: m, Copilot: flows}.Mount(mux)

	pollCalls := 0
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case copilotDeviceCodeURL:
			data, _ := io.ReadAll(req.Body)
			form, _ := url.ParseQuery(string(data))
			if form.Get("client_id") != copilotClientID || form.Get("scope") != copilotScope {
				t.Fatalf("start form = %s", data)
			}
			return httpJSON(200, `{"device_code":"device-secret","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","verification_uri_complete":"https://github.com/login/device?user_code=ABCD-1234","expires_in":900,"interval":5}`), nil
		case copilotAccessTokenURL:
			pollCalls++
			data, _ := io.ReadAll(req.Body)
			form, _ := url.ParseQuery(string(data))
			if form.Get("device_code") != "device-secret" || form.Get("grant_type") != copilotGrantType {
				t.Fatalf("poll form = %s", data)
			}
			switch pollCalls {
			case 1:
				return httpJSON(200, `{"error":"authorization_pending"}`), nil
			case 2:
				return httpJSON(200, `{"error":"slow_down"}`), nil
			default:
				return httpJSON(200, `{"access_token":"access-secret","token_type":"bearer","scope":"read:user"}`), nil
			}
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})

	start := httptest.NewRecorder()
	mux.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/usages/copilot/login/start", strings.NewReader(`{"enterprise_host":"https://github.example.com"}`)))
	if start.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", start.Code, start.Body.String())
	}
	assertNotContains(t, start.Body.String(), "device-secret", "access-secret")
	var startResp copilotStartResponse
	if err := json.Unmarshal(start.Body.Bytes(), &startResp); err != nil {
		t.Fatal(err)
	}

	pending := pollCopilotTest(t, mux, startResp.FlowID)
	if pending.Status != "pending" {
		t.Fatalf("pending = %+v", pending)
	}
	cached := pollCopilotTest(t, mux, startResp.FlowID)
	if cached.Status != "pending" || pollCalls != 1 {
		t.Fatalf("cached = %+v calls=%d", cached, pollCalls)
	}
	now = now.Add(5 * time.Second)
	slow := pollCopilotTest(t, mux, startResp.FlowID)
	if slow.Status != "slow_down" || slow.Interval != 10 {
		t.Fatalf("slow = %+v", slow)
	}
	now = now.Add(10 * time.Second)
	success := pollCopilotTest(t, mux, startResp.FlowID)
	if success.Status != "success" {
		t.Fatalf("success = %+v", success)
	}

	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.Providers[ProviderGitHubCopilot]
	if pc.Token != "access-secret" || !pc.Enabled || pc.EnterpriseHost != "https://api.github.example.com" {
		t.Fatalf("config provider = %+v", pc)
	}
	assertMode(t, store.ConfigPath(), 0o600)
	assertNotContains(t, jsonString(t, success), "access-secret", "device-secret")
}

func TestCopilotDeviceFlowTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"expired", `{"error":"expired_token"}`, "expired"},
		{"denied", `{"error":"access_denied"}`, "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStoreAt(t.TempDir())
			flows := newCopilotFlowStore()
			mux := http.NewServeMux()
			Routes{Manager: NewManager(store, DefaultRegistry()), Copilot: flows}.Mount(mux)
			withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.String() == copilotDeviceCodeURL {
					return httpJSON(200, `{"device_code":"device-secret","user_code":"ABCD","verification_uri":"https://github.com/login/device","expires_in":900,"interval":1}`), nil
				}
				return httpJSON(200, tc.body), nil
			})
			start := httptest.NewRecorder()
			mux.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/usages/copilot/login/start", strings.NewReader(`{}`)))
			var startResp copilotStartResponse
			if err := json.Unmarshal(start.Body.Bytes(), &startResp); err != nil {
				t.Fatal(err)
			}
			flows.flows[startResp.FlowID].LastPollAt = time.Time{}
			resp := pollCopilotTest(t, mux, startResp.FlowID)
			if resp.Status != tc.want {
				t.Fatalf("status = %+v, want %s", resp, tc.want)
			}
		})
	}
}

func TestCopilotTokenRedactionThroughManagerCacheAndHTTP(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderGitHubCopilot] = ProviderConfig{Enabled: true, Token: "access-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		return httpJSON(http.StatusInternalServerError, `{"message":"raw-live-value"}`), nil
	})
	m := NewManager(store, DefaultRegistry())
	resp, err := m.Snapshot(context.Background(), ProviderGitHubCopilot)
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, jsonString(t, resp), "access-secret", "raw-live-value")
	cacheData, err := os.ReadFile(store.CachePath())
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, string(cacheData), "access-secret", "raw-live-value")

	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/usages/providers", strings.NewReader(`{"provider":"github-copilot","enabled":true}`)))
	assertNotContains(t, rr.Body.String(), "access-secret")
}

func TestCopilotLogoutClearsTokenAndDisablesProvider(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderGitHubCopilot] = ProviderConfig{Enabled: true, Token: "access-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Routes{Manager: NewManager(store, DefaultRegistry())}.Mount(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/usages/copilot/login/logout", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("logout status = %d body=%s", rr.Code, rr.Body.String())
	}
	assertNotContains(t, rr.Body.String(), "access-secret")
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.Providers[ProviderGitHubCopilot]
	if pc.Token != "" || pc.Enabled {
		t.Fatalf("provider = %+v", pc)
	}
}

func pollCopilotTest(t *testing.T, mux *http.ServeMux, flowID string) copilotPollResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/usages/copilot/login/poll", strings.NewReader(`{"flow_id":"`+flowID+`"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("poll status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp copilotPollResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCopilotConcurrentPollsDedup(t *testing.T) {
	flows := newCopilotFlowStore()
	now := time.Date(2026, 5, 16, 21, 0, 0, 0, time.UTC)
	flows.now = func() time.Time { return now }
	flows.flows["flow"] = &copilotFlowState{
		FlowID:     "flow",
		DeviceCode: "device-secret",
		Interval:   5,
		ExpiresAt:  now.Add(time.Hour),
	}
	calls := 0
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return httpJSON(200, `{"error":"authorization_pending"}`), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, _, err := flows.poll(context.Background(), "flow"); err != nil || resp.Status != "pending" {
				t.Errorf("poll = %+v err=%v", resp, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestCopilotDifferentFlowPollsDoNotBlockEachOther(t *testing.T) {
	flows := newCopilotFlowStore()
	now := time.Date(2026, 5, 16, 21, 0, 0, 0, time.UTC)
	flows.now = func() time.Time { return now }
	for _, flowID := range []string{"flow-a", "flow-b"} {
		flows.flows[flowID] = &copilotFlowState{
			FlowID:     flowID,
			DeviceCode: "device-" + flowID,
			Interval:   5,
			ExpiresAt:  now.Add(time.Hour),
		}
	}

	var mu sync.Mutex
	calls := 0
	overlap := make(chan struct{})
	sawOverlap := false
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		call := calls
		if calls == 2 {
			close(overlap)
		}
		mu.Unlock()

		if call == 1 {
			select {
			case <-overlap:
				sawOverlap = true
			case <-time.After(250 * time.Millisecond):
			}
		}
		return httpJSON(200, `{"error":"authorization_pending"}`), nil
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, flowID := range []string{"flow-a", "flow-b"} {
		wg.Add(1)
		go func(flowID string) {
			defer wg.Done()
			<-start
			if resp, _, err := flows.poll(context.Background(), flowID); err != nil || resp.Status != "pending" {
				t.Errorf("poll %s = %+v err=%v", flowID, resp, err)
			}
		}(flowID)
	}
	close(start)
	wg.Wait()

	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
	if !sawOverlap {
		t.Fatalf("different flow polls did not overlap")
	}
}
