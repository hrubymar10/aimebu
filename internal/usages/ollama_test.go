package usages

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseOllamaCookieInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"raw header", "Cookie: __Secure-session=abc; foo=bar", "__Secure-session=abc; foo=bar"},
		{"curl header", "curl 'https://ollama.com/settings' -H 'Cookie: session=abc; foo=bar'", "foo=bar; session=abc"},
		{"curl cookie", "curl --cookie 'ollama_session=abc; foo=bar'", "foo=bar; ollama_session=abc"},
		{"curl b", "curl -b \"__Host-ollama_session=abc; foo=bar\"", "__Host-ollama_session=abc; foo=bar"},
		{"quoted blob", "'next-auth.session-token=abc; foo=bar'", "foo=bar; next-auth.session-token=abc"},
		{"chunked", "next-auth.session-token.0=abc; next-auth.session-token.1=def", "next-auth.session-token.0=abc; next-auth.session-token.1=def"},
		{"whitespace", " \n Cookie: session=abc ; foo=bar \n ", "foo=bar; session=abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail, err := normalizeOllamaCookieHeader(tc.in)
			if err != nil {
				t.Fatalf("normalize error = %v detail=%v", err, detail)
			}
			if got != tc.want {
				t.Fatalf("normalized = %q, want %q", got, tc.want)
			}
			pairs, err := parseOllamaCookieInput(tc.in)
			if err != nil {
				t.Fatalf("parse error = %v", err)
			}
			if len(pairs) == 0 {
				t.Fatal("pairs empty")
			}
		})
	}
}

func TestParseOllamaCookieInputRejectsInvalid(t *testing.T) {
	for _, in := range []string{"", "foo=bar"} {
		_, detail, err := normalizeOllamaCookieHeader(in)
		if err == nil {
			t.Fatalf("normalize(%q) succeeded", in)
		}
		if detail == nil || detail.Fields["cookie"] == "" {
			t.Fatalf("detail = %#v", detail)
		}
		if strings.Contains(err.Error(), "bar") {
			t.Fatalf("error leaked cookie value: %v", err)
		}
	}
}

