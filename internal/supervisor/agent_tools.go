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
