package codex

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// A native Codex turn runs from a private CODEX_HOME, so the person's own config.toml (model, profile, approvals, trust) never applies. Their
// MCP servers are the exception: the servers of their config.toml's mcp_servers table are copied into the private config, next to aimebu's own
// entry. Codex merges same-named tables (a url entry and a command entry together are refused), so a v1 bus of aimebu's name is never copied:
// aimebu's entry is the only one under that name.
//
// The config is read with a TOML parser, so every way TOML has of writing a server counts the same: the table form (`[mcp_servers.docs]` and its
// sub-tables), dotted keys, an inline table, spaces and quotes in a header. A file that is not valid TOML is not read at all (Codex itself
// would refuse it).

// server is one entry of the mcp_servers table.
type server struct {
	name  string
	table map[string]any
}

// parseServers reads the mcp_servers of a config.toml, by name. An entry that is not a table is left out (skipped says why).
func parseServers(config string) (servers []server, skipped map[string]string, err error) {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(config), &doc); err != nil {
		return nil, nil, err
	}
	raw, present := doc["mcp_servers"]
	if !present {
		return nil, nil, nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, errors.New("mcp_servers is not a table")
	}
	skipped = map[string]string{}
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, ok := table[name].(map[string]any)
		if !ok {
			skipped[name] = "it is not a table"
			continue
		}
		servers = append(servers, server{name: name, table: entry})
	}
	return servers, skipped, nil
}

// text writes a server's table back out as TOML, which the v1-bus match reads (the canonical spelling, whatever the person wrote).
func (s server) text() string {
	out, err := toml.Marshal(s.table)
	if err != nil {
		return ""
	}
	return string(out)
}

// userMCPServers returns the server tables of a Codex config.toml, written back out as TOML, except the servers skip names and the ones that are
// aimebu's own bus of v1 (see adapter.LooksLikeV1Bus), and the names of the environment variables those servers ask Codex to pass on or read a
// token from. A config that is not valid TOML gives an error and nothing.
func userMCPServers(config, skip string) (tables string, envVars []string, dropped []string, err error) {
	servers, skipped, err := parseServers(config)
	if err != nil {
		return "", nil, nil, err
	}
	for name := range skipped {
		dropped = append(dropped, name)
	}
	carried := map[string]any{}
	for _, s := range servers {
		if s.name == skip || adapter.LooksLikeV1Bus(s.name, s.text()) {
			dropped = append(dropped, s.name)
			continue
		}
		carried[s.name] = s.table
		envVars = append(envVars, referencedEnv(s.table)...)
	}
	sort.Strings(dropped)
	if len(carried) == 0 {
		return "", envVars, dropped, nil
	}
	out, err := toml.Marshal(map[string]any{"mcp_servers": carried})
	if err != nil {
		return "", nil, nil, err
	}
	return string(out), envVars, dropped, nil
}