func TestParseOllamaSettingsHTML(t *testing.T) {
	reset := "2026-05-16T22:00:00Z"
	full := `<span>Cloud Usage</span><span>Pro</span><span id="header-email">user@example.com</span>` +
		`<div>Session usage <span>42% used</span><time data-time="` + reset + `"></time></div>` +
		`<div>Weekly usage <span style="width: 12.5%"></span><time data-time="2026-05-20T22:00:00.000Z"></time></div>`
	snap, detail, err := parseOllamaSettingsHTML([]byte(full))
	if err != nil {
		t.Fatalf("parse full: %v detail=%#v", err, detail)
	}
	if snap.Plan != "Pro" || len(snap.Windows) != 2 {
		t.Fatalf("snap = %#v", snap)
	}
	if snap.Windows[0].Key != "session" || snap.Windows[0].PercentUsed != 42 {
		t.Fatalf("session = %#v", snap.Windows[0])
	}
	if snap.Windows[0].ResetAt == nil || snap.Windows[0].ResetAt.Format(time.RFC3339) != reset {
		t.Fatalf("reset = %#v", snap.Windows[0].ResetAt)
	}
	if snap.Windows[1].Key != "weekly" || snap.Windows[1].PercentUsed != 12.5 {
		t.Fatalf("weekly = %#v", snap.Windows[1])
	}

	longMeter := strings.Repeat(`<span class="usage-meter-segment" style="left: 1%"></span>`, 30)
	sectioned := `<span>Cloud Usage</span><span>Pro</span>` +
		`<section><span>Session usage</span><span>4.1% used</span><div class="usage-meter">` + longMeter + `</div>` +
		`<div class="local-time" data-time="2026-05-24T22:00:00Z">Resets in 3 hours</div></section>` +
		`<section><span>Weekly usage</span><span style="width: 14.1%"></span><div class="usage-meter">` + longMeter + `</div>` +
		`<div class="local-time" data-time="2026-05-25T00:00:00Z">Resets in 5 hours</div></section>`
	snap, detail, err = parseOllamaSettingsHTML([]byte(sectioned))
	if err != nil {
		t.Fatalf("parse sectioned: %v detail=%#v", err, detail)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("sectioned windows = %#v", snap.Windows)
	}
	if snap.Windows[0].Key != "session" || snap.Windows[0].PercentUsed != 4.1 ||
		snap.Windows[0].ResetAt == nil || snap.Windows[0].ResetAt.Format(time.RFC3339) != "2026-05-24T22:00:00Z" {
		t.Fatalf("sectioned session = %#v", snap.Windows[0])
	}
	if snap.Windows[1].Key != "weekly" || snap.Windows[1].PercentUsed != 14.1 ||
		snap.Windows[1].ResetAt == nil || snap.Windows[1].ResetAt.Format(time.RFC3339) != "2026-05-25T00:00:00Z" {
		t.Fatalf("sectioned weekly = %#v", snap.Windows[1])
	}
	if detail != nil {
		if _, ok := detail.Fields["session.reset_at"]; ok {
			t.Fatalf("sectioned detail = %#v", detail)
		}
		if _, ok := detail.Fields["weekly.reset_at"]; ok {
			t.Fatalf("sectioned detail = %#v", detail)
		}
	}

	labeledTrack := `<h2 class="text-xl font-medium flex items-center space-x-2">
    <span>Cloud usage</span>
    <span
      class="text-xs font-normal px-2 py-0.5 rounded-full bg-neutral-100 text-neutral-600 capitalize"
      >pro</span
    >
  </h2>
  <div>
    <div class="flex justify-between mb-2">
      <span class="text-sm ">Session usage</span>
      <span class="text-sm ">
        5% used
      </span>
    </div>
    <div class="usage-meter" data-usage-meter>
      <div class="usage-meter__bubble" data-usage-bubble aria-hidden="true">
        <span class="usage-meter__bubble-model" data-usage-model></span>
        <span class="usage-meter__bubble-requests" data-usage-requests></span>
      </div>
      <div
        class="usage-meter__track"
        data-usage-track
        aria-label="Session usage 5% used"
      >
        <div
          class="usage-meter__fill"
          style="width: 5%; "
        >
          <button
            type="button"
            class="usage-meter__segment"
            style="width: 100%; background: #5ac8fa"
            data-model="gemma4:31b"
            data-requests="302"
            aria-label="gemma4:31b: 302 requests"
          ></button>
        </div>
      </div>
    </div>
    <div
      class="text-xs text-neutral-500 mt-1 local-time"
      data-time="2026-05-24T22:00:00Z"
    >
      Resets in 2 hours
    </div>
  </div>

  <div>
    <div class="flex justify-between mb-2">
      <span class="text-sm">Weekly usage</span>
      <span class="text-sm "
        >14.2% used</span
      >
    </div>
    <div class="usage-meter" data-usage-meter>
      <div class="usage-meter__bubble" data-usage-bubble aria-hidden="true">
        <span class="usage-meter__bubble-model" data-usage-model></span>
        <span class="usage-meter__bubble-requests" data-usage-requests></span>
      </div>
      <div
        class="usage-meter__track"
        data-usage-track
        aria-label="Weekly usage 14.2% used"
      >
        <div
          class="usage-meter__fill"
          style="width: 14.2%"
        >
          <button
            type="button"
            class="usage-meter__segment"
            style="width: 100%; background: #5ac8fa"
            data-model="gemma4:31b"
            data-requests="2726"
            aria-label="gemma4:31b: 2726 requests"
          ></button>
        </div>
      </div>
    </div>
    <div
      class="text-xs text-neutral-500 mt-1 local-time"
      data-time="2026-05-25T00:00:00Z"
    >
      Resets in 4 hours
    </div>
  </div>`
	snap, detail, err = parseOllamaSettingsHTML([]byte(labeledTrack))
	if err != nil {
		t.Fatalf("parse labeled track: %v detail=%#v", err, detail)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("labeled track windows = %#v", snap.Windows)
	}
	if snap.Plan != "pro" {
		t.Fatalf("labeled track plan = %q", snap.Plan)
	}
	if snap.Windows[0].Key != "session" || snap.Windows[0].PercentUsed != 5 ||
		snap.Windows[0].ResetAt == nil || snap.Windows[0].ResetAt.Format(time.RFC3339) != "2026-05-24T22:00:00Z" {
		t.Fatalf("labeled track session = %#v", snap.Windows[0])
	}
	if snap.Windows[1].Key != "weekly" || snap.Windows[1].PercentUsed != 14.2 ||
		snap.Windows[1].ResetAt == nil || snap.Windows[1].ResetAt.Format(time.RFC3339) != "2026-05-25T00:00:00Z" {
		t.Fatalf("labeled track weekly = %#v", snap.Windows[1])
	}

	weeklyResetOnly := `<span>Cloud Usage</span><span>Pro</span>` +
		`<div>Session usage <span>4% used</span></div>` +
		`<div>Weekly usage <span>14% used</span><time data-time="2026-05-25T00:00:00Z"></time></div>`
	snap, detail, err = parseOllamaSettingsHTML([]byte(weeklyResetOnly))
	if err != nil {
		t.Fatalf("parse weekly reset only: %v detail=%#v", err, detail)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("weekly reset only windows = %#v", snap.Windows)
	}
	if snap.Windows[0].Key != "session" || snap.Windows[0].ResetAt != nil ||
		detail == nil || detail.Fields["session.reset_at"] != "missing" {
		t.Fatalf("weekly reset only session=%#v detail=%#v", snap.Windows[0], detail)
	}
	if snap.Windows[1].Key != "weekly" || snap.Windows[1].ResetAt == nil ||
		snap.Windows[1].ResetAt.Format(time.RFC3339) != "2026-05-25T00:00:00Z" {
		t.Fatalf("weekly reset only weekly=%#v detail=%#v", snap.Windows[1], detail)
	}

	noResets := `<span>Cloud Usage</span><span>Pro</span>` +
		`<div>Session usage <span>4% used</span></div>` +
		`<div>Weekly usage <span>14% used</span></div>`
	snap, detail, err = parseOllamaSettingsHTML([]byte(noResets))
	if err != nil {
		t.Fatalf("parse no resets: %v detail=%#v", err, detail)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].ResetAt != nil || snap.Windows[1].ResetAt != nil ||
		detail == nil || detail.Fields["session.reset_at"] != "missing" || detail.Fields["weekly.reset_at"] != "missing" {
		t.Fatalf("no resets snap=%#v detail=%#v", snap, detail)
	}

	hourly := `<span>Cloud Usage</span><span>Free</span><div>Hourly usage <span style="width: 101%"></span></div>`
	snap, detail, err = parseOllamaSettingsHTML([]byte(hourly))
	if err != nil {
		t.Fatalf("parse hourly: %v detail=%#v", err, detail)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Key != "session" || snap.Windows[0].PercentUsed != 100 {
		t.Fatalf("hourly snap = %#v", snap)
	}
	if detail == nil || detail.Fields["session.reset_at"] != "missing" {
		t.Fatalf("hourly detail = %#v", detail)
	}

	weeklyOnly := `<span>Cloud Usage</span><span>Free</span><div>Weekly usage <span>3% used</span></div>`
	snap, _, err = parseOllamaSettingsHTML([]byte(weeklyOnly))
	if err != nil {
		t.Fatalf("parse weekly only: %v", err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Key != "weekly" {
		t.Fatalf("weekly only = %#v", snap.Windows)
	}

	noBars := `<span>Cloud Usage</span><span>Free</span><p>No active quota bars.</p>`
	snap, detail, err = parseOllamaSettingsHTML([]byte(noBars))
	if err != nil {
		t.Fatalf("parse no bars: %v", err)
	}
	if len(snap.Windows) != 0 || detail == nil || detail.Fields["windows"] != "missing" {
		t.Fatalf("no bars snap=%#v detail=%#v", snap, detail)
	}

	lowerNoBars := `<span>Cloud usage</span><p>No active quota bars.</p>`
	snap, detail, err = parseOllamaSettingsHTML([]byte(lowerNoBars))
	if err != nil {
		t.Fatalf("parse lower no bars: %v", err)
	}
	if snap.Status != StatusOK || snap.Plan != "" || len(snap.Windows) != 0 ||
		detail == nil || detail.Fields["windows"] != "missing" {
		t.Fatalf("lower no bars snap=%#v detail=%#v", snap, detail)
	}

	invalidReset := `<span>Cloud Usage</span><span>Pro</span><div>Session usage <span>4% used</span><time data-time="soon"></time></div>`
	snap, detail, err = parseOllamaSettingsHTML([]byte(invalidReset))
	if err != nil {
		t.Fatalf("parse invalid reset: %v", err)
	}
	if snap.Windows[0].ResetAt != nil || detail == nil || detail.Fields["session.reset_at"] != "string" {
		t.Fatalf("invalid reset snap=%#v detail=%#v", snap, detail)
	}
}

func TestParseOllamaSettingsHTMLClassifiesAuthAndDrift(t *testing.T) {
	_, detail, err := parseOllamaSettingsHTML([]byte(`<h1>Sign in to Ollama</h1><form action="/auth/login"><input type="password"></form>`))
	if err == nil || detail == nil || detail.Fields["page"] != "signed_out" {
		t.Fatalf("signed out err=%v detail=%#v", err, detail)
	}
	_, detail, err = parseOllamaSettingsHTML([]byte(`<form><input type="password"></form>`))
	if err == nil || detail == nil || detail.Fields["page"] != "markup_drift" {
		t.Fatalf("generic password form err=%v detail=%#v", err, detail)
	}
	_, detail, err = parseOllamaSettingsHTML([]byte(`<html><body>settings</body></html>`))
	if err == nil || detail == nil || detail.Fields["page"] != "markup_drift" {
		t.Fatalf("drift err=%v detail=%#v", err, detail)
	}
}

func TestOllamaFetchRequestAndStatusMapping(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != ollamaSettingsURL {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		for key, want := range map[string]string{
			"Cookie":          "foo=bar; session=abc",
			"User-Agent":      ollamaUserAgent,
			"Accept":          ollamaAccept,
			"Accept-Language": "en-US,en;q=0.9",
			"Origin":          "https://ollama.com",
			"Referer":         ollamaSettingsURL,
		} {
			if got := req.Header.Get(key); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`<span>Cloud Usage</span><span>Pro</span><div>Session usage <span>1% used</span></div>`)),
			Header:     http.Header{},
		}, nil
	})}
	body, _, status, err := fetchOllamaSettingsHTML(context.Background(), "foo=bar; session=abc")
	if err != nil || status != "" || len(body) == 0 {
		t.Fatalf("fetch err=%v status=%s body=%q", err, status, body)
	}

	for code, want := range map[int]Status{http.StatusUnauthorized: StatusAuthMissing, http.StatusForbidden: StatusAuthMissing, http.StatusInternalServerError: StatusFetchError} {
		ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("upstream secret body")), Header: http.Header{}}, nil
		})}
		_, _, got, err := fetchOllamaSettingsHTML(context.Background(), "session=abc")
		if err == nil || got != want {
			t.Fatalf("status %d => got status=%s err=%v, want %s", code, got, err, want)
		}
	}
}

