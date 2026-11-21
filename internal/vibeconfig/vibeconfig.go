// Package vibeconfig reads the kinds of table of a Mistral Vibe config.toml that aimebu needs, the models and the providers they
// belong to and the MCP servers, without a TOML library. Vibe has no command that lists its models, so the supervisor offers the ones the
// person's config defines, and the Vibe adapter copies those tables into the private home each agent runs in (the rest of the person's
// config, their hooks and skills among it, never reaches an agent).
//
// The reader is line-based and deliberately small: it understands `[[models]]` / `[[providers]]` arrays of tables and the
// `[models.alias]` alias-map form, with their sub-tables, comments, and values spread over several lines; it reads only single-line
// scalar values for itself and copies every other line through untouched. A table written as an inline value (`models = [...]`) is not
// seen.
package vibeconfig

import (
	"os"
	"regexp"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// Block is one table of the file, kept as written: its header, every line up to the next table, and the sub-tables that follow it.
type Block struct {
	Kind   string            // "models", "providers" or "mcp_servers"
	Alias  string            // the alias of a [models.alias] table; empty for an entry of a [[models]] array
	Lines  []string          // the raw lines, header first
	Fields map[string]string // the table's own single-line scalar values, unquoted
}

// Model is a model the config defines, as Vibe is started with it: the alias is what `VIBE_ACTIVE_MODEL` takes.
type Model struct {
	Alias       string
	Name        string
	DisplayName string
	Provider    string
	InputPrice  string
	OutputPrice string
}

// Read parses the file at path; a file that does not exist has no blocks.
func Read(path string) ([]Block, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return Parse(string(data)), nil
}

var headerPattern = regexp.MustCompile(`^\[\[?\s*([^\[\]]+?)\s*\]\]?\s*(?:#.*)?$`)

// Parse returns the models and providers tables of a config.toml, in file order.
func Parse(text string) []Block {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	type header struct {
		index int
		array bool
		path  []string
	}
	var headers []header
	depth, multi := 0, ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if depth == 0 && multi == "" {
			if match := headerPattern.FindStringSubmatch(trimmed); match != nil && strings.HasPrefix(trimmed, "[") {
				headers = append(headers, header{index: i, array: strings.HasPrefix(trimmed, "[["), path: splitKey(match[1])})
				continue
			}
		}
		depth, multi = scanValue(line, depth, multi)
	}
	// A key that is an array of tables anywhere makes its dotted headers sub-tables of the last entry; otherwise each [key.alias] is one entry.
	listForm := map[string]bool{}
	for _, h := range headers {
		if h.array && len(h.path) == 1 {
			listForm[h.path[0]] = true
		}
	}
	var blocks []Block
	var current *Block
	for n, h := range headers {
		end := len(lines)
		if n+1 < len(headers) {
			end = headers[n+1].index
		}
		kind := ""
		if len(h.path) > 0 && (h.path[0] == "models" || h.path[0] == "providers" || h.path[0] == "mcp_servers") {
			kind = h.path[0]
		}
		switch {
		case kind == "":
			current = nil
		case h.array && len(h.path) == 1, !listForm[kind] && len(h.path) == 2 && !h.array:
			block := Block{Kind: kind, Fields: map[string]string{}}
			if len(h.path) == 2 {
				block.Alias = h.path[1]
			}
			blocks = append(blocks, block)
			current = &blocks[len(blocks)-1]
			current.Lines = append(current.Lines, lines[h.index:end]...)
			readFields(current.Fields, lines[h.index+1:end])
		case !listForm[kind] && len(h.path) > 2:
			// A deeper table of an alias-map entry belongs to that alias, wherever it is written.
			current = nil
			for i := range blocks {
				if blocks[i].Kind == kind && blocks[i].Alias == h.path[1] {
					current = &blocks[i]
				}
			}
			if current != nil {
				current.Lines = append(current.Lines, lines[h.index:end]...)
			}
		default:
			// A sub-table or a nested entry of the block before it.
			if current != nil && current.Kind == kind {
				current.Lines = append(current.Lines, lines[h.index:end]...)
			} else {
				current = nil
			}
		}
	}
	for i := range blocks {
		for len(blocks[i].Lines) > 0 && strings.TrimSpace(blocks[i].Lines[len(blocks[i].Lines)-1]) == "" {
			blocks[i].Lines = blocks[i].Lines[:len(blocks[i].Lines)-1]
		}
	}
	return blocks
}

// Models lists the models the blocks define.
func Models(blocks []Block) []Model {
	var out []Model
	for _, block := range blocks {
		if block.Kind != "models" {
			continue
		}
		model := Model{Alias: block.Fields["alias"], Name: block.Fields["name"], DisplayName: block.Fields["display_name"],
			Provider: block.Fields["provider"], InputPrice: block.Fields["input_price"], OutputPrice: block.Fields["output_price"]}
		if model.Alias == "" {
			model.Alias = block.Alias
		}
		if model.Alias == "" {
			model.Alias = model.Name
		}
		if model.Alias == "" {
			continue
		}
		out = append(out, model)
	}
	return out
}

