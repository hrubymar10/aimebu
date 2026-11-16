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

// These settings do not change an agent turn. The model is supplied again at
// thread and turn start; MCP servers are handled by dockerOverrides below.
var warmDockerInertKeys = map[string]bool{"model": true, "mcp_servers": true, "projects": true, "notice": true, "tui": true, "history": true, "file_opener": true, "desktop": true}

// Codex may use the keyring for these stores, but a Docker warm process only
// has the mounted file credentials used by the cold exec path.
var warmDockerFileStores = map[string]bool{"cli_auth_credentials_store": true, "mcp_oauth_credentials_store": true}

// The one host feature known to appear in existing configs is removed in
// Codex 0.169 and defaults to false (`codex features list`). Other feature
// values still fail closed until their cold defaults are verified.
var warmDockerFeatureDefaults = map[string]bool{"js_repl": false}

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
	disablePlugins := false
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
			if _, ok := doc[key].(string); !ok {
				return nil, nil, fmt.Errorf("%w: Codex host config key %q is not a personality", adapter.ErrNoWarm, key)
			}
			neutral = append(neutral, `personality="none"`)
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
		case "plugins", "marketplaces":
			entries, ok := doc[key].(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%w: Codex host config key %q is not a table", adapter.ErrNoWarm, key)
			}
			for name, raw := range entries {
				fields, ok := raw.(map[string]any)
				if !ok {
					return nil, nil, fmt.Errorf("%w: Codex host %s entry %q is not a table", adapter.ErrNoWarm, key, name)
				}
				for field, value := range fields {
					if key == "plugins" {
						if field != "enabled" {
							return nil, nil, fmt.Errorf("%w: Codex host plugin field %q cannot be neutralized", adapter.ErrNoWarm, field)
						}
						if _, ok := value.(bool); !ok {
							return nil, nil, fmt.Errorf("%w: Codex host plugin enabled setting is not boolean", adapter.ErrNoWarm)
						}
					} else {
						if field != "source" && field != "source_type" && field != "last_updated" && field != "last_revision" && field != "ref" {
							return nil, nil, fmt.Errorf("%w: Codex host marketplace field %q cannot be neutralized", adapter.ErrNoWarm, field)
						}
						if _, ok := value.(string); !ok {
							return nil, nil, fmt.Errorf("%w: Codex host marketplace field %q is not a string", adapter.ErrNoWarm, field)
						}
					}
				}
			}
			disablePlugins = true
		default:
			return nil, nil, fmt.Errorf("%w: Codex host config key %q overrides provider/profile or other turn settings", adapter.ErrNoWarm, key)
		}
	}
	if disablePlugins {
		// A cold Docker exec ignores the user's plugin configuration. The
		// plugin manager's feature gate also prevents marketplace loading,
		// including entries whose IDs cannot be used in dotted -c paths.
		neutral = append(neutral, "features.plugins=false", "features.remote_plugin=false", "plugins._default.enabled=false")
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