func TestOllamaSettingsRequestTimeout(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()
	ollamaHTTPClient = &http.Client{Timeout: time.Millisecond, Transport: roundTripFunc(timeoutRoundTrip)}

	_, _, status, err := fetchOllamaSettingsHTML(context.Background(), "session=abc")
	if err == nil {
		t.Fatal("fetchOllamaSettingsHTML succeeded")
	}
	if status != StatusTimeout {
		t.Fatalf("status = %s, want %s", status, StatusTimeout)
	}
}

func TestOllamaAPIKeyFetchRequestAndStatusMapping(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != ollamaTagsURL {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer api-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q", got)
		}
		return httpJSON(http.StatusOK, `{"models":[{"name":"gpt-oss:120b"}]}`), nil
	})}
	body, _, status, err := fetchOllamaTags(context.Background(), "api-secret")
	if err != nil || status != "" || len(body) == 0 {
		t.Fatalf("fetch err=%v status=%s body=%q", err, status, body)
	}

	for code, want := range map[int]Status{http.StatusUnauthorized: StatusAuthMissing, http.StatusForbidden: StatusAuthMissing, http.StatusInternalServerError: StatusFetchError} {
		ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return httpJSON(code, `{"message":"raw-live-value"}`), nil
		})}
		_, _, got, err := fetchOllamaTags(context.Background(), "api-secret")
		if err == nil || got != want {
			t.Fatalf("status %d => got status=%s err=%v, want %s", code, got, err, want)
		}
	}
}

