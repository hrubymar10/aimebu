package usages

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUsageHTTPClientStripsCredentialsOnCrossOriginRedirect(t *testing.T) {
	var got http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("ChatGPT-Account-Id", "account-secret")
	req.Header.Set("X-Api-Key", "secret")
	req.Header.Set("X-Auth-Token", "secret")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")

	resp, err := newUsageHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	for _, name := range []string{"Authorization", "Cookie", "ChatGPT-Account-Id", "X-Api-Key", "X-Auth-Token"} {
		if got.Get(name) != "" {
			t.Fatalf("%s was forwarded on cross-origin redirect: %q", name, got.Get(name))
		}
	}
	if got.Get("X-Github-Api-Version") != "2025-04-01" {
		t.Fatalf("X-Github-Api-Version = %q, want preserved", got.Get("X-Github-Api-Version"))
	}
}

func TestUsageHTTPClientPreservesCredentialsOnSameOriginRedirect(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			got = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("ChatGPT-Account-Id", "account-secret")
	req.Header.Set("X-Api-Key", "secret")

	resp, err := newUsageHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if data, _ := io.ReadAll(resp.Body); len(data) > 0 {
		t.Fatalf("unexpected body: %s", data)
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"Authorization", "Bearer secret"},
		{"Cookie", "session=secret"},
		{"ChatGPT-Account-Id", "account-secret"},
		{"X-Api-Key", "secret"},
	} {
		if got.Get(tc.name) != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, got.Get(tc.name), tc.want)
		}
	}
}

func TestUsageHTTPClientStripsCredentialsOnSchemeRedirect(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "session=secret")
	prev, err := http.NewRequest(http.MethodGet, "http://example.com/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stripCrossOriginUsageCredentials(req, []*http.Request{prev}); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Cookie"); got != "" {
		t.Fatalf("Cookie = %q, want stripped", got)
	}
}

func TestUsageCredentialHeaderClassification(t *testing.T) {
	for _, name := range []string{"Authorization", "Cookie", "ChatGPT-Account-Id", "X-Api-Key", "X-Auth-Token", "X-Secret"} {
		if !usageCredentialHeader(name) {
			t.Fatalf("%s was not classified as credential-bearing", name)
		}
	}
	for _, name := range []string{"Accept", "User-Agent", "X-Github-Api-Version"} {
		if usageCredentialHeader(name) {
			t.Fatalf("%s was classified as credential-bearing", name)
		}
	}
}
