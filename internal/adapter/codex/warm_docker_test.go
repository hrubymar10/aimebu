package codex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		"[plugins.unknown]\nauto_provision = true\n",
		"[marketplaces.unknown]\nunverified = 'value'\n",
	} {
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := warmDockerConfig(spec); !errors.Is(err, adapter.ErrNoWarm) || !strings.Contains(err.Error(), "cannot be neutralized") {
			t.Fatalf("unknown plugin or marketplace field allowed: %v", err)
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
personality = "friendly"
cli_auth_credentials_store = "file"
mcp_oauth_credentials_store = "file"

[features]
js_repl = true

[plugins."documents@openai-primary-runtime"]
enabled = true
[plugins."spreadsheets@openai-primary-runtime"]
enabled = true
[plugins."presentations@openai-primary-runtime"]
enabled = true
[plugins."browser@openai-bundled"]
enabled = true
[plugins."example-architect@example-internal"]
enabled = true

[marketplaces.openai-bundled]
source_type = "local"
source = "/tmp/example-bundled"
last_updated = "2026-01-01T00:00:00Z"
[marketplaces.openai-primary-runtime]
source_type = "local"
source = "/tmp/example-primary"
last_updated = "2026-01-01T00:00:00Z"
[marketplaces.example-internal]
source_type = "git"
source = "https://example.invalid/repo.git"
last_revision = "abc123"

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
	for _, want := range []string{`personality="none"`, "features.js_repl=false", "features.plugins=false", "features.remote_plugin=false", "plugins._default.enabled=false", "mcp_servers.local.enabled=false", "mcp_servers.remote={url="} {
		if !strings.Contains(all, want) {
			t.Errorf("missing neutral override %q in %s", want, all)
		}
	}
	if strings.Contains(all, "example-internal") || strings.Contains(all, "example-bundled") {
		t.Fatalf("plugin or marketplace ID leaked into command line: %s", all)
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
