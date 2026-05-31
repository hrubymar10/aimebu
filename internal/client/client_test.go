package client

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestClientCoreRequestShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		run        func(*Client) (string, error)
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{
			name: "post",
			run: func(c *Client) (string, error) {
				return c.Post("/rooms/general/join", map[string]string{"agent_id": "alice@aimebu"})
			},
			wantMethod: http.MethodPost,
			wantPath:   "/rooms/general/join",
			wantBody:   `"agent_id":"alice@aimebu"`,
		},
		{
			name:       "get",
			run:        func(c *Client) (string, error) { return c.Get("/agents") },
			wantMethod: http.MethodGet,
			wantPath:   "/agents",
		},
		{
			name: "get with timeout",
			run: func(c *Client) (string, error) {
				return c.GetWithTimeout("/agents/alice@aimebu/wait?timeout=1", time.Second)
			},
			wantMethod: http.MethodGet,
			wantPath:   "/agents/alice@aimebu/wait",
		},
		{
			name: "put",
			run: func(c *Client) (string, error) {
				return c.Put("/macros", map[string]map[string]string{"macros": {"LGTM": "looks good"}})
			},
			wantMethod: http.MethodPut,
			wantPath:   "/macros",
			wantBody:   `"LGTM":"looks good"`,
		},
		{
			name:       "delete",
			run:        func(c *Client) (string, error) { return c.Delete("/rooms/general") },
			wantMethod: http.MethodDelete,
			wantPath:   "/rooms/general",
		},
		{
			name:       "react add",
			run:        func(c *Client) (string, error) { c.AgentID = "alice@aimebu"; return c.React(42, "👍", false) },
			wantMethod: http.MethodPut,
			wantPath:   "/messages/42/reactions",
			wantBody:   `"emoji":"👍"`,
		},
		{
			name:       "react remove",
			run:        func(c *Client) (string, error) { c.AgentID = "alice@aimebu"; return c.React(42, "👍", true) },
			wantMethod: http.MethodDelete,
			wantPath:   "/messages/42/reactions",
			wantBody:   `"agent_id":"alice@aimebu"`,
		},
		{
			name:       "message",
			run:        func(c *Client) (string, error) { c.AgentID = "alice@aimebu"; return c.Message(42) },
			wantMethod: http.MethodGet,
			wantPath:   "/messages/42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod, gotPath, gotRawQuery, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotRawQuery = r.URL.RawQuery
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				gotBody = string(body)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
			}))
			t.Cleanup(srv.Close)

			c := &Client{BaseURL: srv.URL}
			got, err := tt.run(c)
			if err != nil {
				t.Fatalf("client call returned error: %v", err)
			}
			if got != `{"ok":true}` {
				t.Fatalf("response = %q, want raw body", got)
			}
			if gotMethod != tt.wantMethod || gotPath != tt.wantPath {
				t.Fatalf("request = %s %s, want %s %s", gotMethod, gotPath, tt.wantMethod, tt.wantPath)
			}
			if tt.wantBody != "" && !strings.Contains(gotBody, tt.wantBody) {
				t.Fatalf("body = %q, want substring %q", gotBody, tt.wantBody)
			}
			if tt.name == "message" && gotRawQuery != "agent_id=alice@aimebu" {
				t.Fatalf("message query = %q, want agent_id", gotRawQuery)
			}
		})
	}
}

func TestClientTransportAndStatusErrors(t *testing.T) {
	t.Run("transport error is unreachable", func(t *testing.T) {
		c := &Client{BaseURL: "http://127.0.0.1:1"}
		if _, err := c.Get("/health"); !IsUnreachable(err) {
			t.Fatalf("Get error = %v, want unreachable", err)
		}
	})

	t.Run("delete with timeout reports non 2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusTeapot)
		}))
		t.Cleanup(srv.Close)

		c := &Client{BaseURL: srv.URL}
		err := c.DeleteAgent("alice@aimebu", time.Second)
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("DeleteAgent error = %v, want status body", err)
		}
	})

	t.Run("heartbeat reports non 2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad heartbeat", http.StatusBadRequest)
		}))
		t.Cleanup(srv.Close)

		c := &Client{BaseURL: srv.URL}
		err := c.HeartbeatAgent("alice@aimebu", time.Second)
		if err == nil || !strings.Contains(err.Error(), "bad heartbeat") {
			t.Fatalf("HeartbeatAgent error = %v, want status body", err)
		}
	})
}
