package usages

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMistralCookieHeader(t *testing.T) {
	got, detail, err := normalizeMistralCookieHeader("curl 'https://console.mistral.ai' -H 'Cookie: csrftoken=csrf-value; ory_session_abc=session-value; ignored=x'")
	if err != nil {
		t.Fatalf("normalize error = %v detail=%#v", err, detail)
	}
	want := "csrftoken=csrf-value; ignored=x; ory_session_abc=session-value"
	if got != want {
		t.Fatalf("normalized = %q, want %q", got, want)
	}
	parsed, detail, err := parseMistralCookieHeader(got)
	if err != nil {
		t.Fatalf("parse error = %v detail=%#v", err, detail)
	}
	if parsed.CSRFToken() != "csrf-value" {
		t.Fatalf("csrf = %q", parsed.CSRFToken())
	}
	if !parsed.HasOrySession() {
		t.Fatalf("expected ory session")
	}
	if vibe := parsed.VibeCookieHeader(); vibe != "csrftoken=csrf-value; ory_session_abc=session-value" {
		t.Fatalf("vibe cookie = %q", vibe)
	}

	parsed, detail, err = parseMistralCookieHeader("csrftoken=csrf; ory_session_a=a; admin_secret=drop; session_token=drop; ory_session_b=b")
	if err != nil {
		t.Fatalf("parse allowlist error = %v detail=%#v", err, detail)
	}
	if vibe := parsed.VibeCookieHeader(); vibe != "csrftoken=csrf; ory_session_a=a; ory_session_b=b" {
		t.Fatalf("allowlisted vibe cookie = %q", vibe)
	}
}

func TestNormalizeMistralCookieHeaderRejectsInvalid(t *testing.T) {
	cases := []string{
		"",
		"ory_session_abc=session",
		"csrftoken=csrf,bad; ory_session_injected=bad",
		"csrftoken=csrf\r\nX-Leak: value; ory_session_abc=session",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, detail, err := normalizeMistralCookieHeader(in)
			if err == nil {
				t.Fatalf("normalize(%q) succeeded", in)
			}
			if detail == nil || len(detail.Fields) == 0 {
				t.Fatalf("detail = %#v", detail)
			}
			if strings.Contains(err.Error(), "session") || strings.Contains(err.Error(), "csrf;") {
				t.Fatalf("error leaked cookie value: %v", err)
			}
		})
	}
}

func TestDecodeMistralVibeUsage(t *testing.T) {
	raw, detail, err := decodeMistralVibeUsage([]byte(mistralVibeResponse(2.8141356666666666, true)))
	if err != nil {
		t.Fatalf("decode: %v detail=%#v", err, detail)
	}
	if raw.UsagePercentage != 2.8141356666666666 || !raw.PaygEnabled || raw.ResetAt != "2026-07-01T00:00:00Z" {
		t.Fatalf("raw = %#v", raw)
	}
	snap, detail, err := normalizeMistralVibeUsage(raw)
	if err != nil {
		t.Fatalf("normalize: %v detail=%#v", err, detail)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Key != "monthly" || snap.Windows[0].PercentUsed != raw.UsagePercentage {
		t.Fatalf("windows = %#v", snap.Windows)
	}
	if snap.Windows[0].ResetAt == nil || snap.Windows[0].ResetAt.Format(time.RFC3339) != "2026-07-01T00:00:00Z" {
		t.Fatalf("reset = %#v", snap.Windows[0].ResetAt)
	}
	if snap.Windows[0].WindowDurationSeconds <= 0 || snap.Windows[0].Pace == nil {
		t.Fatalf("pace/duration missing: %#v", snap.Windows[0])
	}
}

func TestDecodeMistralVibeUsageRejectsBadShapes(t *testing.T) {
	cases := []string{
		`[]`,
		mistralVibeResponse(101, false),
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			raw, detail, err := decodeMistralVibeUsage([]byte(body))
			if err == nil {
				_, detail, err = normalizeMistralVibeUsage(raw)
			}
			if err == nil {
				t.Fatalf("decode/normalize succeeded")
			}
			if detail == nil || len(detail.Fields) == 0 {
				t.Fatalf("detail = %#v", detail)
			}
		})
	}
}

func TestNormalizeMistralSpend(t *testing.T) {
	paid := 2000
	raw := mistralBillingResponse{
		Currency: "EUR",
		Prices: []mistralPrice{
			{BillingMetric: "input_tokens", BillingGroup: "mistral-large", Price: "0.000001"},
			{BillingMetric: "output_tokens", BillingGroup: "mistral-large", Price: "0.000003"},
			{BillingMetric: "cached_tokens", BillingGroup: "mistral-large", Price: "0.0000002"},
		},
		Completion: &mistralModelUsageCategory{Models: map[string]mistralModelUsageData{
			"mistral-large": {
				Input:  []mistralUsageEntry{{BillingMetric: "input_tokens", BillingGroup: "mistral-large", Value: 1000, ValuePaid: &paid}},
				Output: []mistralUsageEntry{{BillingMetric: "output_tokens", BillingGroup: "mistral-large", Value: 3000}},
				Cached: []mistralUsageEntry{{BillingMetric: "cached_tokens", BillingGroup: "mistral-large", Value: 500}},
			},
		}},
	}
	credits, ok := normalizeMistralSpend(raw)
	if !ok || credits == nil {
		t.Fatalf("credits missing ok=%v credits=%#v", ok, credits)
	}
	if credits.Label != "Monthly spend (EUR)" {
		t.Fatalf("label = %q", credits.Label)
	}
	if credits.Balance != 0.0111 {
		t.Fatalf("balance = %.8f", credits.Balance)
	}
}

