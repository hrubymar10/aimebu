package pi

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// A native Pi turn runs with a private PI_CODING_AGENT_DIR, so the person's own ~/.pi/agent never applies. Their MCP servers are the exception:
// the `mcpServers` of their global mcp.json are copied into the private mcp.json, next to aimebu's own entry, which always replaces an entry of
// that name (a v1 bus, say). The project's .pi/mcp.json (read only in trusted projects) is not carried over.

// userAgentDir is where Pi keeps the person's global config: $PI_CODING_AGENT_DIR, else ~/.pi/agent.
func userAgentDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".pi", "agent")
	}
	return ""
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// userMCPServers returns the entries of the person's mcp.json, without the one called skip and the ones that are aimebu's own bus of v1 (see
// adapter.LooksLikeV1Bus), and the names of the environment variables they refer to as ${NAME} (a bearer token, say), which a private turn has
// only when they are passed on. A copied entry is never exposed unprefixed: Pi's "direct" exposure would put its tools next to aimebu's own
// under their bare names, so it is changed to the default, "codemode". A config that cannot be read is left out with a line in the log (the
// path, never the content).
func userMCPServers(dir, skip string) (servers map[string]json.RawMessage, envVars []string) {
	if dir == "" {
		return nil, nil
	}
	path := filepath.Join(dir, "mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("pi: the person's MCP servers in %s are not carried: %v", path, err)
		}
		return nil, nil
	}
	var parsed struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &parsed) != nil {
		log.Printf("pi: the person's MCP servers in %s are not carried: the file is not valid JSON", path)
		return nil, nil
	}
	servers = map[string]json.RawMessage{}
	seen := map[string]bool{}
	for name, entry := range parsed.Servers {
		if name == skip {
			continue
		}
		if adapter.LooksLikeV1Bus(name, string(entry)) {
			log.Printf("pi: the MCP server %q is not carried: it is aimebu's own bus of v1", name)
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil {
			log.Printf("pi: the MCP server %q of %s is not carried: the entry is not an object", name, path)
			continue
		}
		var exposure string
		if raw, ok := fields["exposure"]; ok && json.Unmarshal(raw, &exposure) == nil && strings.EqualFold(strings.TrimSpace(exposure), "direct") {
			fields["exposure"] = json.RawMessage(`"codemode"`)
		}
		rewritten, err := json.Marshal(fields)
		if err != nil {
			continue
		}
		servers[name] = rewritten
		for _, match := range envRef.FindAllSubmatch(entry, -1) {
			if variable := string(match[1]); !seen[variable] {
				seen[variable] = true
				envVars = append(envVars, variable)
			}
		}
	}
	sort.Strings(envVars)
	return servers, adapter.UserEnvNames(envVars)
}
