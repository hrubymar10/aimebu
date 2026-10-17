package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
	"github.com/hrubymar10/aimebu/internal/switcher"
)

// switchViaServerTimeout covers a refresh of the incoming profile (the CLI run
// is allowed 4 minutes) plus the switch itself.
const switchViaServerTimeout = 8 * time.Minute

// cliSwitcher is the *switcher.Manager the CLI uses. On a token-copy install a
// switch is handed to the running server when one answers on this machine,
// because only the server owns the refresher: a switch to a profile that is due
// for a refresh needs it, and the CLI process has none. With no server the
// switch runs locally (and refuses a due incoming profile with a pointer to the
// server).
type cliSwitcher struct {
	*switcher.Manager
}

func (c cliSwitcher) Switch(tool, name string) (string, bool, error) {
	if c.TokenCopiesFor(tool) {
		if active, switched, handled, err := switchViaServer(tool, name); handled {
			return active, switched, err
		}
	}
	return c.Manager.Switch(tool, name)
}

// switchViaServer POSTs the switch to the local server. handled is false only
// when no server could be reached (connection refused, or AIMEBU_URL is not a
// loopback address: a remote server cannot see this machine's credential files),
// in which case the caller switches locally. Any answer from the server, error or
// not, is final.
func switchViaServer(tool, name string) (active string, switched, handled bool, err error) {
	base := client.DefaultClient().BaseURL
	if !isLoopbackURL(base) {
		return "", false, false, nil
	}
	body, _ := json.Marshal(map[string]string{"tool": tool, "profile": name})
	resp, postErr := (&http.Client{Timeout: switchViaServerTimeout}).Post(base+"/api/usages/switcher/switch", "application/json", bytes.NewReader(body))
	if postErr != nil {
		var opErr *net.OpError
		if errors.As(postErr, &opErr) && opErr.Op == "dial" {
			return "", false, false, nil // nobody is listening
		}
		return "", false, true, fmt.Errorf("switch through the server at %s: %w", base, postErr)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out struct {
		Active   string `json:"active"`
		Switched bool   `json:"switched"`
		Error    string `json:"error"`
	}
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode != http.StatusOK {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		if msg == "" {
			msg = resp.Status
		}
		return "", false, true, errors.New(msg)
	}
	return out.Active, out.Switched, true, nil
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
