package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// Known settings outside the denylist can remain in the host config. In
// Codex 0.169, personality is deprecated: friendly and pragmatic no longer
// select a style. The model is supplied again at thread and turn start.
var warmDockerKnownAllowedKeys = map[string]bool{"model": true, "mcp_servers": true, "projects": true, "notice": true, "tui": true, "history": true, "file_opener": true, "desktop": true, "personality": true, "marketplaces": true}

var warmDockerDenyKeys = map[string]bool{
	"model_provider": true, "model_providers": true, "profile": true, "profiles": true, "chatgpt_base_url": true,
	"approval_policy": true, "sandbox_mode": true, "sandbox_workspace_write": true, "shell_environment_policy": true,
	"analytics": true, "approvals_reviewer": true, "browser_use": true, "client_search_provider": true,
	"compact_prompt": true, "computer_use": true, "developer_instructions": true, "forced_chatgpt_workspace_id": true,
	"forced_login_method": true, "instructions": true, "model_auto_compact_token_limit": true,
	"model_auto_compact_token_limit_scope": true, "model_context_window": true, "model_reasoning_summary": true,
	"model_verbosity": true, "review_model": true, "service_tier": true, "tools": true, "web_search": true,
	"notify": true, "otel": true, "hide_agent_reasoning": true, "instructions_file": true,
	"project_doc_fallback_filenames": true, "project_doc_max_bytes": true,
	"openai_base_url": true, "default_permissions": true, "permissions": true,
	"agents": true, "skills": true, "hooks": true,
	"include_environment_context": true, "include_apps_instructions": true,
	"model_catalog_json": true, "plan_mode_reasoning_effort": true,
	"show_raw_agent_reasoning": true, "tool_output_token_limit": true,
	"daybreak": true, "model_post_turn_compact_threshold_percent": true, "auto_review": true,
	"allow_login_shell": true, "allow_symlinked_codex_home": true,
	"include_permissions_instructions": true, "include_collaboration_mode_instructions": true,
	"include_environment_context_time": true, "model_instructions_file": true,
	"mcp_enterprise_managed_auth": true, "mcp_oauth_callback_port": true,
	"mcp_oauth_callback_url": true, "mcp_optional_startup_grace_ms": true,
	"background_terminal_max_timeout": true, "thread_unload_delay_secs": true,
	"js_repl_node_path": true, "js_repl_node_module_dirs": true,
	"rollout_trace": true, "sqlite_home": true, "log_dir": true,
	"apps_mcp_product_sku": true, "responses": true, "responses_api_metadata": true,
	"orchestrator": true, "cloud": true, "audio": true, "realtime": true,
}

var warmDockerUnknownSaid sync.Map // agent ID + key -> struct{}

func warmDockerUnknown(spec adapter.TurnSpec, key string) {
	if spec.StderrSink != nil {
		if _, seen := warmDockerUnknownSaid.LoadOrStore(spec.Agent.ID+"\x00"+key, struct{}{}); !seen {
			fmt.Fprintf(spec.StderrSink, "Codex Docker warm allowed unrecognized host config key %q\n", key)
		}
	}
	class := key
	if !bareKey.MatchString(class) {
		class = "other"
	}
	telemetry.CaptureWarning(context.Background(), "codex-warm-unknown:"+class,
		fmt.Errorf("Codex Docker warm allowed unrecognized host config key %q", class))
}

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
		return nil, nil, fmt.Errorf("%w: cannot read Codex host config", adapter.ErrNoWarm)
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("%w: invalid Codex host config", adapter.ErrNoWarm)
	}
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var neutral []string
	for _, key := range keys {
		if warmDockerKnownAllowedKeys[key] {
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
		default:
			if warmDockerDenyKeys[key] || strings.HasPrefix(key, "experimental_") || strings.HasSuffix(key, "_instructions_file") || strings.HasPrefix(key, "output_") || strings.HasPrefix(key, "response_") {
				return nil, nil, fmt.Errorf("%w: Codex host config key %q changes turn behavior or isolation", adapter.ErrNoWarm, key)
			}
			warmDockerUnknown(spec, key)
		}
	}
	overrides, skipped, err := dockerOverrides(string(data))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: Codex host MCP servers cannot be neutralized", adapter.ErrNoWarm)
	}
	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !bareKey.MatchString(name) || name == "aimebu_docker" {
			return nil, nil, fmt.Errorf("%w: Codex host MCP server name cannot be safely overridden", adapter.ErrNoWarm)
		}
		overrides = append(overrides, "mcp_servers."+name+".enabled=false")
	}
	overrides = append(overrides, neutral...)
	return data, overrides, nil
}
