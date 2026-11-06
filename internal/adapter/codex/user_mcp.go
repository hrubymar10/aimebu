package codex

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// A native Codex turn runs from a private CODEX_HOME, so the person's own config.toml (model, profile, approvals, trust) never applies. Their
// MCP servers are the exception: the tables of their config.toml that define a server are copied into the private config, next to aimebu's
// own entry. Codex merges same-named tables (a url entry and a command entry together are refused), so a v1 bus of aimebu's name is never
// copied: aimebu's entry is the only one under that name.
//
// Only the table form is read: `[mcp_servers.<name>]` and its sub-tables (`.env`, `.http_headers`, …). A `mcp_servers` table written as an
// inline table or as dotted keys at the top of the file is not.

var tableHeader = regexp.MustCompile(`^\s*\[(\[)?\s*([^\[\]]+?)\s*\](\])?\s*(#.*)?$`)

// serverTables groups the lines of every server table of a config.toml by server, in file order, without the section the config ends in.
type serverTables struct {
	order []string
	lines map[string][]string
}

func readServerTables(config string) serverTables {
	out := serverTables{lines: map[string][]string{}}
	current := ""
	for _, line := range strings.Split(config, "\n") {
		if match := tableHeader.FindStringSubmatch(line); match != nil {
			current = ""
			// The header is read in its normal spelling (whitespace around the dots and the brackets, quoting), so a sub-table spelled with spaces
			// still belongs to its server.
			if name, ok := serverName(normalizeKey(match[2])); ok {
				current = name
				if _, seen := out.lines[name]; !seen {
					out.order = append(out.order, name)
				}
			}
		}
		if current != "" {
			out.lines[current] = append(out.lines[current], line)
		}
	}
	return out
}

// serverName reads the server a table header names, from the header in its normal spelling (see normalizeKey): mcp_servers.<name>, with or
// without a sub-table after it.
func serverName(path string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(path), "mcp_servers.")
	if !ok || strings.TrimSpace(rest) == "" {
		return "", false
	}
	name, _, _ := strings.Cut(rest, ".")
	name = strings.TrimSpace(name)
	return name, name != ""
}

var (
	envVarsKey   = regexp.MustCompile(`(?s)\benv_vars\s*=\s*\[([^\]]*)\]`)
	tokenEnvKey  = regexp.MustCompile(`\bbearer_token_env_var\s*=\s*["']([^"']+)["']`)
	envHeaderKey = regexp.MustCompile(`^\s*[^=\s]+\s*=\s*["']([A-Za-z_][A-Za-z0-9_]*)["']\s*(#.*)?$`)
	quoted       = regexp.MustCompile(`["']([^"']+)["']`)
)

// userMCPServers returns the server tables of a Codex config.toml as written, except the servers skip names and the ones that are aimebu's own
// bus of v1 (see adapter.LooksLikeV1Bus), and the names of the environment variables those servers ask Codex to pass on or read a token from.
func userMCPServers(config, skip string) (tables string, envVars []string, dropped []string) {
	groups := readServerTables(config)
	var out strings.Builder
	for _, name := range groups.order {
		text := strings.Join(groups.lines[name], "\n")
		if name == skip || adapter.LooksLikeV1Bus(name, text) {
			dropped = append(dropped, name)
			continue
		}
		out.WriteString(text)
		out.WriteString("\n\n")
		envVars = append(envVars, referencedEnv(text)...)
	}
	return out.String(), envVars, dropped
}

// referencedEnv names the environment variables a server's tables refer to: `env_vars = ["A", "B"]` (on one line or several),
// `bearer_token_env_var = "A"`, and the values of an `env_http_headers` table, which map a header to a variable.
func referencedEnv(text string) []string {
	var names []string
	for _, match := range envVarsKey.FindAllStringSubmatch(text, -1) {
		for _, item := range quoted.FindAllStringSubmatch(match[1], -1) {
			names = append(names, item[1])
		}
	}
	for _, match := range tokenEnvKey.FindAllStringSubmatch(text, -1) {
		names = append(names, match[1])
	}
	in := false
	for _, line := range strings.Split(text, "\n") {
		if header := tableHeader.FindStringSubmatch(line); header != nil {
			in = strings.HasSuffix(strings.TrimSpace(header[2]), ".env_http_headers")
			continue
		}
		if in {
			if match := envHeaderKey.FindStringSubmatch(line); match != nil {
				names = append(names, match[1])
			}
		}
	}
	return names
}

