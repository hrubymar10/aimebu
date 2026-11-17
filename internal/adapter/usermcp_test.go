package adapter

import (
	"reflect"
	"testing"
)

func TestUserEnvNamesRefusesWhatWouldUndoTheIsolation(t *testing.T) {
	t.Parallel()
	got := UserEnvNames([]string{"DOCS_TOKEN", "codex_home", "CODEX_HOME", "PI_CODING_AGENT_DIR", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "VIBE_HOME", "AIMEBU_AGENT_KEY",
		"AIMEBU_URL", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_DATA_DIRS", "HOME", "LD_PRELOAD", "NODE_OPTIONS", "NODE_PATH", "SSH_AUTH_SOCK", "GIT_SSH_COMMAND", "GIT_DIR", "PYTHONPATH",
		"PYTHONSTARTUP", "PERL5LIB", "RUBYOPT", "JAVA_TOOL_OPTIONS", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "bad name", "", "1X", "GITHUB_TOKEN", "DOCS_TOKEN"})
	if want := []string{"DOCS_TOKEN", "GITHUB_TOKEN"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestLooksLikeV1Bus(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		name, text string
		want       bool
	}{
		"by name":                                   {"aimebu", `url = "https://x"`, true},
		"by name, any case":                         {"AIMEBU", `url = "https://x"`, true},
		"by name, padded":                           {" aimebu ", ``, true},
		"the binary with mcp":                       {"bus", `command = "aimebu"` + "\n" + `args = ["mcp"]`, true},
		"a path to the binary":                      {"bus", `{"command":["/usr/local/bin/aimebu","mcp"]}`, true},
		"v1's MCP route on the shared port":         {"bus", `url = "http://localhost:9997/mcp"`, true},
		"remote v1 MCP route":                       {"bus", `url = "http://192.168.1.10:9997/mcp"`, true},
		"unrelated local MCP route on another port": {"bus", `url = "http://host.docker.internal:9897/mcp"`, false},
		"unrelated loopback MCP server":             {"docs", `url = "http://localhost:3000/mcp"`, false},
		"v1's loopback alias and trailing slash":    {"bus", `url = "http://127.0.0.2:9997/mcp/"`, true},
		"v2's MCP route on the shared port":         {"bus", `url = "http://localhost:9997/api/agent/mcp"`, false},
		"a header and the binary":                   {"bus", "[mcp_servers.bus]\ncommand = \"aimebu\"\nargs = [\"mcp\"]\n", true},
		"the binary in a json array":                {"bus", `{"type":"local","command":["aimebu","mcp"]}`, true},
		"a filesystem server in an aimebu checkout": {"files", "[mcp_servers.files]\ncommand = \"npx\"\nargs = [\"-y\", \"@modelcontextprotocol/server-filesystem\", \"/Users/x/xcode/aimebu\"]\n", false},
		"the first word of a command string":        {"bus", "[mcp_servers.bus]\ncommand = \"aimebu mcp\"\n", true},
		"mcp in the args of a wrapper":              {"bus", "[mcp_servers.bus]\ncommand = \"npx\"\nargs = [\"aimebu\", \"mcp\"]\n", true},
		"a windows binary":                          {"bus", "[mcp_servers.bus]\ncommand = \"C:\\\\bin\\\\aimebu.exe\"\nargs = [\"mcp\"]\n", true},
		"env wrapper":                               {"bus", "[mcp_servers.bus]\ncommand = \"env\"\nargs = [\"X=1\", \"aimebu\", \"mcp\"]\n", true},
		"sh -c wrapper":                             {"bus", "[mcp_servers.bus]\ncommand = \"sh\"\nargs = [\"-c\", \"exec aimebu mcp\"]\n", true},
		"a path to the binary, args after":          {"bus", "[mcp_servers.bus]\ncommand = \"/opt/bin/aimebu\"\nargs = [\"--quiet\", \"mcp\"]\n", true},
		"another server on 9997":                    {"other", `url = "http://localhost:9997/api"`, false},
		"another server":                            {"docs", `url = "https://docs.example/mcp"`, false},
		"the word in a longer name":                 {"aimebu-docs", `command = "docs-server"`, false},
		"aimebu without mcp":                        {"tool", `command = "aimebu"` + "\n" + `args = ["server", "start"]`, false},
	} {
		if got := LooksLikeV1Bus(tc.name, tc.text); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestUnsafeContainerURL(t *testing.T) {
	t.Parallel()
	for url, want := range map[string]bool{
		"http://localhost:9/x": true, "https://LOCALHOST./x": true, "http://api.localhost/x": true, "http://127.0.0.1/x": true, "http://127.9.8.7:1/x": true,
		"http://[::1]:80/x": true, "http://0.0.0.0/x": true, "http://user@docs.example/x": true, "https://docs.example/mcp?token=a": true,
		"https://h.example/mcp#tok=abc": true, "https://h.example/mcp#": true, "http://2130706433/x": true, "http://0x7f.1/x": true, "ftp://docs.example/x": true,
		"https://docs.example/mcp": false, "https://localhost.example/mcp": false, "http://10.0.0.5/x": false,
	} {
		if _, got := UnsafeContainerURL(url); got != want {
			t.Errorf("%s: %v, want %v", url, got, want)
		}
	}
}
