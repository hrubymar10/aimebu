package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/cmdprobe"
)

// DockerPreflight checks the requested checkout through the actual launcher.
// A bad mount fails the spawn instead of creating an uncertain first turn.
func DockerPreflight(tool, checkout string, launcherPath ...string) error {
	if tool != "claude" && tool != "codex" && tool != "pi" {
		return fmt.Errorf("unsupported Docker tool %q", tool)
	}
	if err := DockerCheckout(checkout); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	launcher := tool + "-docker"
	if len(launcherPath) > 0 && launcherPath[0] != "" {
		launcher = launcherPath[0]
	}
	cmd := exec.CommandContext(ctx, launcher, "--version")
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(), strings.ToUpper(tool)+"_LAUNCH_FLAGS=")
	out, err := cmdprobe.CombinedOutput(cmd) // a launcher that exits 0 and leaves a helper behind passed its preflight
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Docker launcher preflight: %w", ctx.Err())
		}
		return fmt.Errorf("Docker launcher preflight: %s: %w", SanitizeStderr(string(out)), err)
	}
	return nil
}

// DockerCheckout keeps generated turn files below the mirrored checkout.
// The launcher itself rejects a cwd that is not mounted in its container.
func DockerCheckout(checkout string) error {
	if !filepath.IsAbs(checkout) {
		return errors.New("Docker checkout path must be absolute")
	}
	info, err := os.Stat(checkout)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("Docker checkout path is not a directory")
	}
	path := filepath.Join(checkout, ".aimebu")
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", path)
	}
	return nil
}

// DockerMCP reads the supervisor-private credential on the host. Docker turns
// pass an HTTP MCP endpoint inline, so no key-bearing file enters a checkout.
func DockerMCP(path, agentID string) (endpoint, authorization string, err error) {
	return DockerMCPAt(path, agentID, "")
}

func DockerMCPAt(path, agentID, visibleServerURL string) (endpoint, authorization string, err error) {
	endpoint, authorization, err = agentHTTPMCP(path, agentID, visibleServerURL == "")
	if err != nil || visibleServerURL == "" {
		return endpoint, authorization, err
	}
	visible, err := url.Parse(visibleServerURL)
	if err != nil || (visible.Scheme != "http" && visible.Scheme != "https") || visible.Hostname() == "" {
		return "", "", errors.New("invalid launcher agent server URL")
	}
	visible.Path = "/api/agent/mcp"
	visible.RawQuery, visible.Fragment = "", ""
	return visible.String(), authorization, nil
}

// NativeMCP uses the same validated private credential without rewriting the
// server address for a container.
func NativeMCP(path, agentID string) (endpoint, authorization string, err error) {
	return agentHTTPMCP(path, agentID, false)
}

func agentHTTPMCP(path, agentID string, docker bool) (endpoint, authorization string, err error) {
	if path == "" {
		return "", "", errors.New("normal turn requires agent tools config")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var config struct {
		Servers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", "", err
	}
	env := config.Servers["aimebu"].Env
	if env["AIMEBU2_AGENT_ID"] != agentID || !strings.HasPrefix(env["AIMEBU2_AGENT_KEY"], "aak_") {
		return "", "", errors.New("invalid private aimebu MCP config")
	}
	u, err := url.Parse(env["AIMEBU2_URL"])
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" {
		return "", "", errors.New("invalid aimebu server URL")
	}
	if docker && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		port := u.Port()
		u.Host = "host.docker.internal"
		if port != "" {
			u.Host += ":" + port
		}
	}
	u.Path = "/api/agent/mcp"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), "Bearer " + env["AIMEBU2_AGENT_KEY"], nil
}
