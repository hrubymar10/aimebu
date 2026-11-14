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
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("[mcp_servers.local]\ncommand = 'true'\n[mcp_servers.remote]\nurl = 'https://example.com/mcp'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, overrides, err := warmDockerConfig(adapter.TurnSpec{})
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
	if _, _, err := warmDockerConfig(adapter.TurnSpec{}); !errors.Is(err, adapter.ErrNoWarm) {
		t.Fatalf("unsafe name was allowed: %v", err)
	}
}

func TestWarmDockerDeclinesHostTurnOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "config.toml")
	for key, config := range map[string]string{
		"model_provider":         `model_provider = "custom"`,
		"profile":                `profile = "private"`,
		"model_reasoning_effort": `model_reasoning_effort = "low"`,
		"developer_instructions": `developer_instructions = "different instructions"`,
		"model_providers":        `[model_providers.custom]` + "\nname = 'Custom'\nbase_url = 'https://example.com'\n",
		"approval_policy":        `model = 'gpt-6-sol'` + "\napproval_policy = 'never'\n",
	} {
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := warmDockerConfig(adapter.TurnSpec{})
		if !errors.Is(err, adapter.ErrNoWarm) || !strings.Contains(err.Error(), `host config key "`+key+`" overrides provider/profile`) {
			t.Fatalf("host override %q did not force cold mode: %v", config, err)
		}
	}
	// Keys Codex writes itself, or that only touch the terminal UI, must not force every warm Docker turn cold.
	harmless := "model = 'gpt-6-sol'\nfile_opener = 'cursor'\n[projects.'/work/repo']\ntrust_level = 'trusted'\n[notice]\nhide_full_access_warning = true\n[tui]\nnotifications = true\n[history]\npersistence = 'none'\n"
	if err := os.WriteFile(path, []byte(harmless), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := warmDockerConfig(adapter.TurnSpec{}); err != nil {
		t.Fatalf("trust, notice and UI settings should still allow warm mode: %v", err)
	}
	if err := os.WriteFile(path, []byte("model = 'gpt-6-sol'\n[mcp_servers.remote]\nurl = 'https://example.com/mcp'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := warmDockerConfig(adapter.TurnSpec{}); err != nil {
		t.Fatalf("model and safe MCP settings should still allow warm mode: %v", err)
	}
}
