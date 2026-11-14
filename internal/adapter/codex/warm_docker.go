package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// warmDockerInertKeys are the host config keys a warm Docker turn may carry besides model and mcp_servers: Codex writes
// the trust and notice tables itself, and the rest only change the terminal UI, history or editor links. None of them
// selects a provider, profile, instructions or model setting, so they cannot change the turn's endpoint or behavior.
var warmDockerInertKeys = map[string]bool{"model": true, "mcp_servers": true, "projects": true, "notice": true, "tui": true, "history": true, "file_opener": true}

// warmDockerConfig closes the gap left by app-server having no exec's
// --ignore-user-config flag. Every host MCP server the cold Docker path would
// skip is disabled before initialization. A name that cannot be overridden
// safely, or invalid TOML, keeps this turn on the cold path.
func warmDockerConfig(spec adapter.TurnSpec) ([]byte, []string, error) {
	path := filepath.Join(filepath.Dir(hostCodexAuthPath(spec)), "config.toml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, adapter.ErrNoWarm
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, nil, adapter.ErrNoWarm
	}
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !warmDockerInertKeys[key] {
			return nil, nil, fmt.Errorf("%w: Codex host config key %q overrides provider/profile or other turn settings", adapter.ErrNoWarm, key)
		}
	}
	overrides, skipped, err := dockerOverrides(string(data))
	if err != nil {
		return nil, nil, adapter.ErrNoWarm
	}
	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !bareKey.MatchString(name) || name == "aimebu_docker" {
			return nil, nil, adapter.ErrNoWarm
		}
		overrides = append(overrides, "mcp_servers."+name+".enabled=false")
	}
	return data, overrides, nil
}
