package vibeconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const listed = `# a person's config
active_model = "fast"
enable_telemetry = true

[[providers]]
name = "acme"
api_base = "https://llm.example.test/v1"
api_key_env_var = "ACME_API_KEY"   # the name, never the key

[providers.extra_headers]
"X-Team" = "docs"

[[models]]
name = "devstral-small"
provider = "mistral"
alias = "fast"
display_name = "Devstral Small"
input_price = 0.1
output_price = 0.3
allowed = [
  "a",
  "[not a header]",
]

[[models]]
name = "acme-large"
provider = "acme"   # no alias: the name is the alias

[mcp_servers_note]
x = 1

[[mcp_servers]]
name = "grafana"
transport = "stdio"
command = "uvx"

[[models]]
name = "after-mcp"
alias = "late"
`

func TestParsesArraysOfTablesWithTheirSubTables(t *testing.T) {
	blocks := Parse(listed)
	var kinds []string
	for _, block := range blocks {
		kinds = append(kinds, block.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"providers", "models", "models", "mcp_servers", "models"}) {
		t.Fatalf("kinds=%v: only models, providers and MCP servers are kept, in file order", kinds)
	}
	if !strings.Contains(strings.Join(blocks[0].Lines, "\n"), `"X-Team" = "docs"`) {
		t.Fatalf("a provider keeps its sub-table: %q", blocks[0].Lines)
	}
	if blocks[1].Fields["display_name"] != "Devstral Small" || blocks[1].Fields["input_price"] != "0.1" {
		t.Fatalf("fields=%v", blocks[1].Fields)
	}
	if !strings.Contains(strings.Join(blocks[1].Lines, "\n"), `"[not a header]"`) {
		t.Fatalf("a line of a multi-line array is not a header: %q", blocks[1].Lines)
	}
	if got := ProviderEnv(blocks); !reflect.DeepEqual(got, []string{"ACME_API_KEY"}) {
		t.Fatalf("provider env=%v", got)
	}
	var aliases []string
	for _, model := range Models(blocks) {
		aliases = append(aliases, model.Alias)
	}
	if !reflect.DeepEqual(aliases, []string{"fast", "acme-large", "late"}) {
		t.Fatalf("aliases=%v", aliases)
	}
	for _, block := range blocks {
		if strings.Contains(strings.Join(block.Lines, "\n"), "mcp_servers_note") {
			t.Fatalf("a table that only starts like a kept one was copied: %q", block.Lines)
		}
	}
	if got := MCPEnv(Parse("[[mcp_servers]]\nname = \"docs\"\napi_key_env = \"DOCS_KEY\"\n[[mcp_servers]]\nname = \"aimebu\"\napi_key_env = \"V1_KEY\"\n")); !reflect.DeepEqual(got, []string{"DOCS_KEY", "V1_KEY"}) {
		t.Fatalf("mcp env=%v", got)
	}
}

func TestWithoutMCPServerDropsOnlyThatServer(t *testing.T) {
	array := Parse("[[mcp_servers]]\nname = \"aimebu\"\nurl = \"http://v1\"\n[mcp_servers.headers]\nX = \"1\"\n\n[[mcp_servers]]\nname = \"docs\"\nurl = \"http://docs\"\n")
	if out := Render(WithoutMCPServer(array, "aimebu")); strings.Contains(out, "aimebu") || strings.Contains(out, "http://v1") || !strings.Contains(out, `name = "docs"`) {
		t.Fatalf("only the aimebu entry goes:\n%s", out)
	}
	alias := Parse("[mcp_servers.aimebu]\nurl = \"http://alias\"\n\n[mcp_servers.docs]\nurl = \"http://docs\"\n")
	if out := Render(WithoutMCPServer(alias, "aimebu")); strings.Contains(out, "http://alias") || !strings.Contains(out, "http://docs") {
		t.Fatalf("the alias-map form too:\n%s", out)
	}
}

func TestParsesTheAliasMapForm(t *testing.T) {
	blocks := Parse("[models.big]\nname = \"m-big\"\nprovider = \"mistral\"\n\n[models.\"odd.alias\"]\nname = \"m-odd\"\n\n[models.big.extra]\nk = 1\n")
	models := Models(blocks)
	if len(models) != 2 || models[0].Alias != "big" || models[1].Alias != "odd.alias" || models[0].Name != "m-big" {
		t.Fatalf("models=%+v", models)
	}
	if !strings.Contains(strings.Join(blocks[0].Lines, "\n"), "[models.big.extra]") {
		t.Fatalf("a deeper table stays with its alias: %q", blocks[0].Lines)
	}
}

func TestRenderRoundTripsTheTablesAndNothingElse(t *testing.T) {
	out := Render(Parse(listed))
	if strings.Contains(out, "active_model") || strings.Contains(out, "mcp_servers_note") || !strings.Contains(out, "acme-large") || !strings.Contains(out, "grafana") {
		t.Fatalf("render=%q", out)
	}
	again := Parse(out)
	if len(again) != 5 {
		t.Fatalf("a rendered config parses back to the same tables: %d", len(again))
	}
}

func TestReadOfAMissingFileHasNoBlocks(t *testing.T) {
	if blocks, err := Read(filepath.Join(t.TempDir(), "absent.toml")); err != nil || blocks != nil {
		t.Fatalf("blocks=%v err=%v", blocks, err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(listed), 0600); err != nil {
		t.Fatal(err)
	}
	if blocks, err := Read(path); err != nil || len(blocks) != 5 {
		t.Fatalf("blocks=%d err=%v", len(blocks), err)
	}
}

func TestAnUnterminatedStringDoesNotSwallowTheFile(t *testing.T) {
	blocks := Parse("[[models]]\nname = \"a\"\nnote = \"\"\"open\nstill open\n\n[[models]]\nname = \"b\"\n")
	if len(blocks) != 1 || blocks[0].Fields["name"] != "a" {
		t.Fatalf("an unclosed multi-line string stays inside its own table: %+v", blocks)
	}
}

func TestOnlyRemoteServersWithAUrlAContainerCanUseAreKept(t *testing.T) {
	blocks := Parse(`[[mcp_servers]]
name = "ok"
transport = "http"
url = "https://docs.example/mcp"

[[mcp_servers]]
name = "self"
transport = "http"
url = "http://127.0.0.1:3000/mcp"

[[mcp_servers]]
name = "query"
transport = "sse"
url = "https://docs.example/mcp?token=a"

[[mcp_servers]]
name = "cmd"
transport = "stdio"
command = "tool"
`)
	kept, dropped := WithOnlyRemoteMCPServers(blocks)
	if len(kept) != 1 || kept[0].Fields["name"] != "ok" || len(dropped) != 3 {
		t.Fatalf("kept = %v dropped = %v", kept, dropped)
	}
}