func TestNormalizeMistralSpendSkipsZeroSpend(t *testing.T) {
	credits, ok := normalizeMistralSpend(mistralBillingResponse{})
	if ok || credits != nil {
		t.Fatalf("credits = %#v ok=%v", credits, ok)
	}
}

func TestSetMistralConfig(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	m := NewManager(store, EmptyRegistry())
	cfg, err := m.SetMistralConfig(context.Background(), ptrString("csrftoken=csrf; ory_session_abc=session"))
	if err != nil {
		t.Fatalf("SetMistralConfig: %v", err)
	}
	if !cfg.Providers[ProviderMistral].Enabled || cfg.Providers[ProviderMistral].Cookie == "" {
		t.Fatalf("provider config = %#v", cfg.Providers[ProviderMistral])
	}
	cfg, err = m.SetMistralConfig(context.Background(), ptrString(""))
	if err != nil {
		t.Fatalf("clear SetMistralConfig: %v", err)
	}
	if cfg.Providers[ProviderMistral].Enabled || cfg.Providers[ProviderMistral].Cookie != "" {
		t.Fatalf("cleared provider config = %#v", cfg.Providers[ProviderMistral])
	}
}

func TestMistralFetchKeepsVibeWindowWhenSpendFallbackFails(t *testing.T) {
	oldClient := mistralHTTPClient
	t.Cleanup(func() { mistralHTTPClient = oldClient })
	mistralHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "console.mistral.ai":
			if got := req.Header.Get("Cookie"); got != "csrftoken=csrf; ory_session_abc=session" {
				t.Fatalf("console Cookie = %q", got)
			}
			return testMistralResponse(req, http.StatusOK, mistralVibeResponse(4.5, true)), nil
		case "admin.mistral.ai":
			// Plan fetch and spend fetch both fail — plan falls back to "Vibe".
			return testMistralResponse(req, http.StatusServiceUnavailable, `{"error":"down"}`), nil
		default:
			t.Fatalf("unexpected host %s", req.URL.Host)
			return nil, nil
		}
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderMistral] = ProviderConfig{Enabled: true, Cookie: "csrftoken=csrf; ory_session_abc=session"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	snap, err := NewMistralProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if snap.Status != StatusOK || len(snap.Windows) != 1 || snap.Windows[0].PercentUsed != 4.5 {
		t.Fatalf("snapshot = %#v", snap)
	}
	if snap.Plan != "Vibe" {
		t.Fatalf("plan fallback = %q, want \"Vibe\"", snap.Plan)
	}
	if snap.Credits != nil {
		t.Fatalf("credits should not be attached on spend fallback failure: %#v", snap.Credits)
	}
	if snap.ErrorDetail == nil || snap.ErrorDetail.Fields["usage.error"] != "string" {
		t.Fatalf("expected fallback shape detail, got %#v", snap.ErrorDetail)
	}
}

func TestMistralFetchActivePlan(t *testing.T) {
	oldClient := mistralHTTPClient
	t.Cleanup(func() { mistralHTTPClient = oldClient })
	mistralHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "console.mistral.ai":
			return testMistralResponse(req, http.StatusOK, mistralVibeResponse(1.0, false)), nil
		case "admin.mistral.ai":
			if req.URL.Path == "/api/users/me" {
				return testMistralResponse(req, http.StatusOK, `{"organization":{"active_api_plan":"FREE"}}`), nil
			}
			t.Fatalf("unexpected admin path %s", req.URL.Path)
			return nil, nil
		default:
			t.Fatalf("unexpected host %s", req.URL.Host)
			return nil, nil
		}
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderMistral] = ProviderConfig{Enabled: true, Cookie: "csrftoken=csrf; ory_session_abc=session"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	snap, err := NewMistralProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if snap.Plan != "Free" {
		t.Fatalf("plan = %q, want \"Free\"", snap.Plan)
	}
}

func mistralVibeResponse(percent float64, payg bool) string {
	return `[{` +
		`"result":{"data":{"json":{` +
		`"usage_percentage":` + strconvFormatFloat(percent) + `,` +
		`"quota_changed_this_month":false,` +
		`"payg_enabled":` + strconvFormatBool(payg) + `,` +
		`"reset_at":"2026-07-01T00:00:00Z"` +
		`}}}}]`
}

func strconvFormatFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.16f", v), "0"), ".")
}

func strconvFormatBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func ptrString(v string) *string { return &v }

func testMistralResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}
