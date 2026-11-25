package supervisor

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/tlsclient"
)

// An agent's key lives in this process's memory and nowhere else: the server hands it over in a spawn (or, after a restart, in hello_ack), a turn
// gets its own agent's key and no other (adapter.AgentToolsAccess, which is never journaled), and it is dropped when the agent stops. No file holds
// it, so an agent that reads the supervisor's directories finds no key, its own or another's.

// legacyAgentToolsDir is where an older supervisor kept one config per agent, key included. Nothing writes it now; a start removes it.
func (c *Client) legacyAgentToolsDir() string {
	if c.Journal == nil || c.Journal.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.Journal.path), "agent-tools")
}

// SweepLegacyKeys removes what an older supervisor left on the disk that holds an agent's key: the agent-tools directory, and the generated
// configs of the adapters (a key in a Codex config.toml or a Pi mcp.json). It is called once when the supervisor starts, and skipped while another instance of the machine holds its journal lock.
func (c *Client) SweepLegacyKeys() {
	// The sweep is machine-wide (the runtime roots are shared), so it would delete the config a running turn of another instance still reads.
	if c.otherInstanceHeld() {
		c.logSweepSkipped()
		return
	}
	if dir := c.legacyAgentToolsDir(); dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			c.report("the legacy agent-tools directory could not be removed", err)
		}
	}
	if sweeper, ok := c.Adapter.(interface{ SweepKeys() }); ok {
		sweeper.SweepKeys()
	}
}

// SetAgentKey holds a key the server issued for an agent, as a spawn or hello_ack does: for a caller that embeds a supervisor and already has the
// key. Without one, a supervisor that says it takes keys is given a new key for every agent it should run, which replaces the key the caller has.
func (c *Client) SetAgentKey(agentID, key string) error { return c.setAgentKey(agentID, key) }

// setAgentKey holds the key of an agent in memory.
func (c *Client) setAgentKey(agentID, key string) error {
	if agentID == "" || len(key) != 47 || key[:4] != "aak_" {
		return errors.New("invalid agent tools credential")
	}
	c.mu.Lock()
	if c.agentKeys == nil {
		c.agentKeys = make(map[string]string)
	}
	changed := c.agentKeys[agentID] != "" && c.agentKeys[agentID] != key
	c.agentKeys[agentID] = key
	c.mu.Unlock()
	if changed {
		// A live process holds the key it started with: a key that is replaced (and so the old one revoked) ends it.
		c.retireWarm(agentID, "the agent's key changed")
	}
	return nil
}

// heldKeys is the agents whose key this process holds, as hello reports them.
func (c *Client) heldKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.agentKeys))
	for id := range c.agentKeys {
		out = append(out, id)
	}
	return out
}

// forgetAgentKey drops an agent's key once the agent is stopped, and deletes whatever generated config an adapter wrote for it. The server
// issues a new key whenever the agent starts again, so nothing a stopped agent leaves behind can authenticate, and nothing stays after it is
// removed.
func (c *Client) forgetAgentKey(agentID string) {
	if !c.keepAliveForget(agentID) || !c.retireWarm(agentID, "the agent's key is dropped") {
		return // a process of the agent may still hold the key: it stays until the process is confirmed gone
	}
	c.mu.Lock()
	delete(c.agentKeys, agentID)
	c.mu.Unlock()
	if forgetter, ok := c.Adapter.(interface{ ForgetAgent(string) }); ok {
		forgetter.ForgetAgent(agentID)
	}
}

// SetTelemetryEnv sets the environment the agent-tools bridge is started with for error and performance reporting (AIMEBU_SENTRY_DSN, ...; an empty
// map means none, and the bridge then reports nothing unless its own environment says otherwise). It reaches the bridge through the environment of
// its MCP entry, never its arguments, and applies to the next turn of any agent; a bridge that is already running keeps what it started with.
func (c *Client) SetTelemetryEnv(env map[string]string) {
	c.mu.Lock()
	c.telemetryEnv = env
	c.mu.Unlock()
}

// agentToolsAccess is what a turn of the agent needs to reach the bus, or nil when this process holds no key for it.
func (c *Client) agentToolsAccess(agentID string) (*adapter.AgentToolsAccess, error) {
	command, err := os.Executable()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	key := c.agentKeys[agentID]
	env := map[string]string{}
	for name, value := range c.telemetryEnv {
		env[name] = value
	}
	c.mu.Unlock()
	if key == "" {
		return nil, nil
	}
	if tlsclient.InsecureSkipVerify() {
		env["AIMEBU_INSECURE_SKIP_VERIFY"] = "1"
	}
	return &adapter.AgentToolsAccess{AgentID: agentID, ServerURL: c.ServerURL, Key: key, Command: command, Env: env}, nil
}
