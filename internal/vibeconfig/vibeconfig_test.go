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
	if !reflect.DeepEqual(kinds, []string{"providers", "models", "models", "models"}) {
		t.Fatalf("kinds=%v: only models and providers are kept, in file order", kinds)
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
		if strings.Contains(strings.Join(block.Lines, "\n"), "grafana") || strings.Contains(strings.Join(block.Lines, "\n"), "mcp_servers") {
			t.Fatalf("something that is not a model or provider was copied: %q", block.Lines)
		}
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
	if strings.Contains(out, "active_model") || strings.Contains(out, "grafana") || !strings.Contains(out, "acme-large") {
		t.Fatalf("render=%q", out)
	}
	again := Parse(out)
	if len(again) != 4 {
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
	if blocks, err := Read(path); err != nil || len(blocks) != 4 {
		t.Fatalf("blocks=%d err=%v", len(blocks), err)
	}
}

func TestAnUnterminatedStringDoesNotSwallowTheFile(t *testing.T) {
	blocks := Parse("[[models]]\nname = \"a\"\nnote = \"\"\"open\nstill open\n\n[[models]]\nname = \"b\"\n")
	if len(blocks) != 1 || blocks[0].Fields["name"] != "a" {
		t.Fatalf("an unclosed multi-line string stays inside its own table: %+v", blocks)
	}
}
