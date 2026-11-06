package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

const userConfig = `model = "gpt-6"
approval_policy = "never"

[mcp_servers.aimebu]
command = "aimebu"
args = ["mcp"]
[mcp_servers.aimebu.env]
AIMEBU_URL = "http://v1"

[mcp_servers.'aimebu']
url = "http://literal-quoted-v1"

[mcp_servers.AIMEBU]
url = "http://upper-v1"

[mcp_servers.bus]
command = "/usr/local/bin/aimebu"
args = ["mcp"]

[mcp_servers.v1port]
url = "http://localhost:9997/mcp"

[mcp_servers.docs]
url = "https://docs.example/mcp"
bearer_token_env_var = "DOCS_TOKEN"
[mcp_servers.docs.env_http_headers]
X-Team = "TEAM_ID"

[mcp_servers."quoted name"]
command = "/usr/bin/tool"
env_vars = [
  "TOOL_KEY",
  "OTHER",
  "CODEX_HOME",
]

[profiles.fast]
model = "gpt-6-mini"

[mcp_servers.late]
command = "late"
`

// An ordinary server that only mentions aimebu is kept: a filesystem server rooted in an aimebu checkout, or aimebu started with other arguments.
func TestOrdinaryServersThatMentionAimebuAreKept(t *testing.T) {
	t.Parallel()
	config := "[mcp_servers.files]\ncommand = \"npx\"\nargs = [\"-y\", \"@modelcontextprotocol/server-filesystem\", \"/Users/x/xcode/aimebu\"]\n\n[mcp_servers.daemon]\ncommand = \"aimebu\"\nargs = [\"server\", \"start\"]\n\n[mcp_servers.real]\ncommand = \"aimebu\"\nargs = [\"mcp\"]\n"
	tables, _, dropped := userMCPServers(config, "aimebu")
	if !strings.Contains(tables, "server-filesystem") || !strings.Contains(tables, `args = ["server", "start"]`) || strings.Contains(tables, "[mcp_servers.real]") || len(dropped) != 1 || dropped[0] != "real" {
		t.Fatalf("tables:\n%s\ndropped: %v", tables, dropped)
	}
}

func TestUserMCPServersKeepsTheirTablesAndDropsAimebuAndV1Buses(t *testing.T) {
	t.Parallel()
	tables, vars, dropped := userMCPServers(userConfig, "aimebu")
	for _, want := range []string{"[mcp_servers.docs]", `url = "https://docs.example/mcp"`, "[mcp_servers.docs.env_http_headers]", `[mcp_servers."quoted name"]`, "[mcp_servers.late]"} {
		if !strings.Contains(tables, want) {
			t.Errorf("missing %q in\n%s", want, tables)
		}
	}
	for _, bad := range []string{"aimebu", "AIMEBU", "AIMEBU_URL", "literal-quoted-v1", "upper-v1", "9997", "approval_policy", "profiles.fast", `model = `} {
		if strings.Contains(tables, bad) {
			t.Errorf("%q must not be copied:\n%s", bad, tables)
		}
	}
	if len(dropped) != 4 {
		t.Errorf("dropped = %v: aimebu in two quotings (one server), AIMEBU, the binary and the v1 port", dropped)
	}
	if got := strings.Join(vars, ","); got != "DOCS_TOKEN,TEAM_ID,TOOL_KEY,OTHER,CODEX_HOME" && got != "DOCS_TOKEN,TOOL_KEY,OTHER,CODEX_HOME,TEAM_ID" {
		t.Errorf("env vars = %q", got)
	}
}