// referencedEnv names the environment variables a server's table refers to: `env_vars = ["A", "B"]`, `bearer_token_env_var = "A"`, and the values of
// an `env_http_headers` table, which map a header to a variable.
func referencedEnv(table map[string]any) []string {
	var names []string
	if list, ok := table["env_vars"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok {
				names = append(names, name)
			}
		}
	}
	if name, ok := table["bearer_token_env_var"].(string); ok {
		names = append(names, name)
	}
	if headers, ok := table["env_http_headers"].(map[string]any); ok {
		for _, value := range headers {
			if name, ok := value.(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// parseProblem says what is wrong with a config.toml for a log line: where the parser stopped, never the content around it.
func parseProblem(err error) string {
	var decode *toml.DecodeError
	if errors.As(err, &decode) {
		row, column := decode.Position()
		return fmt.Sprintf("it is not valid TOML (line %d, column %d)", row, column)
	}
	return "it is not read: " + err.Error()
}

// userMCP is what a native turn copies from the person's Codex config: the tables to add to the private config, and the variable names to pass.
type userMCP struct {
	tables  string
	envVars []string
}

// A rejected entry is a property of the user's config, not of an individual
// turn. Keep the warning visible once without printing it on every delivery.
var skippedMCPWarnings sync.Map

func skippedMCPKey(kind, path string, config []byte, name, reason string) string {
	digest := sha256.Sum256(config)
	return fmt.Sprintf("%s\x00%s\x00%x\x00%s\x00%s", kind, path, digest, name, reason)
}

func logSkippedMCPOnce(key, format string, args ...any) {
	if _, loaded := skippedMCPWarnings.LoadOrStore(key, struct{}{}); !loaded {
		log.Printf(format, args...)
	}
}

// readUserMCP reads the person's Codex config next to their auth file for a native turn. A missing config has none; one that cannot be read or parsed is left
// out with a line in the log (the path, never the content), and the turn goes on without the servers.
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
	tables, envVars, dropped, err := userMCPServers(string(data), "aimebu")
	if err != nil {
		log.Printf("codex: the MCP servers of %s are not carried: %s", path, parseProblem(err))
		return userMCP{}
	}
	for _, name := range dropped {
		logSkippedMCPOnce(skippedMCPKey("native", path, data, name, "v1-or-not-table"), "codex: the MCP server %q of %s is not carried: it is aimebu's own bus of v1, or not a table", name, path)
	}
	if tables == "" {
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
// token variable, headers: a literal header is a credential, which would sit in the process list), one that points at the host itself (the
// container's localhost is not the host), and one whose name would replace aimebu's own entry.

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// carriedKeys are the settings of a server table that the -c override reproduces.
var carriedKeys = map[string]bool{"url": true, "startup_timeout_sec": true, "tool_timeout_sec": true}

// dockerOverrides returns the `-c` values, `mcp_servers.<name>={url=…,…}`, of the remote servers in a Codex config.toml that a container can use,
// and for each server it leaves out the reason, in words for the log. It fails closed: a server is carried only when its table holds nothing but
// the settings the override reproduces (url, the two timeouts, enabled = true); any other key, a sub-table of any kind, or a value of another type
// leaves the server out. A config that is not valid TOML gives an error and nothing.
func dockerOverrides(config string) (overrides []string, skipped map[string]string, err error) {
	servers, skipped, err := parseServers(config)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range servers {
		if s.name == "aimebu" || adapter.LooksLikeV1Bus(s.name, s.text()) {
			skipped[s.name] = "it is aimebu's own bus of v1"
			continue
		}
		if s.name == "aimebu_docker" {
			skipped[s.name] = "its name is the one aimebu's own entry has in a container"
			continue
		}
		override, reason := plainServer(s)
		if reason != "" {
			skipped[s.name] = reason
			continue
		}
		overrides = append(overrides, override)
	}
	return overrides, skipped, nil
}

// plainServer writes a server that is a url, and optionally enabled = true and the two timeouts, as a -c override, or says why it is not plain.
func plainServer(s server) (override, reason string) {
	if !bareKey.MatchString(s.name) {
		// Codex reads a quoted segment of a -c key with its quotes, so the server would get another name.
		return "", "its name cannot be given on a command line"
	}
	if _, runs := s.table["command"]; runs {
		return "", "it runs a command on the host, which is not in the container"
	}
	keys := make([]string, 0, len(s.table))
	for key := range s.table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "enabled" || carriedKeys[key] {
			continue
		}
		if _, isTable := s.table[key].(map[string]any); isTable {
			return "", "it has other tables (headers, variables, tools) that the container cannot be given safely"
		}
		return "", "it has a setting the container override cannot carry (" + key + ")"
	}
	if value, ok := s.table["enabled"]; ok && value != true {
		return "", "it is switched off"
	}
	rawURL, ok := s.table["url"]
	if !ok {
		return "", "it has no url (it runs a command on the host, which is not in the container)"
	}
	url, ok := rawURL.(string)
	if !ok {
		return "", "its url is not a plain string"
	}
	if why, unsafe := adapter.UnsafeContainerURL(url); unsafe {
		return "", why
	}
	value, err := tomlValue(url)
	if err != nil {
		return "", "its url cannot be written on a command line"
	}
	parts := []string{"url=" + value}
	for _, key := range []string{"startup_timeout_sec", "tool_timeout_sec"} {
		raw, ok := s.table[key]
		if !ok {
			continue
		}
		number, ok := tomlNumber(raw)
		if !ok {
			return "", "its " + key + " is not a plain number"
		}
		parts = append(parts, key+"="+number)
	}
	return "mcp_servers." + s.name + "={" + strings.Join(parts, ",") + "}", ""
}

// tomlValue writes a string as a TOML value, quoted the way the parser wants it.
func tomlValue(text string) (string, error) {
	out, err := toml.Marshal(map[string]string{"v": text})
	if err != nil {
		return "", err
	}
	value, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "v = ")
	if !ok {
		return "", errors.New("unexpected output")
	}
	return value, nil
}

// tomlNumber writes a finite number as a TOML number.
func tomlNumber(raw any) (string, bool) {
	switch number := raw.(type) {
	case int64:
		return strconv.FormatInt(number, 10), true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return "", false
		}
		text := strconv.FormatFloat(number, 'f', -1, 64)
		if !strings.ContainsAny(text, ".") {
			text += ".0"
		}
		return text, true
	}
	return "", false
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
	overrides, skipped, err := dockerOverrides(string(data))
	if err != nil {
		log.Printf("codex: the MCP servers of %s are not carried to the container: %s", path, parseProblem(err))
		return nil
	}
	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		logSkippedMCPOnce(skippedMCPKey("docker", path, data, name, skipped[name]), "codex: the MCP server %q of %s is not carried to the container: %s", name, path, skipped[name])
	}
	return overrides
}
