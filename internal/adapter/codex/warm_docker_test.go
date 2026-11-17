package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

func TestWarmDockerDisablesUncarriedHostMCPServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	spec := adapter.TurnSpec{LauncherCodexHome: home}
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("[mcp_servers.local]\ncommand = 'true'\n[mcp_servers.remote]\nurl = 'https://example.com/mcp'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, overrides, err := warmDockerConfig(spec)
	if err != nil || len(config) == 0 {
		t.Fatalf("Docker config %v", err)
	}
	all := strings.Join(overrides, "\n")
	if !strings.Contains(all, "mcp_servers.local.enabled=false") || !strings.Contains(all, "mcp_servers.remote={url=") {
		t.Fatalf("MCP overrides: %q", all)
	}
	if err := os.WriteFile(path, []byte("[mcp_servers.'has.dot']\ncommand = 'true'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := warmDockerConfig(spec); !errors.Is(err, adapter.ErrNoWarm) {
		t.Fatalf("unsafe name was allowed: %v", err)
	}
}

func TestWarmDockerDeclinesHostTurnOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	spec := adapter.TurnSpec{LauncherCodexHome: home}
	path := filepath.Join(home, "config.toml")
	for key, config := range map[string]string{
		"model_provider":         `model_provider = "custom"`,
		"profile":                `profile = "private"`,
		"developer_instructions": `developer_instructions = "different instructions"`,
		"model_providers":        `[model_providers.custom]` + "\nname = 'Custom'\nbase_url = 'https://example.com'\n",
		"approval_policy":        `model = 'gpt-6-sol'` + "\napproval_policy = 'never'\n",
	} {
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := warmDockerConfig(spec)
		if !errors.Is(err, adapter.ErrNoWarm) || !strings.Contains(err.Error(), `host config key "`+key+`" overrides provider/profile`) {
			t.Fatalf("host override %q did not force cold mode: %v", config, err)
		}
	}
	for key, config := range map[string]string{
		"features": "[features]\nunverified_feature = true\n",
	} {
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := warmDockerConfig(spec)
		if !errors.Is(err, adapter.ErrNoWarm) || !strings.Contains(err.Error(), `host feature "unverified_feature" has no verified cold default`) {
			t.Fatalf("behavior key %q did not force cold with its own reason: %v", key, err)
		}
	}
	for _, config := range []string{
		"[plugins.'bad name@market']\nenabled = true\n",
		"[plugins.'has.dot@market']\nenabled = true\n",
		"[plugins._default]\nenabled = false\nunknown = true\n",
		"[plugins._default]\nunknown = true\n",
		"plugins = 'not a table'\n",
		"marketplaces = 'not a table'\n",
	} {
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := warmDockerConfig(spec); !errors.Is(err, adapter.ErrNoWarm) {
			t.Fatalf("unsafe plugin or malformed table allowed: %v", err)
		}
	}
	for _, key := range []string{"cli_auth_credentials_store", "mcp_oauth_credentials_store"} {
		for _, value := range []string{"auto", "keyring"} {
			if err := os.WriteFile(path, []byte(key+` = "`+value+`"`), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, err := warmDockerConfig(spec)
			if !errors.Is(err, adapter.ErrNoWarm) || !strings.Contains(err.Error(), `host config key "`+key+`" must be file`) {
				t.Fatalf("%s=%s: %v", key, value, err)
			}
		}
	}
	// Keys Codex writes itself, or that only touch the terminal UI, must not force every warm Docker turn cold.
	harmless := "model = 'gpt-6-sol'\nfile_opener = 'cursor'\ncli_auth_credentials_store = 'file'\nmcp_oauth_credentials_store = 'file'\n[projects.'/work/repo']\ntrust_level = 'trusted'\n[notice]\nhide_full_access_warning = true\n[tui]\nnotifications = true\n[history]\npersistence = 'none'\n[desktop]\n"
	if err := os.WriteFile(path, []byte(harmless), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := warmDockerConfig(spec); err != nil {
		t.Fatalf("trust, notice and UI settings should still allow warm mode: %v", err)
	}
	if err := os.WriteFile(path, []byte("model = 'gpt-6-sol'\n[mcp_servers.remote]\nurl = 'https://example.com/mcp'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := warmDockerConfig(spec); err != nil {
		t.Fatalf("model and safe MCP settings should still allow warm mode: %v", err)
	}
}

func TestWarmDockerNeutralizesOrdinaryHostConfig(t *testing.T) {
	home := t.TempDir()
	config := `model = "gpt-6-sol"
model_reasoning_effort = "high"
cli_auth_credentials_store = "file"
mcp_oauth_credentials_store = "file"

[features]
js_repl = true
plugins = false
remote_plugin = false

[desktop]
mac-menu-bar-enabled = true
git-pull-request-merge-method = "squash"
git-show-sidebar-pr-icons = true
[tui]
screen_reader_detection_done = true
[tui.model_availability_nux]
"gpt-5.5" = 1
"gpt-6-astra" = 2
"gpt-6.1-sol" = 3
[notice.model_migrations]
"gpt-5.5" = "gpt-6-sol"
[projects."/tmp/example-project"]
trust_level = "trusted"
[mcp_servers.local]
command = "true"
[mcp_servers.remote]
url = "https://example.invalid/mcp"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	_, overrides, err := warmDockerConfig(adapter.TurnSpec{LauncherCodexHome: home})
	if err != nil {
		t.Fatalf("ordinary host config ran cold: %v", err)
	}
	all := strings.Join(overrides, "\n")
	for _, want := range []string{"features.js_repl=false", "features.plugins=true", "features.remote_plugin=true", "mcp_servers.local.enabled=false", "mcp_servers.remote={url="} {
		if !strings.Contains(all, want) {
			t.Errorf("missing neutral override %q in %s", want, all)
		}
	}
	if strings.Contains(all, "personality=") || strings.Contains(all, "plugins._default") {
		t.Fatalf("warm overrides changed a cold default: %s", all)
	}
}

func TestWarmDockerKeepsPersonalityDefault(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	spec := adapter.TurnSpec{LauncherCodexHome: home}
	for _, config := range []string{"personality = 'none'\n"} {
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		var first string
		for i := 0; i < 20; i++ {
			_, _, err := warmDockerConfig(spec)
			if !errors.Is(err, adapter.ErrNoWarm) || !strings.Contains(err.Error(), "cannot be neutralized") {
				t.Fatalf("host config changed a cold default: %v", err)
			}
			if i == 0 {
				first = err.Error()
			} else if err.Error() != first {
				t.Fatalf("refusal order changed: first %q, later %q", first, err)
			}
		}
	}
}

func TestWarmDockerDisablesConfiguredPlugins(t *testing.T) {
	home := t.TempDir()
	config := `model = "gpt-6-sol"
model_reasoning_effort = "high"
cli_auth_credentials_store = "file"
mcp_oauth_credentials_store = "file"
[features]
js_repl = true
plugins = false
remote_plugin = false
[desktop]
mac_menu_bar_enabled = true
[tui]
notifications = true
[notice]
hide_full_access_warning = true
[projects."/tmp/example"]
trust_level = "trusted"
[mcp_servers.local]
command = "true"
[marketplaces.example-internal]
source_type = "local"
source = "/tmp/example-market"
[plugins."documents@openai-primary-runtime"]
enabled = true
[plugins."browser@openai-bundled"]
enabled = false
[plugins."example-architect@example-internal"]
enabled = true
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	_, overrides, err := warmDockerConfig(adapter.TurnSpec{LauncherCodexHome: home})
	if err != nil {
		t.Fatalf("ordinary plugin and marketplace config ran cold: %v", err)
	}
	joined := strings.Join(overrides, "\n")
	for _, id := range []string{"documents@openai-primary-runtime", "browser@openai-bundled", "example-architect@example-internal"} {
		want := `plugins.` + id + `.enabled=false`
		if !strings.Contains(joined, want) {
			t.Errorf("missing override %q in %s", want, joined)
		}
	}
	if !strings.Contains(joined, "features.plugins=true") {
		t.Fatalf("plugin feature changed from the cold default: %s", joined)
	}
}

func TestWarmDockerPluginOverridesReachCodexConfig(t *testing.T) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex CLI is not installed")
	}
	home := t.TempDir()
	ids := []string{"documents@openai-primary-runtime", "browser@openai-bundled", "example-architect@example-internal"}
	var config strings.Builder
	config.WriteString("[features]\nplugins = true\n")
	for _, id := range ids {
		config.WriteString("[plugins.\"" + id + "\"]\nenabled = true\n")
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config.String()), 0600); err != nil {
		t.Fatal(err)
	}
	_, overrides, err := warmDockerConfig(adapter.TurnSpec{LauncherCodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	args := make([]string, 0, len(overrides)*2+1)
	for _, override := range overrides {
		args = append(args, "-c", override)
	}
	args = append(args, "app-server")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "HOME="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if _, err := fmt.Fprint(stdin, "{\"id\":1,\"method\":\"initialize\",\"params\":{\"clientInfo\":{\"name\":\"probe\",\"title\":\"probe\",\"version\":\"1\"},\"capabilities\":{\"experimentalApi\":false}}}\n{\"method\":\"initialized\",\"params\":{}}\n{\"id\":2,\"method\":\"config/read\",\"params\":{}}\n"); err != nil {
		t.Fatal(err)
	}
	for scanner := bufio.NewScanner(stdout); scanner.Scan(); {
		line := scanner.Text()
		var frame struct {
			ID     int `json:"id"`
			Result struct {
				Config struct {
					Plugins map[string]struct {
						Enabled bool `json:"enabled"`
					} `json:"plugins"`
				} `json:"config"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(line), &frame) != nil || frame.ID != 2 {
			continue
		}
		if len(frame.Result.Config.Plugins) != len(ids) {
			t.Fatalf("Codex parsed phantom plugin keys: %#v", frame.Result.Config.Plugins)
		}
		for _, id := range ids {
			entry, ok := frame.Result.Config.Plugins[id]
			if !ok || entry.Enabled {
				t.Fatalf("Codex did not disable %q: %#v", id, frame.Result.Config.Plugins)
			}
		}
		return
	}
	t.Fatal("Codex app-server omitted config/read response")
}

func TestWarmDockerResetsPluginDefault(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[plugins._default]\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, overrides, err := warmDockerConfig(adapter.TurnSpec{LauncherCodexHome: home})
	if err != nil || !strings.Contains(strings.Join(overrides, "\n"), "plugins._default.enabled=true") {
		t.Fatalf("plugin default was not reset: %v, %q", err, overrides)
	}
}

func TestDockerWarmUsesOnlyReportedCodexHome(t *testing.T) {
	reported := t.TempDir()
	t.Setenv("CODEX_HOME", t.TempDir())
	auth := filepath.Join(reported, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"token","account_id":"account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewDocker()
	spec := adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "codex", Variant: "docker"}, LauncherCodexHome: reported}
	if got := a.WarmAuthPath(spec); got != auth {
		t.Fatalf("auth path = %q, want %q", got, auth)
	}
	if err := warmAuthAllowed(a.WarmAuthPath(spec), spec); err != nil {
		t.Fatalf("reported login: %v", err)
	}
	spec.LauncherCodexHome = ""
	if got := a.WarmAuthPath(spec); got != "" {
		t.Fatalf("missing report used %q", got)
	}
	if _, _, err := warmDockerConfig(spec); !errors.Is(err, adapter.ErrNoWarm) {
		t.Fatalf("missing report allowed warm config: %v", err)
	}
	spec.TurnID, spec.Agent.ID, spec.Agent.Model, spec.CheckoutPath, spec.Mode = "turn", "agent", "model", reported, adapter.ModeNormal
	if _, err := a.WarmFingerprint(spec); !errors.Is(err, adapter.ErrNoWarm) || !strings.Contains(err.Error(), "not reported") {
		t.Fatalf("missing report did not force cold with a reason: %v", err)
	}
}
