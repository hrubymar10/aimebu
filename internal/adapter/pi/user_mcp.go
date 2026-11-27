package pi

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

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

// skippedWarnings holds what has been logged: a turn rebuilds the private mcp.json from the person's file, so a server left out is left out again
// on every turn of the session, and the line would otherwise repeat with each one.
var skippedWarnings sync.Map

// warnOnce logs a line once per session and message (the message names the server and the reason, so a new reason of the same server logs
// again), the pattern of the Claude adapter's skipped-transcript warning (internal/adapter/claude/transcript.go).
func warnOnce(session, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if _, seen := skippedWarnings.LoadOrStore(session+"\x00"+line, struct{}{}); !seen {
		log.Print(line)
	}
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// userMCPServers returns the entries of the person's mcp.json, without the one called skip and the ones that are aimebu's own bus of v1 (see
// adapter.LooksLikeV1Bus), and the names of the environment variables they refer to as ${NAME} (a bearer token, say), which a private turn has
// only when they are passed on. A copied entry is never exposed unprefixed: Pi's "direct" exposure would put its tools next to aimebu's own
// under their bare names, so it is changed to the default, "codemode". A config that cannot be read is left out with a line in the log (the
// path, never the content). A container turn carries only the entries that are safe to carry into one (containerServers): the config file
// lands in the shared checkout, where a co-tenant could read it, so an entry that holds more than a url is left out entirely rather than
// carried in a weaker form.
func userMCPServers(dir, skip, session string, container bool) (servers map[string]json.RawMessage, envVars []string) {
	if dir == "" {
		return nil, nil
	}
	path := filepath.Join(dir, "mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			warnOnce(session, "pi: the person's MCP servers in %s are not carried: %v", path, err)
		}
		return nil, nil
	}
	var parsed struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &parsed) != nil {
		warnOnce(session, "pi: the person's MCP servers in %s are not carried: the file is not valid JSON", path)
		return nil, nil
	}
	servers = map[string]json.RawMessage{}
	seen := map[string]bool{}
	for name, entry := range parsed.Servers {
		if name == skip {
			continue
		}
		if adapter.LooksLikeV1Bus(name, string(entry)) {
			warnOnce(session, "pi: the MCP server %q is not carried: it is aimebu's own bus of v1", name)
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil {
			warnOnce(session, "pi: the MCP server %q of %s is not carried: the entry is not an object", name, path)
			continue
		}
		if container && !containerServer(session, name, fields) {
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

// containerServer says whether an entry of the person's mcp.json may be carried into a container, where the file that would hold it is
// readable by a co-tenant: only a plain HTTP server with nothing but its url (and its exposure, which is rewritten above) is. A command
// points at a binary the container does not have, and a header, an environment variable or an OAuth field is a credential or a setting the
// checkout file must not hold; the entry is left out with the reason, never carried in a weaker form.
func containerServer(session, name string, fields map[string]json.RawMessage) bool {
	for key := range fields {
		switch key {
		case "url", "exposure":
		default:
			warnOnce(session, "pi: the MCP server %q is not carried to the container: its %s does not belong in the shared checkout", name, key)
			return false
		}
	}
	var entry struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(fields["url"], &entry.URL); err != nil || entry.URL == "" {
		warnOnce(session, "pi: the MCP server %q is not carried to the container: a server without a url has no remote form", name)
		return false
	}
	if reason, unsafe := adapter.UnsafeContainerURL(entry.URL); unsafe {
		warnOnce(session, "pi: the MCP server %q is not carried to the container: %s", name, reason)
		return false
	}
	return true
}
