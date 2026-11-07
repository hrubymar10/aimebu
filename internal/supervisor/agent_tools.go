package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/tlsclient"
)

func (c *Client) agentToolsPath(agentID string) string {
	hash := sha256.Sum256([]byte(agentID))
	return filepath.Join(filepath.Dir(c.Journal.path), "agent-tools", hex.EncodeToString(hash[:])+".json")
}

// forgetAgentKey deletes what holds an agent's key once the agent is stopped: the agent tools config and whatever generated config an adapter
// wrote from it. The server issues a new key whenever the agent starts again and the spawn writes these anew, so nothing a stopped agent
// leaves on the disk can authenticate, and nothing stays there after it is removed.
func (c *Client) forgetAgentKey(agentID string) {
	if err := os.Remove(c.agentToolsPath(agentID)); err != nil && !os.IsNotExist(err) {
		c.report(fmt.Errorf("remove agent tools config for %s: %w", agentID, err))
	}
	if forgetter, ok := c.Adapter.(interface{ ForgetAgent(string) }); ok {
		forgetter.ForgetAgent(agentID)
	}
}

// SetTelemetryEnv sets the environment the agent-tools bridge is started with for error and performance reporting (AIMEBU2_SENTRY_DSN, ...; an empty
// map means none, and the bridge then reports nothing unless its own environment says otherwise). It reaches the bridge through the environment of
// the MCP config entry, never its arguments, and is written into the configs that exist, so the next bridge any agent starts has it; a bridge
// that is already running keeps what it started with.
func (c *Client) SetTelemetryEnv(env map[string]string) {
	c.mu.Lock()
	c.telemetryEnv = env
	c.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(c.Journal.path), "agent-tools"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if err := c.updateAgentToolsTelemetry(filepath.Join(filepath.Dir(c.Journal.path), "agent-tools", entry.Name()), env); err != nil {
			c.report(fmt.Errorf("telemetry settings for an agent's tools config: %w", err))
		}
	}
}

var telemetryEnvNames = []string{"AIMEBU2_SENTRY_DSN", "AIMEBU2_SENTRY_ENV", "AIMEBU2_SENTRY_TRACES_SAMPLE_RATE"}

// updateAgentToolsTelemetry rewrites the telemetry variables of one config, keeping everything else in it (its key included), atomically.
func (c *Client) updateAgentToolsTelemetry(path string, env map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	servers, _ := config["mcpServers"].(map[string]any)
	entry, _ := servers["aimebu"].(map[string]any)
	current, _ := entry["env"].(map[string]any)
	if current == nil {
		return nil
	}
	for _, name := range telemetryEnvNames {
		delete(current, name)
	}
	for name, value := range env {
		current[name] = value
	}
	data, err = json.Marshal(config)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-tools-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (c *Client) writeAgentToolsConfig(agentID, key string) error {
	if agentID == "" || len(key) != 47 || key[:4] != "aak_" {
		return fmt.Errorf("invalid agent tools credential")
	}
	command, err := os.Executable()
	if err != nil {
		return err
	}
	path := c.agentToolsPath(agentID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	env := map[string]string{"AIMEBU2_URL": c.ServerURL, "AIMEBU2_AGENT_ID": agentID, "AIMEBU2_AGENT_KEY": key}
	if tlsclient.InsecureSkipVerify() {
		env["AIMEBU2_INSECURE_SKIP_VERIFY"] = "1"
	}
	c.mu.Lock()
	for name, value := range c.telemetryEnv {
		env[name] = value
	}
	c.mu.Unlock()
	config := map[string]any{"mcpServers": map[string]any{"aimebu": map[string]any{
		"command": command, "args": []string{"agent-tools"}, "env": env,
	}}}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-tools-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
