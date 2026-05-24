package config

import (
	"os"
	"path/filepath"
)

const (
	envConfigDir = "AIMEBU_CONFIG_DIR"
	defaultRoot  = ".aimebu"
)

var (
	serverLegacyEntries = []string{
		"schema.json",
		"rooms.json",
		"messages.json",
		"agents.json",
		"macros.json",
		"fleet.json",
		"settings.json",
		"sounds",
		"aimebu.pid",
		"aimebu.log",
	}
	agentLegacyEntries = []string{
		"agent-sessions.json",
		"agent-warning-acknowledged",
	}
)

// Root returns the config root from env or the default home directory.
func Root() string {
	root := os.Getenv(envConfigDir)
	if root != "" {
		return root
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, defaultRoot)
}

// ServerDir returns the server-owned data directory.
func ServerDir() string {
	return filepath.Join(Root(), "server")
}

// AgentsDir returns the agent-owned state directory.
func AgentsDir() string {
	return filepath.Join(Root(), "agents")
}