func TestOllamaFetchUsesAPIKeyMode(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != ollamaTagsURL {
			t.Fatalf("request URL = %s, want tags API", req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer api-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		return httpJSON(http.StatusOK, `{"models":[{}]}`), nil
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderOllamaCloud] = ProviderConfig{Enabled: true, AuthMode: OllamaAuthAPIKey, APIKey: "api-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	snap, err := NewOllamaCloudProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != StatusOK || snap.Plan != "API key verified" || len(snap.Windows) != 0 {
		t.Fatalf("snapshot = %#v", snap)
	}
}

func TestOllamaFetchRejectsAPIKey(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return httpJSON(http.StatusUnauthorized, `{"error":"bad-key"}`), nil
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderOllamaCloud] = ProviderConfig{Enabled: true, AuthMode: OllamaAuthAPIKey, APIKey: "bad-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	snap, err := NewOllamaCloudProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != StatusAuthMissing {
		t.Fatalf("snapshot = %#v", snap)
	}
	assertNoOllamaSecret(t, jsonString(t, snap), "bad-key", "bad-secret")
}

func TestOllamaFetchRetriesSessionCookieCandidates(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()

	var cookies []string
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		cookie := req.Header.Get("Cookie")
		cookies = append(cookies, cookie)
		body := `<h1>Sign in to Ollama</h1><form action="/auth/login"><input type="password"></form>`
		if cookie == "__Secure-session=valid" {
			body = `<span>Cloud Usage</span><span>Pro</span><div>Session usage <span>1% used</span></div>`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderOllamaCloud] = ProviderConfig{Enabled: true, Cookie: "session=stale; __Secure-session=valid"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	snap, err := NewOllamaCloudProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != StatusOK || snap.Plan != "Pro" {
		t.Fatalf("snapshot = %#v", snap)
	}
	if got, want := strings.Join(cookies, "|"), "__Secure-session=valid; session=stale|__Secure-session=valid"; got != want {
		t.Fatalf("cookies = %q, want %q", got, want)
	}
}

func TestOllamaAutoUsesCookieBeforeAPIKey(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()

	var urls []string
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		urls = append(urls, req.URL.String())
		if req.URL.String() == ollamaTagsURL {
			t.Fatal("API key should not be used when cookie succeeds")
		}
		return httpJSON(http.StatusOK, `<span>Cloud Usage</span><span>Pro</span><div>Session usage <span>1% used</span></div>`), nil
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderOllamaCloud] = ProviderConfig{Enabled: true, AuthMode: OllamaAuthAuto, Cookie: "session=valid", APIKey: "api-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	snap, err := NewOllamaCloudProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != StatusOK || snap.Plan != "Pro" || len(snap.Windows) == 0 {
		t.Fatalf("snapshot = %#v", snap)
	}
	if got := strings.Join(urls, "|"); got != ollamaSettingsURL {
		t.Fatalf("urls = %q, want settings only", got)
	}
}

