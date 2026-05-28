package usages

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withHTTPTransport(t *testing.T, fn roundTripFunc) {
	t.Helper()
	old := usageHTTPClient
	usageHTTPClient = &http.Client{Timeout: usageHTTPTimeout, Transport: fn}
	t.Cleanup(func() { usageHTTPClient = old })
}

func withTimeoutHTTPTransport(t *testing.T, fn roundTripFunc) {
	t.Helper()
	old := usageHTTPClient
	usageHTTPClient = &http.Client{Timeout: time.Millisecond, Transport: fn}
	t.Cleanup(func() { usageHTTPClient = old })
}

func timeoutRoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func writeCodexAuthFixture(t *testing.T, root string, body string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func httpJSON(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestCodexProviderFetchesAndNormalizesUsage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeCodexAuthFixture(t, root, `{
  "tokens": {
    "access_token": "access-secret",
    "refresh_token": "refresh-secret",
    "account_id": "acct-secret"
  },
  "last_refresh": "`+time.Now().UTC().Format(time.RFC3339Nano)+`"
}`)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != codexUsageURL {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer access-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("ChatGPT-Account-Id"); got != "acct-secret" {
			t.Fatalf("ChatGPT-Account-Id = %q", got)
		}
		return httpJSON(200, `{
  "plan_type": "pro",
  "rate_limit": {
    "primary_window": {"used_percent": 47.5, "reset_at": 1893456000, "limit_window_seconds": 604800},
    "secondary_window": {"used_percent": 12, "reset_at": 1892851200, "limit_window_seconds": 18000}
  },
  "credits": {"balance": "123.45"}
}`), nil
	})
	snap, err := NewCodexProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if snap.Status != StatusOK || snap.Plan != "pro" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].Key != "session" || snap.Windows[1].Key != "weekly" {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if snap.Windows[1].PercentUsed != 47.5 {
		t.Fatalf("weekly percent = %v, want 47.5", snap.Windows[1].PercentUsed)
	}
	if snap.Credits == nil || snap.Credits.Balance != 123.45 {
		t.Fatalf("credits = %+v", snap.Credits)
	}
}

