package adapter

import (
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// What every native adapter does with the MCP servers of the person's own config for the tool (see the user_mcp.go of codex, opencode, pi and
// the vibe adapter): the servers are carried into the agent's private config, and the environment variables they name are passed through.
// Both need a rule that keeps a person's config from undoing the isolation, and one that keeps a v1 bus out.

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// deniedEnvPrefixes and deniedEnv are the variables a person's MCP server config can never pass through, because they would undo what the
// adapter sets for the turn: the tool's own homes and config locations (the adapter writes its value last, but a name it does not set, such
// as OPENCODE_CONFIG, would otherwise point the tool at the person's full config), aimebu's own, and what injects code into the process.
var (
	deniedEnvPrefixes = []string{"AIMEBU", "OPENCODE", "VIBE_", "PI_", "CODEX_", "CLAUDE", "LD_", "DYLD_", "XDG_", "GIT_", "PYTHON", "PERL5", "RUBY"}
	deniedEnv         = map[string]bool{"HOME": true, "PATH": true, "SSH_AUTH_SOCK": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NODE_EXTRA_CA_CERTS": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "REQUESTS_CA_BUNDLE": true, "JAVA_TOOL_OPTIONS": true,
		"_JAVA_OPTIONS": true, "JDK_JAVA_OPTIONS": true, "BASH_ENV": true, "ENV": true, "SHELL": true, "IFS": true, "PERL5LIB": true, "PERL5OPT": true}
)

// UserEnvNames keeps the names a person's MCP server config asks to be passed through that are safe to pass: well-formed, and none that the
// adapter owns (see deniedEnv). The result has each name once.
func UserEnvNames(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range names {
		if !envNamePattern.MatchString(name) || seen[name] || deniedEnv[strings.ToUpper(name)] {
			continue
		}
		denied := false
		for _, prefix := range deniedEnvPrefixes {
			if strings.HasPrefix(strings.ToUpper(name), prefix) {
				denied = true
				break
			}
		}
		if !denied {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

var (
	commandValue = regexp.MustCompile(`(?is)\bcommand["']?\s*[:=]\s*(\[[^\]]*\]|"(?:[^"\\]|\\.)*"|'[^']*')`)
	argsValue    = regexp.MustCompile(`(?is)\bargs["']?\s*[:=]\s*(\[[^\]]*\])`)
	quotedString = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|'([^']*)'`)
	v1Endpoint   = regexp.MustCompile(`:9997/[^\s"']*mcp`)
)

// strings reads the quoted strings of a value, in order.
func quotedStrings(value string) []string {
	var out []string
	for _, match := range quotedString.FindAllStringSubmatch(value, -1) {
		if match[1] != "" || match[2] == "" {
			out = append(out, match[1])
		} else {
			out = append(out, match[2])
		}
	}
	return out
}

// LooksLikeV1Bus says whether an MCP server entry of a person's config is aimebu's own bus of v1, which must never reach a v2 agent: the entry is
// called aimebu (any case), or its command line, read as words (the command, an array's items and the args, each split at spaces, so a wrapper
// such as `env` or `sh -c "aimebu mcp"` shows through), has the aimebu binary (a bare name or a path, with or without .exe) followed by an mcp
// word, or it points at v1's default port with an mcp path. text is the entry as written, header and all. A server that merely mentions aimebu
// (a filesystem server rooted in an aimebu checkout, or aimebu started with other arguments) is not one.
func LooksLikeV1Bus(name, text string) bool {
	if strings.EqualFold(strings.TrimSpace(name), "aimebu") {
		return true
	}
	if v1Endpoint.MatchString(strings.ToLower(text)) {
		return true
	}
	var words []string
	for _, pattern := range []*regexp.Regexp{commandValue, argsValue} {
		if match := pattern.FindStringSubmatch(text); match != nil {
			for _, item := range quotedStrings(match[1]) {
				words = append(words, strings.Fields(item)...)
			}
		}
	}
	for i, word := range words {
		base := strings.ToLower(filepath.Base(strings.ReplaceAll(strings.Trim(word, `"'`), `\`, "/")))
		if base != "aimebu" && base != "aimebu.exe" {
			continue
		}
		for _, later := range words[i+1:] {
			if strings.EqualFold(strings.Trim(later, `"'`), "mcp") {
				return true
			}
		}
	}
	return false
}

// UnsafeContainerURL says whether a remote MCP server url is one a container must not be given: it cannot be read as a url, it names the machine
// itself (localhost in any spelling, a loopback or unspecified address, or an address written as a bare number or in hex, which resolvers
// read as an address), which a container does not reach by that name, it carries a password or a user name, or it has a query string or a fragment (the
// places a token tends to go). The reason is in words for a log line.
func UnsafeContainerURL(raw string) (reason string, unsafe bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "its url cannot be read", true
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return "its url is not http or https", true
	}
	if u.User != nil {
		return "its url carries a user name or password", true
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "its url has a query string", true
	}
	if u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#") {
		return "its url has a fragment", true
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasSuffix(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "its url is the host itself, which a container does not reach by that name", true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr = addr.Unmap(); addr.IsLoopback() || addr.IsUnspecified() {
			return "its url is the host itself, which a container does not reach by that name", true
		}
		return "", false
	}
	if numericHost.MatchString(host) {
		return "its url names an address in a form that is read as the host", true
	}
	return "", false
}

var numericHost = regexp.MustCompile(`^(?:0x[0-9a-f]*|[0-9]+)(?:\.(?:0x[0-9a-f]*|[0-9]+))*$`)
