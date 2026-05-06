package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-json"
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
	resp, err := http.Post(c.BaseURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), nil
}

func (c *Client) Get(path string) (string, error) {
	resp, err := http.Get(c.BaseURL + path)
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
	httpClient := &http.Client{Timeout: timeout + 5*time.Second}
	resp, err := httpClient.Get(c.BaseURL + path)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), nil
}

func (c *Client) Delete(path string) (string, error) {
	req, _ := http.NewRequest("DELETE", c.BaseURL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", &UnreachableError{BaseURL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), nil
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
