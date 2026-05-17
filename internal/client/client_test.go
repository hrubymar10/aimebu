package client

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeleteAgentUsesDeleteEndpoint(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := &Client{BaseURL: srv.URL}
	if err := c.DeleteAgent("worker@aimebu", time.Second); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodDelete {
		t.Fatalf("DeleteAgent used method %q, want %q", gotMethod, http.MethodDelete)
	}
	if gotPath != "/agents/worker@aimebu" {
		t.Fatalf("DeleteAgent used path %q, want %q", gotPath, "/agents/worker@aimebu")
	}
}

func TestInsecureSkipVerifyEnabled(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"1", true},
		{"true", true},
		{"YES", true},
		{" on ", true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("AIMEBU_INSECURE_SKIP_VERIFY", tc.raw)
			if got := insecureSkipVerifyEnabled(); got != tc.want {
				t.Fatalf("insecureSkipVerifyEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHTTPClientInsecureTransport(t *testing.T) {
	t.Setenv("AIMEBU_INSECURE_SKIP_VERIFY", "1")

	c := httpClient(3 * time.Second)
	if c.Timeout != 3*time.Second {
		t.Fatalf("Timeout = %s, want 3s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("TLSClientConfig = %#v, want InsecureSkipVerify", tr.TLSClientConfig)
	}
	if http.DefaultTransport.(*http.Transport).TLSClientConfig != nil &&
		http.DefaultTransport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatal("httpClient mutated http.DefaultTransport")
	}
}

func TestHTTPClientDefaultTransport(t *testing.T) {
	t.Setenv("AIMEBU_INSECURE_SKIP_VERIFY", "")

	if c := httpClient(0); c != http.DefaultClient {
		t.Fatalf("httpClient(0) = %#v, want http.DefaultClient", c)
	}
	c := httpClient(2 * time.Second)
	if c.Timeout != 2*time.Second {
		t.Fatalf("Timeout = %s, want 2s", c.Timeout)
	}
	if c.Transport != nil {
		t.Fatalf("Transport = %#v, want nil default transport", c.Transport)
	}
}

func TestHTTPClientInsecureUsesTLSConfig(t *testing.T) {
	t.Setenv("AIMEBU_INSECURE_SKIP_VERIFY", "yes")

	c := httpClient(0)
	tr := c.Transport.(*http.Transport)
	if _, ok := any(tr.TLSClientConfig).(*tls.Config); !ok {
		t.Fatalf("TLSClientConfig = %T, want *tls.Config", tr.TLSClientConfig)
	}
}