// normalizeKey spells a dotted key the one way: each part trimmed and unquoted ("docs" and docs are the same table).
func normalizeKey(path string) string {
	var parts []string
	var part strings.Builder
	var quote byte
	flush := func() {
		parts = append(parts, strings.TrimSpace(part.String()))
		part.Reset()
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				part.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '.':
			flush()
		default:
			part.WriteByte(c)
		}
	}
	flush()
	return strings.Join(parts, ".")
}

// wellFormed is a structural check of copied tables, since there is no TOML parser to ask: no table header twice, and no string or bracket left
// open at the end of the text. A config that fails it would make Codex refuse every turn, so the copy is dropped instead.
func wellFormed(tables string) bool {
	seen := map[string]bool{}
	depth, inString, multi := 0, byte(0), ""
	for _, line := range strings.Split(tables, "\n") {
		if depth == 0 && inString == 0 && multi == "" {
			if match := tableHeader.FindStringSubmatch(line); match != nil {
				// An array of tables ([[x]]) repeats its header by design.
				if match[1] == "" {
					key := normalizeKey(match[2])
					if seen[key] {
						return false
					}
					seen[key] = true
				}
				continue
			}
		}
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case multi != "":
				if strings.HasPrefix(line[i:], multi) {
					i += 2
					multi = ""
				}
			case inString != 0:
				if c == '\\' && inString == '"' {
					i++
				} else if c == inString {
					inString = 0
				}
			case c == '#':
				i = len(line)
			case strings.HasPrefix(line[i:], `"""`), strings.HasPrefix(line[i:], `'''`):
				multi = line[i : i+3]
				i += 2
			case c == '"' || c == '\'':
				inString = c
			case c == '[' || c == '{':
				depth++
			case c == ']' || c == '}':
				depth--
				if depth < 0 {
					return false
				}
			}
		}
		if inString != 0 { // a basic or literal string ends on its own line
			return false
		}
	}
	return depth == 0 && multi == ""
}

// userMCP is what a native turn copies from the person's Codex config: the tables to add to the private config, and the variable names to pass.
type userMCP struct {
	tables  string
	envVars []string
}

// readUserMCP reads the person's Codex config next to their auth file once. A missing config has none; one that cannot be read, or whose
// copied tables are not well formed, is left out with a line in the log (the path, never the content), and the turn goes on without them.
func readUserMCP(authPath string) userMCP {
	if authPath == "" {
		return userMCP{}
	}
	path := filepath.Join(filepath.Dir(authPath), "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("codex: the person's MCP servers in %s are not carried: %v", path, err)
		}
		return userMCP{}
	}
	tables, envVars, dropped := userMCPServers(string(data), "aimebu")
	for _, name := range dropped {
		log.Printf("codex: the MCP server %q of %s is not carried: it is aimebu's own bus of v1", name, path)
	}
	if tables == "" {
		return userMCP{}
	}
	if !wellFormed(tables) {
		log.Printf("codex: the MCP servers of %s are not carried: their tables are not well formed", path)
		return userMCP{}
	}
	sort.Strings(envVars)
	return userMCP{tables: tables, envVars: adapter.UserEnvNames(envVars)}
}

// A Docker turn cannot run a command that lives on the host, has no way to hand the container a variable the person's server names, and passes
// what it configures on a command line that any process of the host can read, so it carries only the person's plain remote servers: an entry with
// a url and nothing else that matters. Each goes to Codex as a -c override of its own table (the container's own config.toml is ignored, see
// --ignore-user-config), under the same filters as a native turn. An entry the override cannot reproduce exactly is left out, never carried in a
// weaker form: one that is switched off (it would be switched on), one with a setting the override does not carry (tool lists, required, a
// token variable, headers of any spelling: a literal header is a credential, which would sit in the process list), one that points at the host
// itself (the container's localhost is not the host), and one whose name would replace aimebu's own entry.

var (
	keyValue  = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+|"[^"]+"|'[^']+')\s*=\s*(.+?)\s*$`)
	bareKey   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	stringVal = regexp.MustCompile(`^("(?:[^"\\]|\\(?:[btnfr"\\]|u[0-9A-Fa-f]{4}|U[0-9A-Fa-f]{8}))*"|'[^']*')\s*(#.*)?$`)
	numberVal = regexp.MustCompile(`^(-?[0-9]+(?:\.[0-9]+)?)\s*(#.*)?$`)
)