func TestOllamaAutoFallsBackToAPIKey(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()

	var urls []string
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		urls = append(urls, req.URL.String())
		if req.URL.String() == ollamaSettingsURL {
			return httpJSON(http.StatusInternalServerError, `settings failed`), nil
		}
		if req.URL.String() == ollamaTagsURL {
			if got := req.Header.Get("Authorization"); got != "Bearer api-secret" {
				t.Fatalf("Authorization = %q", got)
			}
			return httpJSON(http.StatusOK, `{"models":[{}]}`), nil
		}
		t.Fatalf("unexpected URL %s", req.URL)
		return nil, nil
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderOllamaCloud] = ProviderConfig{Enabled: true, AuthMode: OllamaAuthAuto, Cookie: "session=valid", APIKey: "api-secret"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	snap, err := NewOllamaCloudProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != StatusOK || snap.Plan != "API key verified" {
		t.Fatalf("snapshot = %#v", snap)
	}
	if got := strings.Join(urls, "|"); got != ollamaSettingsURL+"|"+ollamaSettingsURL+"|"+ollamaTagsURL {
		t.Fatalf("urls = %q", got)
	}
}

func TestOllamaFetchKeepsSignedOutWhenCandidatesFail(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()

	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`<h1>Sign in to Ollama</h1><form action="/auth/login"><input type="password"></form>`)),
			Header:     http.Header{},
		}, nil
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderOllamaCloud] = ProviderConfig{Enabled: true, Cookie: "session=expired; __Secure-session=expired"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	snap, err := NewOllamaCloudProvider().Fetch(context.Background(), store)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Status != StatusAuthMissing || snap.ErrorDetail == nil || snap.ErrorDetail.Fields["page"] != "signed_out" {
		t.Fatalf("snapshot = %#v", snap)
	}
}