func TestWellFormedCatchesWhatWouldBreakEveryTurn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		text string
		ok   bool
	}{
		"plain":                  {"[mcp_servers.a]\nurl = \"x\"\nargs = [\"a\", \"b\"]\n", true},
		"multi-line array":       {"[mcp_servers.a]\nargs = [\n  \"a\",\n  \"b\", # note\n]\n", true},
		"inline table":           {"[mcp_servers.a]\nenv = { A = \"1\", B = \"[x\" }\n", true},
		"the same table twice":   {"[mcp_servers.a]\nurl = \"x\"\n[mcp_servers.a]\nurl = \"y\"\n", false},
		"the same table, quoted": {"[mcp_servers.docs]\nurl = \"x\"\n[mcp_servers.\"docs\"]\nurl = \"y\"\n", false},
		"quoted dotted parts":    {"[mcp_servers.'docs'.env]\nA = \"1\"\n[mcp_servers.docs.\"env\"]\nB = \"2\"\n", false},
		"an array of tables":     {"[[tools]]\nname = \"a\"\n[[tools]]\nname = \"b\"\n", true},
		"an open string":         {"[mcp_servers.a]\nurl = \"x\n", false},
		"an open array":          {"[mcp_servers.a]\nargs = [\"a\"\n[mcp_servers.b]\nurl = \"y\"\n", false},
		"a closing bracket more": {"[mcp_servers.a]\nargs = [\"a\"]]\n", false},
		"triple-quoted":          {"[mcp_servers.a]\nnote = \"\"\"a\nb\"\"\"\n", true},
	} {
		if got := wellFormed(tc.text); got != tc.ok {
			t.Errorf("%s: wellFormed = %v", name, got)
		}
	}
}

// A native turn: the private config.toml has exactly one aimebu table, the person's own servers and none of the v1 buses, the variables their
// servers name reach Codex (but never CODEX_HOME, which would point it at the person's whole config), and a read-only turn gets none of it.
func TestNativeTurnCarriesThePersonsOwnServersNextToAimebu(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	t.Setenv("DOCS_TOKEN", "tok")
	t.Setenv("UNRELATED", "no")
	root := t.TempDir()
	userHome := filepath.Join(root, "user-codex")
	if err := os.MkdirAll(userHome, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"config.toml": userConfig, "auth.json": `{"auth_mode":"chatgpt"}`} {
		if err := os.WriteFile(filepath.Join(userHome, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tools := filepath.Join(root, "tools.json")
	command := filepath.Join(root, "aimebu")
	raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"aimebu": map[string]any{"command": command, "args": []string{"mcp"}, "env": map[string]string{"AIMEBU2_URL": "http://127.0.0.1:9", "AIMEBU2_AGENT_ID": "agt_test", "AIMEBU2_AGENT_KEY": "aak_k"}}}})
	if err := os.WriteFile(tools, raw, 0600); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "captured")
	script := filepath.Join(root, "codex")
	body := "#!/bin/sh\ncat \"$CODEX_HOME/config.toml\" > " + capture + ".toml\nenv | grep -E '^(DOCS_TOKEN|UNRELATED|TEAM_ID|CODEX_HOME)=' | sort > " + capture + ".env\ncat >/dev/null\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}' '{\"type\":\"turn.started\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"messages\\\":[]}\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(mode adapter.Mode) (config, env string) {
		t.Helper()
		a := New(filepath.Join(root, "runtime-"+string(mode)))
		a.Command, a.AuthPath = script, filepath.Join(userHome, "auth.json")
		turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "own-" + string(mode), Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-luna"}, Mode: mode,
			CheckoutPath: root, AgentToolsConfigPath: tools, NotesJSON: json.RawMessage(`[]`)})
		if err != nil {
			t.Fatal(err)
		}
		for range turn.Events() {
		}
		if _, err := turn.Wait(); err != nil {
			t.Fatal(err)
		}
		c, _ := os.ReadFile(capture + ".toml")
		e, _ := os.ReadFile(capture + ".env")
		return string(c), string(e)
	}
	config, env := run(adapter.ModeNormal)
	if strings.Count(config, "[mcp_servers.aimebu]") != 1 || strings.Contains(config, "[mcp_servers.aimebu.env]\nAIMEBU_URL") || strings.Contains(config, "v1") {
		t.Fatalf("exactly one aimebu table, and no v1 bus:\n%s", config)
	}
	if !strings.Contains(config, "[mcp_servers.docs]") || !strings.Contains(config, `[mcp_servers."quoted name"]`) || !wellFormed(config) {
		t.Fatalf("the person's servers, in a well-formed config:\n%s", config)
	}
	if !strings.Contains(env, "DOCS_TOKEN=tok") || strings.Contains(env, "UNRELATED") || strings.Contains(env, "user-codex") {
		t.Fatalf("a server's named variable reaches Codex, nothing else, and CODEX_HOME is the private one:\n%s", env)
	}
	config, _ = run(adapter.ModeDiagnostic)
	if strings.Contains(config, "docs") || strings.Contains(config, "mcp_servers") {
		t.Fatalf("a read-only turn gets no MCP server at all:\n%s", config)
	}
}

