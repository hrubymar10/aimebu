package client

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/envflags"
)

// UnreachableError reports a transport-level failure talking to the aimebu
// server. Callers can detect it with errors.As / IsUnreachable.
type UnreachableError struct {
	BaseURL string
	Err     error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("aimebu unreachable (%s): %v", e.BaseURL, e.Err)
}

func (e *UnreachableError) Unwrap() error {
	return e.Err
}

func IsUnreachable(err error) bool {
	var target *UnreachableError
	return errors.As(err, &target)
}

// Client holds the base URL and the caller's AgentID. AgentID starts empty
// and is populated:
//   - for AI clients: from the server's response to POST /agents (bus_register)
//   - for human CLI calls: from --name or $AIMEBU_NAME
type Client struct {
	BaseURL   string
	AgentID   string
	AgentName string
	// Prompts caches the configured prompt bodies fetched on MCP initialize.
	// nil means not yet fetched; callers should use a compiled default when nil.
	Prompts map[string]string
}

var insecureSkipVerifyWarnOnce sync.Once

func insecureSkipVerifyEnabled() bool {
	return envflags.Enabled("AIMEBU_INSECURE_SKIP_VERIFY")
}

func httpClient(timeout time.Duration) *http.Client {
	if !insecureSkipVerifyEnabled() {
		if timeout == 0 {
			return http.DefaultClient
		}
		return &http.Client{Timeout: timeout}
	}
	insecureSkipVerifyWarnOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "WARNING: AIMEBU_INSECURE_SKIP_VERIFY is enabled; TLS certificate verification is disabled for aimebu client requests.")
	})
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Explicit opt-in development flag for self-signed certs.
	return &http.Client{Timeout: timeout, Transport: tr}
}

// DefaultClient returns a client with BaseURL set from $AIMEBU_URL (default
// http://localhost:9997) and an empty AgentID. Callers are responsible for
// populating AgentID before using any tool that requires it.
func DefaultClient() *Client {
	baseURL := os.Getenv("AIMEBU_URL")
	if baseURL == "" {
		baseURL = "http://localhost:9997"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	return &Client{BaseURL: baseURL}
}

func (c *Client) Post(path string, body interface{}) (string, error) {
	data, _ := json.Marshal(body)
	resp, err := httpClient(0).Post(c.BaseURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), nil
}

func (c *Client) Get(path string) (string, error) {
	resp, err := httpClient(0).Get(c.BaseURL + path)
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), nil
}

// GetWithTimeout issues a GET with a client-side timeout slightly longer than
// the server-side long-poll timeout, so the server has a chance to return
// cleanly before the client aborts.
func (c *Client) GetWithTimeout(path string, timeout time.Duration) (string, error) {
	resp, err := httpClient(timeout + 5*time.Second).Get(c.BaseURL + path)
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), nil
}

func (c *Client) Put(path string, body interface{}) (string, error) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", c.BaseURL+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient(0).Do(req)
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), nil
}

func (c *Client) Delete(path string) (string, error) {
	req, _ := http.NewRequest("DELETE", c.BaseURL+path, nil)
	resp, err := httpClient(0).Do(req)
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), nil
}

// DeleteWithTimeout issues a DELETE with a caller-supplied client timeout.
func (c *Client) DeleteWithTimeout(path string, timeout time.Duration) (string, error) {
	req, _ := http.NewRequest("DELETE", c.BaseURL+path, nil)
	resp, err := httpClient(timeout).Do(req)
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusBadRequest {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("DELETE %s: %s", path, msg)
	}
	return string(out), nil
}

// DeleteAgent deregisters an agent from the bus using a short-lived client.
func (c *Client) DeleteAgent(id string, timeout time.Duration) error {
	_, err := c.DeleteWithTimeout("/agents/"+url.PathEscape(id), timeout)
	return err
}

// Message fetches a single message by its global ID. The caller's AgentID
// is passed for the membership check; returns the raw JSON response.
func (c *Client) Message(id int64) (string, error) {
	return c.Get(fmt.Sprintf("/messages/%d?agent_id=%s", id, c.AgentID))
}

func PrettyJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return s
	}
	return buf.String()
}