func TestOllamaCookieEndpointSanitizesAndClears(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	m := NewManager(store, DefaultRegistry())
	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)

	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/usages/ollama/cookie", strings.NewReader(`{"cookie":"Cookie: session=secret-value; foo=bar"}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "secret-value") || strings.Contains(resp.Body.String(), `"cookie"`) {
		t.Fatalf("save response leaked cookie: %s", resp.Body.String())
	}
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers[ProviderOllamaCloud].Enabled || !strings.Contains(cfg.Providers[ProviderOllamaCloud].Cookie, "session=secret-value") {
		t.Fatalf("saved config = %#v", cfg.Providers[ProviderOllamaCloud])
	}

	resp = httptest.NewRecorder()
	mux.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/usages/ollama/cookie", strings.NewReader(`{"cookie":""}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", resp.Code, resp.Body.String())
	}
	cfg, err = store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[ProviderOllamaCloud].Enabled || cfg.Providers[ProviderOllamaCloud].Cookie != "" {
		t.Fatalf("cleared config = %#v", cfg.Providers[ProviderOllamaCloud])
	}
}

func TestOllamaConfigEndpointSanitizesAndClears(t *testing.T) {
	store := NewStoreAt(t.TempDir())
	m := NewManager(store, DefaultRegistry())
	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)

	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/usages/ollama/config", strings.NewReader(`{"auth_mode":"auto","api_key":"api-secret-value","cookie":"Cookie: session=cookie-secret-value"}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", resp.Code, resp.Body.String())
	}
	assertNoOllamaSecret(t, resp.Body.String(), "api-secret-value", "cookie-secret-value")
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.Providers[ProviderOllamaCloud]
	if !pc.Enabled || pc.AuthMode != OllamaAuthAuto || pc.APIKey != "api-secret-value" || !strings.Contains(pc.Cookie, "cookie-secret-value") {
		t.Fatalf("saved config = %#v", pc)
	}

	resp = httptest.NewRecorder()
	mux.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/usages/ollama/config", strings.NewReader(`{"auth_mode":"auto","api_key":"","cookie":""}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", resp.Code, resp.Body.String())
	}
	cfg, err = store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	pc = cfg.Providers[ProviderOllamaCloud]
	if pc.Enabled || pc.APIKey != "" || pc.Cookie != "" {
		t.Fatalf("cleared config = %#v", pc)
	}
}