// A person's config that cannot be carried over never breaks the turn: it runs with aimebu's entry alone.
func TestAMalformedUserConfigIsLeftOutAndTheTurnRuns(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	root := t.TempDir()
	userHome := filepath.Join(root, "user-codex")
	if err := os.MkdirAll(userHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte("[mcp_servers.docs]\nurl = \"https://docs.example\n[mcp_servers.docs]\nargs = [\"a\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readUserMCP(filepath.Join(userHome, "auth.json")); got.tables != "" || len(got.envVars) != 0 {
		t.Fatalf("malformed tables are not carried: %+v", got)
	}
	if got := readUserMCP(filepath.Join(root, "absent", "auth.json")); got.tables != "" {
		t.Fatalf("no config, no servers: %+v", got)
	}
}

const dockerConfig = `[mcp_servers.aimebu]
url = "http://v1.example/mcp"

[mcp_servers.docs]
url = "https://docs.example/mcp"
startup_timeout_sec = 20

[mcp_servers.literal]
url = "https://l.example/mcp"
[mcp_servers.literal.http_headers]
Authorization = "Bearer user-literal-token"

[mcp_servers.inline]
url = "https://i.example/mcp"
http_headers = { Authorization = "Bearer user-literal-token" }

[mcp_servers.tokened]
url = "https://t.example/mcp"
bearer_token_env_var = "T_TOKEN"

[mcp_servers.headered]
url = "https://h.example/mcp"
[mcp_servers.headered.env_http_headers]
X-Key = "H_KEY"

[mcp_servers.off]
url = "https://off.example/mcp"
enabled = false

[mcp_servers.listed]
url = "https://listed.example/mcp"
disabled_tools = ["danger"]

[mcp_servers.required]
url = "https://req.example/mcp"
required = true

[mcp_servers.stdio]
command = "/usr/bin/tool"
args = ["a"]

[mcp_servers.bus]
url = "http://localhost:9997/mcp"

[mcp_servers.local]
url = "http://127.0.0.1:8080/mcp"

[mcp_servers.loopback6]
url = "http://[::1]:8080/mcp"

[mcp_servers.aimebu_docker]
url = "https://impostor.example/mcp"

[mcp_servers.escapes]
url = "https://e.example/\q"

[mcp_servers."quoted name"]
url = "https://q.example"
`

func TestDockerOverridesCarryOnlyThePlainRemoteServersAContainerCanUse(t *testing.T) {
	t.Parallel()
	overrides, skipped := dockerOverrides(dockerConfig)
	if len(overrides) != 1 || overrides[0] != `mcp_servers.docs={url="https://docs.example/mcp",startup_timeout_sec=20}` {
		t.Fatalf("overrides = %q", overrides)
	}
	for name, why := range map[string]string{"aimebu": "v1", "literal": "other tables", "inline": "cannot carry (http_headers)", "tokened": "cannot carry (bearer_token_env_var)",
		"headered": "other tables", "off": "switched off", "listed": "cannot carry (disabled_tools)", "required": "cannot carry (required)", "stdio": "command",
		"bus": "v1", "local": "host itself", "loopback6": "host itself", "aimebu_docker": "own entry", "escapes": "plain string", "quoted name": "command line"} {
		if !strings.Contains(skipped[name], why) {
			t.Errorf("%s: skipped for %q, want %q", name, skipped[name], why)
		}
	}
}

// The docker path fails closed: a server table is carried only when every line of it is one the override reproduces verbatim.
func TestDockerOverridesFailClosedOnAnythingTheyCannotReproduce(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ config, why string }{
		"dotted header": {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\n[mcp_servers.docs.http_headers]\nX = \"y\"\n", "other tables"},
		"spaced header": {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\n[ mcp_servers . docs . http_headers ]\nX = \"y\"\n", "other tables"},
		"quoted header": {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\n[mcp_servers.\"docs\".http_headers]\nX = \"y\"\n", "other tables"},
		"dotted tools":  {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\ntools.search.approval_mode = \"approve\"\n", "cannot reproduce"},
		"dotted key":    {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\nhttp_headers.X = \"y\"\n", "cannot reproduce"},
		"userinfo":      {"[mcp_servers.docs]\nurl = \"https://u:p@d.example/mcp\"\n", "user name"},
		"query":         {"[mcp_servers.docs]\nurl = \"https://d.example/mcp?token=abc\"\n", "query"},
	} {
		overrides, skipped := dockerOverrides(tc.config)
		if len(overrides) != 0 {
			t.Errorf("%s: carried %q", name, overrides)
		}
		if !strings.Contains(skipped["docs"], tc.why) {
			t.Errorf("%s: skipped for %q, want %q", name, skipped["docs"], tc.why)
		}
	}
	if overrides, _ := dockerOverrides("[ mcp_servers . docs ]\nurl = \"https://d.example/mcp\"\n"); len(overrides) != 1 {
		t.Errorf("a spaced header of a plain server should still be carried: %q", overrides)
	}
}

// A Docker turn passes the person's remote servers as -c overrides, next to aimebu's own entry, and keeps ignoring the container's config.toml.
func TestDockerTurnPassesThePersonsRemoteServersAsOverrides(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "user-codex")
	if err := os.MkdirAll(userHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte(dockerConfig), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", userHome)
	config := filepath.Join(root, "private.json")
	if err := os.WriteFile(config, []byte(`{"mcpServers":{"aimebu":{"env":{"AIMEBU2_URL":"http://127.0.0.1:9897","AIMEBU2_AGENT_ID":"agt_test","AIMEBU2_AGENT_KEY":"aak_secret"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "codex-docker")
	argsPath := filepath.Join(root, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"messages\\\":[]}\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "docker"}
	run := func(mode adapter.Mode) string {
		t.Helper()
		turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "docker-" + string(mode), Agent: adapter.AgentSpec{ID: "agt_test", Tool: "codex", Variant: "docker", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: mode,
			AgentToolsConfigPath: config, NotesJSON: json.RawMessage(`[]`)})
		if err != nil {
			t.Fatal(err)
		}
		for range turn.Events() {
		}
		if _, err := turn.Wait(); err != nil {
			t.Fatal(err)
		}
		args, _ := os.ReadFile(argsPath)
		return string(args)
	}
	args := run(adapter.ModeNormal)
	for _, want := range []string{"mcp_servers.aimebu={command=\"true\",enabled=false}", "mcp_servers.aimebu_docker=", `mcp_servers.docs={url="https://docs.example/mcp"`, "--ignore-user-config"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q:\n%s", want, args)
		}
	}
	for _, bad := range []string{"tokened", "headered", "stdio", "v1.example", "9997", "quoted name", "user-literal-token", "impostor", "off.example", "127.0.0.1", "listed.example"} {
		if strings.Contains(args, bad) {
			t.Fatalf("%q must not reach the container:\n%s", bad, args)
		}
	}
	// aimebu's own two entries come last: nothing of the person's can replace them.
	if last := strings.LastIndex(args, "mcp_servers.docs="); last > strings.Index(args, "mcp_servers.aimebu_docker=") || last > strings.Index(args, "mcp_servers.aimebu={command") {
		t.Fatalf("the person's servers come before aimebu's own entries:\n%s", args)
	}
	if args = run(adapter.ModeDiagnostic); strings.Contains(args, "mcp_servers.docs") {
		t.Fatalf("a read-only turn gets no server:\n%s", args)
	}
}