func TestNormalizeCodexUsageIncludesSparkAdditionalWindows(t *testing.T) {
	var raw codexUsageRaw
	if err := json.Unmarshal([]byte(`{
  "plan_type": "pro",
  "rate_limit": {
    "primary_window": {"used_percent": 22, "reset_at": 1892851200, "limit_window_seconds": 18000},
    "secondary_window": {"used_percent": 43, "reset_at": 1893456000, "limit_window_seconds": 604800}
  },
  "additional_rate_limits": [
    {
      "limit_name": "GPT-5.3-Codex-Spark",
      "metered_feature": "gpt_5_3_codex_spark",
      "rate_limit": {
        "primary_window": {"used_percent": 30, "reset_at": 1892854800, "limit_window_seconds": 18000},
        "secondary_window": {"used_percent": 100, "reset_at": 1893459600, "limit_window_seconds": 604800}
      }
    }
  ]
}`), &raw); err != nil {
		t.Fatal(err)
	}
	snap, detail, err := normalizeCodexUsage(raw, codexCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if detail != nil {
		t.Fatalf("detail = %+v", detail)
	}
	gotKeys := make([]string, 0, len(snap.Windows))
	for _, w := range snap.Windows {
		gotKeys = append(gotKeys, w.Key)
	}
	wantKeys := []string{"session", "weekly", "codex_spark", "codex_spark_weekly"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("window keys = %v, want %v", gotKeys, wantKeys)
	}
	if snap.Windows[2].PercentUsed != 30 || snap.Windows[3].PercentUsed != 100 {
		t.Fatalf("spark windows = %+v", snap.Windows[2:])
	}
}

func TestNormalizeCodexUsageIgnoresMalformedAdditionalLimits(t *testing.T) {
	var raw codexUsageRaw
	if err := json.Unmarshal([]byte(`{
  "rate_limit": {
    "primary_window": {"used_percent": 22, "reset_at": 1892851200, "limit_window_seconds": 18000}
  },
  "additional_rate_limits": [
    "bad",
    {
      "limit_name": "GPT-5.3-Codex-Spark Weekly",
      "metered_feature": "gpt_5_3_codex_spark",
      "rate_limit": {
        "primary_window": {"used_percent": 80, "reset_at": 1893459600, "limit_window_seconds": 604800}
      }
    },
    42
  ]
}`), &raw); err != nil {
		t.Fatal(err)
	}
	snap, _, err := normalizeCodexUsage(raw, codexCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	gotKeys := make([]string, 0, len(snap.Windows))
	for _, w := range snap.Windows {
		gotKeys = append(gotKeys, w.Key)
	}
	wantKeys := []string{"session", "codex_spark_weekly"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("window keys = %v, want %v", gotKeys, wantKeys)
	}
}

func TestCodexUsageRequestTimeout(t *testing.T) {
	withTimeoutHTTPTransport(t, timeoutRoundTrip)
	_, _, status, err := fetchCodexUsage(context.Background(), codexCredentials{AccessToken: "access-secret"})
	if err == nil {
		t.Fatal("fetchCodexUsage succeeded")
	}
	if status != StatusTimeout {
		t.Fatalf("status = %s, want %s", status, StatusTimeout)
	}
}

func TestNormalizeCodexUsageClassifiesWindowDurationsByRange(t *testing.T) {
	cases := []struct {
		name         string
		primarySec   int64
		secondarySec int64
		wantKeys     []string
		wantDetail   map[string]string
	}{
		{
			name:         "three hour session",
			primarySec:   10800,
			secondarySec: 604800,
			wantKeys:     []string{"session", "weekly"},
		},
		{
			name:         "fourteen day weekly",
			primarySec:   18000,
			secondarySec: 1209600,
			wantKeys:     []string{"session", "weekly"},
		},
		{
			name:         "one minute session",
			primarySec:   60,
			secondarySec: 604800,
			wantKeys:     []string{"session", "weekly"},
		},
		{
			name:         "zero duration dropped",
			primarySec:   0,
			secondarySec: 604800,
			wantKeys:     []string{"weekly"},
			wantDetail:   map[string]string{"rate_limit.primary_window.limit_window_seconds": "number=0"},
		},
		{
			name:         "sixty day duration dropped",
			primarySec:   5184000,
			secondarySec: 18000,
			wantKeys:     []string{"session"},
			wantDetail:   map[string]string{"rate_limit.primary_window.limit_window_seconds": "number=5184000"},
		},
		{
			name:         "swapped arbitrary durations",
			primarySec:   1209600,
			secondarySec: 10800,
			wantKeys:     []string{"session", "weekly"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw codexUsageRaw
			body := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":10,"reset_at":1893456000,"limit_window_seconds":%d},"secondary_window":{"used_percent":20,"reset_at":1892851200,"limit_window_seconds":%d}}}`, tc.primarySec, tc.secondarySec)
			if err := json.Unmarshal([]byte(body), &raw); err != nil {
				t.Fatal(err)
			}
			snap, detail, err := normalizeCodexUsage(raw, codexCredentials{})
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Windows) != len(tc.wantKeys) {
				t.Fatalf("windows = %+v, want keys %v", snap.Windows, tc.wantKeys)
			}
			for i, want := range tc.wantKeys {
				if snap.Windows[i].Key != want {
					t.Fatalf("windows = %+v, want keys %v", snap.Windows, tc.wantKeys)
				}
			}
			for path, want := range tc.wantDetail {
				if detail == nil || detail.Fields[path] != want {
					t.Fatalf("detail = %+v, want %s=%s", detail, path, want)
				}
			}
		})
	}
}

func TestCodexProviderRefreshesStaleAuth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeCodexAuthFixture(t, root, `{
  "tokens": {"access_token": "old-access", "refresh_token": "old-refresh"},
  "last_refresh": "`+time.Now().Add(-9*24*time.Hour).UTC().Format(time.RFC3339)+`"
}`)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case codexRefreshURL:
			data, _ := io.ReadAll(req.Body)
			body := string(data)
			for _, want := range []string{`"client_id":"` + codexClientID + `"`, `"grant_type":"refresh_token"`, `"refresh_token":"old-refresh"`, `"scope":"openid profile email"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("refresh body missing %s: %s", want, body)
				}
			}
			return httpJSON(200, `{"access_token":"new-access","refresh_token":"new-refresh","id_token":"new-id"}`), nil
		case codexUsageURL:
			if got := req.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("Authorization = %q", got)
			}
			return httpJSON(200, `{"rate_limit":{"primary_window":{"used_percent":1,"reset_at":1892851200,"limit_window_seconds":18000}}}`), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})
	snap, err := NewCodexProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil || snap.Status != StatusOK {
		t.Fatalf("Fetch = %+v err=%v", snap, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"new-access", "new-refresh", "last_refresh"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("auth file missing %q: %s", want, data)
		}
	}
	assertMode(t, filepath.Join(root, "auth.json"), 0o600)
}

func TestCodexProviderRefreshesAfterUnauthorizedUsage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeCodexAuthFixture(t, root, `{
  "tokens": {"access_token": "old-access", "refresh_token": "old-refresh"},
  "last_refresh": "`+time.Now().UTC().Format(time.RFC3339)+`"
}`)
	usageCalls := 0
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case codexUsageURL:
			usageCalls++
			if usageCalls == 1 {
				if got := req.Header.Get("Authorization"); got != "Bearer old-access" {
					t.Fatalf("first Authorization = %q", got)
				}
				return httpJSON(http.StatusUnauthorized, `{"error":"expired"}`), nil
			}
			if got := req.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("second Authorization = %q", got)
			}
			return httpJSON(200, `{"rate_limit":{"primary_window":{"used_percent":1,"reset_at":1892851200,"limit_window_seconds":18000}}}`), nil
		case codexRefreshURL:
			data, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(data), `"refresh_token":"old-refresh"`) {
				t.Fatalf("refresh body = %s", data)
			}
			return httpJSON(200, `{"access_token":"new-access","refresh_token":"new-refresh"}`), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})
	snap, err := NewCodexProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil || snap.Status != StatusOK {
		t.Fatalf("Fetch = %+v err=%v", snap, err)
	}
	if usageCalls != 2 {
		t.Fatalf("usage calls = %d, want 2", usageCalls)
	}
	data, err := os.ReadFile(filepath.Join(root, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"new-access", "new-refresh"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("auth file missing %q: %s", want, data)
		}
	}
}

func TestCodexProviderRetriesRewrittenAuthAfterUnauthorizedUsage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeCodexAuthFixture(t, root, `{
  "tokens": {"access_token": "old-access", "refresh_token": "old-refresh"},
  "last_refresh": "`+time.Now().UTC().Format(time.RFC3339)+`"
}`)
	usageCalls := 0
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case codexUsageURL:
			usageCalls++
			if usageCalls == 1 {
				if got := req.Header.Get("Authorization"); got != "Bearer old-access" {
					t.Fatalf("first Authorization = %q", got)
				}
				writeCodexAuthFixture(t, root, `{
  "tokens": {"access_token": "external-access", "refresh_token": "external-refresh"},
  "last_refresh": "`+time.Now().UTC().Format(time.RFC3339)+`"
}`)
				return httpJSON(http.StatusUnauthorized, `{"error":"expired"}`), nil
			}
			if got := req.Header.Get("Authorization"); got != "Bearer external-access" {
				t.Fatalf("second Authorization = %q", got)
			}
			return httpJSON(200, `{"rate_limit":{"primary_window":{"used_percent":1,"reset_at":1892851200,"limit_window_seconds":18000}}}`), nil
		case codexRefreshURL:
			t.Fatal("refresh endpoint should not be called after auth.json was rewritten")
			return nil, nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})
	snap, err := NewCodexProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil || snap.Status != StatusOK {
		t.Fatalf("Fetch = %+v err=%v", snap, err)
	}
	if usageCalls != 2 {
		t.Fatalf("usage calls = %d, want 2", usageCalls)
	}
}

func TestCodexAuthRefreshPreservesUnknownFieldsAndTokenCasing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeCodexAuthFixture(t, root, `{
  "future_field": "preserved-value",
  "tokens": {
    "accessToken": "old-access",
    "refreshToken": "old-refresh",
    "future_token_field": "preserved-token-value"
  },
  "last_refresh": "`+time.Now().Add(-9*24*time.Hour).UTC().Format(time.RFC3339)+`"
}`)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case codexRefreshURL:
			return httpJSON(200, `{"access_token":"new-access","refresh_token":"new-refresh"}`), nil
		case codexUsageURL:
			if got := req.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("Authorization = %q", got)
			}
			return httpJSON(200, `{"rate_limit":{"primary_window":{"used_percent":1,"reset_at":1892851200,"limit_window_seconds":18000}}}`), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	})
	if _, err := NewCodexProvider().Fetch(context.Background(), NewStoreAt(t.TempDir())); err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{`"future_field": "preserved-value"`, `"future_token_field": "preserved-token-value"`, `"accessToken": "new-access"`, `"refreshToken": "new-refresh"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("auth file missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"access_token"`) || strings.Contains(body, `"refresh_token"`) {
		t.Fatalf("auth file unexpectedly added snake_case aliases: %s", body)
	}
}

func TestCodexPlanFromIDTokenClaimShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"literal", `{"https://api.openai.com/auth.chatgpt_plan_type":"pro"}`, "pro"},
		{"nested", `{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`, "plus"},
		{"top", `{"chatgpt_plan_type":"team"}`, "team"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(tc.payload)) + ".sig"
			if got := codexPlanFromIDToken(token); got != tc.want {
				t.Fatalf("plan = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexBalanceShapes(t *testing.T) {
	cases := []struct {
		name       string
		balance    string
		want       *float64
		wantDetail string
	}{
		{"number", `12.5`, floatPtr(12.5), ""},
		{"string", `"12.5"`, floatPtr(12.5), ""},
		{"null", `null`, nil, ""},
		{"absent", `__ABSENT__`, nil, ""},
		{"object", `{"remaining":"7.25"}`, floatPtr(7.25), ""},
		{"object unknown", `{"unexpected":true}`, nil, "object"},
		{"array", `[1,2]`, nil, "array"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credits := ""
			if tc.balance == "__ABSENT__" {
				credits = `{}`
			} else {
				credits = `{"balance":` + tc.balance + `}`
			}
			var raw codexUsageRaw
			if err := json.Unmarshal([]byte(`{"rate_limit":{"primary_window":{"used_percent":47.5,"reset_at":1892851200,"limit_window_seconds":18000}},"credits":`+credits+`}`), &raw); err != nil {
				t.Fatal(err)
			}
			snap, detail, err := normalizeCodexUsage(raw, codexCredentials{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil && snap.Credits != nil {
				t.Fatalf("credits = %+v, want nil", snap.Credits)
			}
			if tc.want != nil && (snap.Credits == nil || snap.Credits.Balance != *tc.want) {
				t.Fatalf("credits = %+v, want %v", snap.Credits, *tc.want)
			}
			if tc.wantDetail != "" {
				if detail == nil || detail.Fields["credits.balance"] != tc.wantDetail {
					t.Fatalf("detail = %+v, want credits.balance=%s", detail, tc.wantDetail)
				}
			}
		})
	}
}

func TestCodexAuthMissingAndScopeMissing(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	snap, err := NewCodexProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if snap.Status != StatusAuthMissing {
		t.Fatalf("missing auth status = %s", snap.Status)
	}
	if snap.Error != codexLoginRequired {
		t.Fatalf("missing auth error = %q, want %q", snap.Error, codexLoginRequired)
	}

	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeCodexAuthFixture(t, root, `{"tokens":{"access_token":"access-secret","refresh_token":"refresh-secret"},"last_refresh":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		return httpJSON(http.StatusForbidden, `{"error":"secret-upstream"}`), nil
	})
	snap, err = NewCodexProvider().Fetch(context.Background(), NewStoreAt(t.TempDir()))
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if snap.Status != StatusScopeMissing {
		t.Fatalf("scope status = %+v", snap)
	}
	assertNotContains(t, jsonString(t, snap), "secret-upstream", "access-secret", "refresh-secret")
}

func TestCodexFetchErrorRedactsSecretsThroughManagerAndHTTP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeCodexAuthFixture(t, root, `{"tokens":{"access_token":"access-secret","refresh_token":"refresh-secret","account_id":"acct-secret"},"last_refresh":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	withHTTPTransport(t, func(req *http.Request) (*http.Response, error) {
		return httpJSON(http.StatusInternalServerError, `{"message":"raw-live-value"}`), nil
	})
	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderCodex] = ProviderConfig{Enabled: true}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, DefaultRegistry())
	resp, err := m.Snapshot(context.Background(), ProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	body := jsonString(t, resp)
	assertNotContains(t, body, "access-secret", "refresh-secret", "acct-secret", "raw-live-value")
	cacheData, err := os.ReadFile(store.CachePath())
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, string(cacheData), "access-secret", "refresh-secret", "acct-secret", "raw-live-value")
}

func TestJSONShapeDetailSoftCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{`)
	for i := 0; i < jsonShapeDetailMaxFields+10; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"field_%02d":%d`, i, i)
	}
	b.WriteString(`}`)

	detail := jsonShapeDetail("usage", []byte(b.String()))
	if detail == nil {
		t.Fatal("detail is nil")
	}
	if got := len(detail.Fields); got != jsonShapeDetailMaxFields+1 {
		t.Fatalf("fields = %d, want cap plus truncation marker (%d): %+v", got, jsonShapeDetailMaxFields+1, detail.Fields)
	}
	if got := detail.Fields["_truncated"]; got != "true" {
		t.Fatalf("_truncated = %q, want true", got)
	}
	for i := 0; i < jsonShapeDetailMaxFields; i++ {
		path := fmt.Sprintf("usage.field_%02d", i)
		if got := detail.Fields[path]; got != "number" {
			t.Fatalf("%s = %q, want number; fields=%+v", path, got, detail.Fields)
		}
	}
	if got := detail.Fields["usage.field_50"]; got != "" {
		t.Fatalf("usage.field_50 = %q, want omitted by cap", got)
	}
}

func floatPtr(v float64) *float64 { return &v }

func jsonString(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertNotContains(t *testing.T, body string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if strings.Contains(body, needle) {
			t.Fatalf("body leaked %q: %s", needle, body)
		}
	}
}