func TestOllamaCookieRedactionThroughManagerCacheAndHTTP(t *testing.T) {
	old := ollamaHTTPClient
	defer func() { ollamaHTTPClient = old }()
	secret := "session=ollama-secret-value-1234567890"
	apiSecret := "api-secret-value-1234567890"
	upstreamBody := "upstream included ollama-secret-value-1234567890"
	ollamaHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader(upstreamBody)), Header: http.Header{}}, nil
	})}

	store := NewStoreAt(t.TempDir())
	cfg := DefaultConfig()
	cfg.Providers[ProviderOllamaCloud] = ProviderConfig{Enabled: true, AuthMode: OllamaAuthCookie, APIKey: apiSecret, Cookie: secret}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, DefaultRegistry())
	_, _, _ = m.ForceRefresh(context.Background(), ProviderOllamaCloud)

	cacheData, err := io.ReadAll(mustOpen(t, store.CachePath()))
	if err != nil {
		t.Fatal(err)
	}
	assertNoOllamaSecret(t, string(cacheData), secret, apiSecret, upstreamBody)

	mux := http.NewServeMux()
	Routes{Manager: m}.Mount(mux)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/usages", nil),
		httptest.NewRequest(http.MethodPost, "/api/usages/providers", strings.NewReader(`{"provider":"ollama-cloud","enabled":true}`)),
		httptest.NewRequest(http.MethodPost, "/api/usages/ollama/cookie", strings.NewReader(`{"cookie":"Cookie: session=next-secret-value"}`)),
	} {
		resp := httptest.NewRecorder()
		mux.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s %s status=%d body=%s", req.Method, req.URL.Path, resp.Code, resp.Body.String())
		}
		assertNoOllamaSecret(t, resp.Body.String(), secret, apiSecret, upstreamBody)
		if req.URL.Path == "/api/usages/ollama/cookie" {
			assertNoOllamaSecret(t, resp.Body.String(), "next-secret-value", upstreamBody)
		}
	}
}

func TestOllamaProviderRegistered(t *testing.T) {
	if !DefaultRegistry().HasProvider(ProviderOllamaCloud) {
		t.Fatal("ollama-cloud provider is not registered")
	}
}

func assertNoOllamaSecret(t *testing.T, haystack string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value != "" && strings.Contains(haystack, value) {
			t.Fatalf("secret leaked in %q", haystack)
		}
	}
}

func TestOllamaWeeklyWindowPace(t *testing.T) {
	// Weekly window with a future reset_at should get WindowDurationSeconds set
	// and a non-nil Pace. Session window (ambiguous duration) should have neither.
	futureReset := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	html := `<span>Cloud Usage</span><span>Pro</span>` +
		`<div>Session usage <span>30% used</span><time data-time="` + futureReset + `"></time></div>` +
		`<div>Weekly usage <span style="width: 40%"></span><time data-time="` + futureReset + `"></time></div>`
	snap, _, err := parseOllamaSettingsHTML([]byte(html))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(snap.Windows))
	}
	var session, weekly Window
	for _, w := range snap.Windows {
		switch w.Key {
		case "session":
			session = w
		case "weekly":
			weekly = w
		}
	}
	if session.WindowDurationSeconds != 0 {
		t.Errorf("session.WindowDurationSeconds = %d, want 0 (ambiguous duration)", session.WindowDurationSeconds)
	}
	if session.Pace != nil {
		t.Errorf("session.Pace should be nil, got %+v", session.Pace)
	}
	const wantDuration = int64(7 * 24 * 3600)
	if weekly.WindowDurationSeconds != wantDuration {
		t.Errorf("weekly.WindowDurationSeconds = %d, want %d", weekly.WindowDurationSeconds, wantDuration)
	}
	if weekly.Pace == nil {
		t.Error("weekly.Pace is nil, expected non-nil for window with future reset_at and 7d duration")
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
