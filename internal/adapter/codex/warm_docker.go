package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// These settings do not change an agent turn. The model is supplied again at
// thread and turn start; MCP servers are handled by dockerOverrides below.
var warmDockerInertKeys = map[string]bool{"model": true, "mcp_servers": true, "projects": true, "notice": true, "tui": true, "history": true, "file_opener": true, "desktop": true}

// Codex may use the keyring for these stores, but a Docker warm process only
// has the mounted file credentials used by the cold exec path.
var warmDockerFileStores = map[string]bool{"cli_auth_credentials_store": true, "mcp_oauth_credentials_store": true}

// These are the 0.169 cold defaults with user config ignored. Plugin features
// remain enabled even when no user plugin table is loaded.
var warmDockerFeatureDefaults = map[string]bool{"js_repl": false, "plugins": true, "remote_plugin": true}

// Codex's -c parser splits paths on dots and treats quotes as literal key
// characters. Only IDs with bare path segments can be disabled reliably.
var warmDockerPluginID = regexp.MustCompile(`^[A-Za-z0-9_-]+@[A-Za-z0-9_-]+$`)

// warmDockerConfig closes the gap left by app-server having no exec's
// --ignore-user-config flag. Every host MCP server the cold Docker path would
// skip is disabled before initialization. A name that cannot be overridden
// safely, or invalid TOML, keeps this turn on the cold path.
func warmDockerConfig(spec adapter.TurnSpec) ([]byte, []string, error) {
	if spec.LauncherCodexHome == "" || !filepath.IsAbs(spec.LauncherCodexHome) {
		return nil, nil, fmt.Errorf("%w: Docker Codex home was not reported by the launcher", adapter.ErrNoWarm)
	}
	path := filepath.Join(spec.LauncherCodexHome, "config.toml")
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
	var neutral []string
	for _, key := range keys {
		if warmDockerInertKeys[key] {
			continue
		}
		if warmDockerFileStores[key] {
			if value, ok := doc[key].(string); !ok || value != "file" {
				return nil, nil, fmt.Errorf("%w: Codex host config key %q must be file for Docker warm mode", adapter.ErrNoWarm, key)
			}
			continue
		}
		switch key {
		case "model_reasoning_effort":
			if _, ok := doc[key].(string); !ok {
				return nil, nil, fmt.Errorf("%w: Codex host config key %q is not a reasoning effort", adapter.ErrNoWarm, key)
			}
			// The app-server's model/list default is sent on every turn/start.
		case "personality":
			return nil, nil, fmt.Errorf("%w: Codex host config key %q cannot be neutralized to the cold default personality", adapter.ErrNoWarm, key)
		case "features":
			features, ok := doc[key].(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%w: Codex host config key %q is not a table", adapter.ErrNoWarm, key)
			}
			featureKeys := make([]string, 0, len(features))
			for name := range features {
				featureKeys = append(featureKeys, name)
			}
			sort.Strings(featureKeys)
			for _, name := range featureKeys {
				defaultValue, known := warmDockerFeatureDefaults[name]
				if !known || !bareKey.MatchString(name) {
					return nil, nil, fmt.Errorf("%w: Codex host feature %q has no verified cold default", adapter.ErrNoWarm, name)
				}
				if _, ok := features[name].(bool); !ok {
					return nil, nil, fmt.Errorf("%w: Codex host feature %q is not boolean", adapter.ErrNoWarm, name)
				}
				neutral = append(neutral, fmt.Sprintf("features.%s=%t", name, defaultValue))
			}
		case "plugins":
			plugins, ok := doc[key].(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%w: Codex host config key %q is not a table", adapter.ErrNoWarm, key)
			}
			ids := make([]string, 0, len(plugins))
			for id := range plugins {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				entry, ok := plugins[id].(map[string]any)
				if !ok {
					return nil, nil, fmt.Errorf("%w: Codex host plugin %q is not a table", adapter.ErrNoWarm, id)
				}
				if enabled, present := entry["enabled"]; present {
					if _, ok := enabled.(bool); !ok {
						return nil, nil, fmt.Errorf("%w: Codex host plugin %q has a non-boolean enabled value", adapter.ErrNoWarm, id)
					}
				}
				if id == "_default" {
					// Cold exec has no host default override. Other fields could
					// change plugin behavior and have no verified cold value.
					if _, present := entry["enabled"]; len(entry) != 1 || !present {
						return nil, nil, fmt.Errorf("%w: Codex host plugin default has unknown settings", adapter.ErrNoWarm)
					}
					neutral = append(neutral, "plugins._default.enabled=true")
					continue
				}
				if !warmDockerPluginID.MatchString(id) {
					return nil, nil, fmt.Errorf("%w: Codex host plugin ID %q cannot be safely overridden", adapter.ErrNoWarm, id)
				}
				neutral = append(neutral, "plugins."+id+".enabled=false")
			}
		case "marketplaces":
			// Marketplace entries only define sources. Runtime loading reads
			// configured plugin IDs, all of which are disabled above.
			if _, ok := doc[key].(map[string]any); !ok {
				return nil, nil, fmt.Errorf("%w: Codex host config key %q is not a table", adapter.ErrNoWarm, key)
			}
		default:
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
	overrides = append(overrides, neutral...)
	return data, overrides, nil
}