// carriedKeys are the settings of a server table that the -c override reproduces.
var carriedKeys = map[string]bool{"url": true, "startup_timeout_sec": true, "tool_timeout_sec": true}

// dockerOverrides returns the `-c` values, `mcp_servers.<name>={url=…,…}`, of the remote servers in a Codex config.toml that a container can use,
// and for each server it leaves out the reason, in words for the log. It fails closed: a server table is carried only when every line of it is
// one the override reproduces verbatim (its own header, `url`, `enabled = true`, the two timeouts, blank lines and comments). Any other line, a
// dotted key, a header table of any kind, an inline table, a continuation of a longer value, a key it does not know, means the server is left out.
func dockerOverrides(config string) (overrides []string, skipped map[string]string) {
	skipped = map[string]string{}
	groups := readServerTables(config)
	for _, name := range groups.order {
		text := strings.Join(groups.lines[name], "\n")
		if name == "aimebu" || adapter.LooksLikeV1Bus(name, text) {
			skipped[name] = "it is aimebu's own bus of v1"
			continue
		}
		if name == "aimebu_docker" {
			skipped[name] = "its name is the one aimebu's own entry has in a container"
			continue
		}
		values, reason := plainServer(name, groups.lines[name])
		if reason != "" {
			skipped[name] = reason
			continue
		}
		overrides = append(overrides, "mcp_servers."+name+"={url="+values["url"]+values["timeouts"]+"}")
	}
	return overrides, skipped
}

// plainServer reads a server table that is nothing but a url, and optionally enabled = true and the two timeouts. It returns the url as written
// (a TOML string) and the timeouts as an override fragment, or why the server is not plain.
func plainServer(name string, lines []string) (values map[string]string, reason string) {
	if !bareKey.MatchString(name) {
		// Codex reads a quoted segment of a -c key with its quotes, so the server would get another name.
		return nil, "its name cannot be given on a command line"
	}
	top := map[string]string{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if header := tableHeader.FindStringSubmatch(line); header != nil {
			if i != 0 || normalizeKey(header[2]) != "mcp_servers."+name || header[1] != "" {
				return nil, "it has other tables (headers, variables, tools) that the container cannot be given safely"
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		kv := keyValue.FindStringSubmatch(line)
		if kv == nil {
			return nil, "it has a line the container override cannot reproduce (" + shorten(trimmed) + ")"
		}
		key := kv[1]
		if !carriedKeys[key] && key != "enabled" {
			return nil, "it has a setting the container override cannot carry (" + key + ")"
		}
		if _, dup := top[key]; dup {
			return nil, "it sets " + key + " twice"
		}
		top[key] = kv[2]
	}
	if v, ok := top["enabled"]; ok && strings.TrimSpace(strings.SplitN(v, "#", 2)[0]) != "true" {
		return nil, "it is switched off"
	}
	if top["url"] == "" {
		return nil, "it has no url (it runs a command on the host, which is not in the container)"
	}
	url := stringVal.FindStringSubmatch(top["url"])
	if url == nil {
		return nil, "its url is not a plain string"
	}
	if why, unsafe := adapter.UnsafeContainerURL(strings.Trim(url[1], `"'`)); unsafe {
		return nil, why
	}
	values = map[string]string{"url": url[1]}
	for _, key := range []string{"startup_timeout_sec", "tool_timeout_sec"} {
		if v, ok := top[key]; ok {
			match := numberVal.FindStringSubmatch(v)
			if match == nil {
				return nil, "its " + key + " is not a plain number"
			}
			values["timeouts"] += "," + key + "=" + match[1]
		}
	}
	return values, ""
}

// shorten cuts a line down for a log message.
func shorten(line string) string {
	if len(line) > 40 {
		return line[:40] + "…"
	}
	return line
}

// userDockerOverrides reads the person's Codex config on the host (the same file a native turn reads) for a Docker turn.
func userDockerOverrides() []string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(userHome, ".codex")
	}
	path := filepath.Join(home, "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("codex: the person's MCP servers in %s are not carried to the container: %v", path, err)
		}
		return nil
	}
	overrides, skipped := dockerOverrides(string(data))
	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		log.Printf("codex: the MCP server %q of %s is not carried to the container: %s", name, path, skipped[name])
	}
	return overrides
}