// ProviderBases maps each provider's name to its api_base, for the providers that give one.
func ProviderBases(blocks []Block) map[string]string {
	out := map[string]string{}
	for _, block := range blocks {
		if block.Kind == "providers" && block.Fields["name"] != "" && block.Fields["api_base"] != "" {
			out[block.Fields["name"]] = block.Fields["api_base"]
		}
	}
	return out
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ProviderEnv names the environment variables the providers read their key from, so only those reach an agent.
func ProviderEnv(blocks []Block) []string {
	var names []string
	seen := map[string]bool{}
	for _, block := range blocks {
		if name := block.Fields["api_key_env_var"]; block.Kind == "providers" && envName.MatchString(name) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// WithoutMCPServer drops the MCP server entries called name, and keeps every other block.
func WithoutMCPServer(blocks []Block, name string) []Block {
	var out []Block
	for _, block := range blocks {
		if block.Kind == "mcp_servers" && (block.Fields["name"] == name || block.Alias == name) {
			continue
		}
		out = append(out, block)
	}
	return out
}

// WithoutMCPServers drops every MCP server entry.
func WithoutMCPServers(blocks []Block) []Block {
	var out []Block
	for _, block := range blocks {
		if block.Kind != "mcp_servers" {
			out = append(out, block)
		}
	}
	return out
}

// WithOnlyRemoteMCPServers keeps the MCP server entries that connect over the network (a streamable-http, http or sse transport with a url)
// and drops the ones that run a command, with the names of the dropped ones.
func WithOnlyRemoteMCPServers(blocks []Block) (kept []Block, dropped []string) {
	for _, block := range blocks {
		if block.Kind == "mcp_servers" {
			remote := false
			switch block.Fields["transport"] {
			case "http", "streamable-http", "sse":
				remote = block.Fields["url"] != ""
				if _, unsafe := adapter.UnsafeContainerURL(block.Fields["url"]); unsafe {
					remote = false
				}
			}
			if !remote {
				name := block.Fields["name"]
				if name == "" {
					name = block.Alias
				}
				dropped = append(dropped, name)
				continue
			}
		}
		kept = append(kept, block)
	}
	return kept, dropped
}

// MCPEnv names the environment variables the MCP servers read a token from (`api_key_env`), so only those reach an agent.
func MCPEnv(blocks []Block) []string {
	var names []string
	seen := map[string]bool{}
	for _, block := range blocks {
		if name := block.Fields["api_key_env"]; block.Kind == "mcp_servers" && envName.MatchString(name) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// Render writes the blocks back out, to seed another config.toml with them.
func Render(blocks []Block) string {
	var out strings.Builder
	for _, block := range blocks {
		out.WriteString(strings.Join(block.Lines, "\n"))
		out.WriteString("\n\n")
	}
	return out.String()
}

// scanValue follows one line of a value that can span lines (an array, a multi-line string), returning the bracket depth and the
// open multi-line string delimiter after it.
func scanValue(line string, depth int, multi string) (int, string) {
	for i := 0; i < len(line); i++ {
		if multi != "" {
			if strings.HasPrefix(line[i:], multi) {
				i += 2
				multi = ""
			}
			continue
		}
		switch c := line[i]; {
		case c == '#':
			return depth, multi
		case strings.HasPrefix(line[i:], `"""`), strings.HasPrefix(line[i:], `'''`):
			multi = line[i : i+3]
			i += 2
		case c == '"':
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' {
					i++
				}
			}
		case c == '\'':
			for i++; i < len(line) && line[i] != '\''; i++ {
			}
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth, multi
}

// readFields records the single-line `key = value` scalars of a table's own lines, which end at its first sub-table.
func readFields(fields map[string]string, lines []string) {
	depth, multi := 0, ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if depth == 0 && multi == "" && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if key, value, ok := strings.Cut(trimmed, "="); ok {
				if name := unquote(strings.TrimSpace(key)); name != "" && !strings.ContainsAny(name, " \t") {
					if scalar, ok := scalarValue(strings.TrimSpace(value)); ok {
						fields[name] = scalar
					}
				}
			}
		}
		depth, multi = scanValue(line, depth, multi)
	}
}

// scalarValue reads a string, number or boolean written on one line, without a trailing comment.
func scalarValue(value string) (string, bool) {
	if value == "" || strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") || strings.HasPrefix(value, `"""`) || strings.HasPrefix(value, `'''`) {
		return "", false
	}
	if value[0] == '"' || value[0] == '\'' {
		quote := value[0]
		for i := 1; i < len(value); i++ {
			switch {
			case quote == '"' && value[i] == '\\':
				i++
			case value[i] == quote:
				return unquote(value[:i+1]), true
			}
		}
		return "", false
	}
	if at := strings.Index(value, "#"); at >= 0 {
		value = value[:at]
	}
	return strings.TrimSpace(value), true
}

// unquote removes the quotes of a basic or literal string and the escapes of a basic one that matter for the values read here.
func unquote(value string) string {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		inner := value[1 : len(value)-1]
		return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t").Replace(inner)
	}
	return value
}

// splitKey splits a dotted key into its segments, honouring quotes.
func splitKey(key string) []string {
	var parts []string
	var current strings.Builder
	var quote byte
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				current.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '.':
			parts = append(parts, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteByte(c)
		}
	}
	return append(parts, strings.TrimSpace(current.String()))
}
