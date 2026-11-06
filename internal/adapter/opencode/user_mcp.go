package opencode

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

// A native OpenCode turn runs with a private XDG_CONFIG_HOME and an inline config, so the person's own global config never applies. Their MCP
// servers are the exception: the `mcp` entries of their global opencode.jsonc / opencode.json are copied into the inline config, next to
// aimebu's own entry, which always replaces an entry of that name (a v1 bus, say) instead of being merged with it (OpenCode deep-merges
// configs, so a leftover key of the other entry would otherwise survive). Project config stays off (OPENCODE_DISABLE_PROJECT_CONFIG).

// userConfigDir is where OpenCode reads the person's global config: $XDG_CONFIG_HOME/opencode, else ~/.config/opencode.
func userConfigDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "opencode")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "opencode")
	}
	return ""
}

var (
	envRef  = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)
	fileRef = regexp.MustCompile(`\{file:`)
)

// userMCPEntries returns the person's MCP entries, without the one called skip and the ones that are aimebu's own bus of v1 (see
// adapter.LooksLikeV1Bus), and the names of the environment variables they refer to as {env:NAME} (OpenCode fills those in from its environment,
// which a private turn only has for names that are passed on). An entry that reads a file ({file:…}) is left out: the path is relative to a
// config directory that this turn does not have. A config that cannot be read is left out with a line in the log (the path, never the content).
func userMCPEntries(dir, skip string) (entries map[string]json.RawMessage, envVars []string) {
	if dir == "" {
		return nil, nil
	}
	entries = map[string]json.RawMessage{}
	for _, name := range []string{"opencode.jsonc", "opencode.json"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("opencode: the person's MCP servers in %s are not carried: %v", path, err)
			}
			continue
		}
		var parsed struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(stripJSONC(data), &parsed) != nil {
			log.Printf("opencode: the person's MCP servers in %s are not carried: the file is not valid JSON", path)
			continue
		}
		for server, entry := range parsed.MCP {
			if server != skip {
				entries[server] = entry
			}
		}
	}
	seen := map[string]bool{}
	for server, entry := range entries {
		if adapter.LooksLikeV1Bus(server, string(entry)) {
			log.Printf("opencode: the MCP server %q is not carried: it is aimebu's own bus of v1", server)
			delete(entries, server)
			continue
		}
		if fileRef.Match(entry) {
			delete(entries, server)
			continue
		}
		for _, match := range envRef.FindAllSubmatch(entry, -1) {
			if name := string(match[1]); !seen[name] {
				seen[name] = true
				envVars = append(envVars, name)
			}
		}
	}
	sort.Strings(envVars)
	return entries, adapter.UserEnvNames(envVars)
}

// stripJSONC makes the text of an opencode.jsonc plain JSON: it drops // and /* */ comments and the commas before a closing bracket, and leaves
// everything inside a string as it is.
func stripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case inString:
			out = append(out, c)
			if c == '\\' && i+1 < len(data) {
				i++
				out = append(out, data[i])
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			i++
		default:
			out = append(out, c)
		}
	}
	return dropTrailingCommas(out)
}

func dropTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(data) {
				i++
				out = append(out, data[i])
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
		}
		if c == ',' {
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// remoteOnly keeps the remote entries of the person's servers, for a container: an entry that runs a command (type "local") points at a binary of
// the host, which is not in the container, and is left out with a line in the log. It also returns the {env:NAME} variables the kept ones name.
func remoteOnly(entries map[string]json.RawMessage) (map[string]json.RawMessage, []string) {
	kept := map[string]json.RawMessage{}
	seen := map[string]bool{}
	var envVars []string
	for name, entry := range entries {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(entry, &kind) != nil || kind.Type != "remote" {
			log.Printf("opencode: the MCP server %q is not carried to the container: a command on the host is not in the container", name)
			continue
		}
		var remote struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(entry, &remote)
		if why, unsafe := adapter.UnsafeContainerURL(remote.URL); unsafe {
			log.Printf("opencode: the MCP server %q is not carried to the container: %s", name, why)
			continue
		}
		kept[name] = entry
		for _, match := range envRef.FindAllSubmatch(entry, -1) {
			if variable := string(match[1]); !seen[variable] {
				seen[variable] = true
				envVars = append(envVars, variable)
			}
		}
	}
	sort.Strings(envVars)
	return kept, adapter.UserEnvNames(envVars)
}
