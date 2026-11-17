package codex

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	toml "github.com/pelletier/go-toml/v2"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

func TestSkippedDockerMCPWarningRepeatsOnlyAfterConfigChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	path := filepath.Join(root, "config.toml")
	config := "[mcp_servers.aimebu]\nurl = \"http://localhost:9997/mcp\"\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	userDockerOverrides()
	userDockerOverrides()
	if got := strings.Count(output.String(), "not carried to the container"); got != 1 {
		t.Fatalf("same config warned %d times, want once: %s", got, output.String())
	}
	if err := os.WriteFile(path, []byte(config+"# changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	userDockerOverrides()
	if got := strings.Count(output.String(), "not carried to the container"); got != 2 {
		t.Fatalf("changed config warned %d times, want twice: %s", got, output.String())
	}
}

const userConfig = `model = "gpt-6"
approval_policy = "never"

[mcp_servers.aimebu]
command = "aimebu"
args = ["mcp"]
[mcp_servers.aimebu.env]
AIMEBU_URL = "http://v1"

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

// carried parses the tables a copy produced and returns the servers in them, by name.
func carried(t *testing.T, tables string) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Servers map[string]map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal([]byte(tables), &doc); err != nil {
		t.Fatalf("the copied tables are not valid TOML (%v):\n%s", err, tables)
	}
	return doc.Servers
}

// An ordinary server that only mentions aimebu is kept: a filesystem server rooted in an aimebu checkout, or aimebu started with other arguments.
func TestOrdinaryServersThatMentionAimebuAreKept(t *testing.T) {
	t.Parallel()
	config := "[mcp_servers.files]\ncommand = \"npx\"\nargs = [\"-y\", \"@modelcontextprotocol/server-filesystem\", \"/Users/x/xcode/aimebu\"]\n\n[mcp_servers.daemon]\ncommand = \"aimebu\"\nargs = [\"server\", \"start\"]\n\n[mcp_servers.real]\ncommand = \"aimebu\"\nargs = [\"mcp\"]\n"
	tables, _, dropped, err := userMCPServers(config, "aimebu")
	servers := carried(t, tables)
	if err != nil || servers["files"] == nil || servers["daemon"] == nil || servers["real"] != nil || len(dropped) != 1 || dropped[0] != "real" {
		t.Fatalf("servers %v dropped %v err %v", servers, dropped, err)
	}
	if args, _ := servers["daemon"]["args"].([]any); len(args) != 2 || args[0] != "server" || args[1] != "start" {
		t.Fatalf("a kept server keeps its arguments: %v", servers["daemon"])
	}
}

func TestUserMCPServersKeepsTheirTablesAndDropsAimebuAndV1Buses(t *testing.T) {
	t.Parallel()
	tables, vars, dropped, err := userMCPServers(userConfig, "aimebu")
	if err != nil {
		t.Fatal(err)
	}
	servers := carried(t, tables)
	if got := len(servers); got != 3 || servers["docs"] == nil || servers["quoted name"] == nil || servers["late"] == nil {
		t.Fatalf("docs, quoted name and late are kept, nothing else: %v", servers)
	}
	if headers, _ := servers["docs"]["env_http_headers"].(map[string]any); headers["X-Team"] != "TEAM_ID" || servers["docs"]["url"] != "https://docs.example/mcp" {
		t.Errorf("docs keeps its url and its header map: %v", servers["docs"])
	}
	for _, bad := range []string{"aimebu", "AIMEBU", "AIMEBU_URL", "upper-v1", ":9997", "approval_policy", "profiles", "model"} {
		if strings.Contains(tables, bad) {
			t.Errorf("%q must not be copied:\n%s", bad, tables)
		}
	}
	if strings.Join(dropped, ",") != "AIMEBU,aimebu,bus,v1port" {
		t.Errorf("dropped = %v: aimebu, AIMEBU, the binary and the v1 port", dropped)
	}
	if got := strings.Join(vars, ","); got != "DOCS_TOKEN,TEAM_ID,TOOL_KEY,OTHER,CODEX_HOME" && got != "DOCS_TOKEN,TOOL_KEY,OTHER,CODEX_HOME,TEAM_ID" {
		t.Errorf("env vars = %q", got)
	}
}

// A real parser reads every spelling the same: dotted keys, an inline table of servers, spaces and quotes in a header, a server defined with its
// sub-tables first. A v1 bus is found whatever way it is written.
func TestEveryWayOfWritingAServerIsRead(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"table":           "[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\n[mcp_servers.docs.http_headers]\nX = \"y\"\n",
		"spaced header":   "[ mcp_servers . docs ]\nurl = \"https://d.example/mcp\"\n[ mcp_servers . docs . http_headers ]\nX = \"y\"\n",
		"quoted header":   "[mcp_servers.\"docs\"]\nurl = \"https://d.example/mcp\"\n[mcp_servers.'docs'.http_headers]\nX = \"y\"\n",
		"dotted keys":     "mcp_servers.docs.url = \"https://d.example/mcp\"\nmcp_servers.docs.http_headers.X = \"y\"\n",
		"inline table":    "mcp_servers = { docs = { url = \"https://d.example/mcp\", http_headers = { X = \"y\" } } }\n",
		"sub-table first": "[mcp_servers.docs.http_headers]\nX = \"y\"\n[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\n",
	} {
		tables, _, _, err := userMCPServers(config, "aimebu")
		servers := carried(t, tables)
		headers, _ := servers["docs"]["http_headers"].(map[string]any)
		if err != nil || servers["docs"]["url"] != "https://d.example/mcp" || headers["X"] != "y" {
			t.Errorf("%s: err %v servers %v", name, err, servers)
		}
	}
	for name, config := range map[string]string{
		"dotted":             "mcp_servers.aimebu.command = \"aimebu\"\nmcp_servers.aimebu.args = [\"mcp\"]\n",
		"inline":             "mcp_servers = { bus = { command = \"/opt/aimebu\", args = [\"mcp\"] } }\n",
		"wrapped":            "[mcp_servers.bus]\ncommand = \"env\"\nargs = [\"X=1\", \"aimebu\", \"mcp\"]\n",
		"under another name": "[mcp_servers.tools]\nurl = \"http://localhost:9997/mcp\"\n",
	} {
		tables, _, dropped, err := userMCPServers(config, "aimebu")
		if err != nil || strings.TrimSpace(tables) != "" || len(dropped) != 1 {
			t.Errorf("%s: a v1 bus must not be copied: err %v tables %q dropped %v", name, err, tables, dropped)
		}
	}
}

// A file that is not valid TOML is not read at all: Codex itself would refuse it, and a half-read config could carry a half of a server.
func TestAConfigThatIsNotValidTOMLCarriesNothing(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"the same table twice":         "[mcp_servers.a]\nurl = \"x\"\n[mcp_servers.a]\nurl = \"y\"\n",
		"the same table, quoted":       "[mcp_servers.docs]\nurl = \"x\"\n[mcp_servers.\"docs\"]\nurl = \"y\"\n",
		"an open string":               "[mcp_servers.a]\nurl = \"x\n",
		"an open array":                "[mcp_servers.a]\nargs = [\"a\"\n[mcp_servers.b]\nurl = \"y\"\n",
		"a closing bracket more":       "[mcp_servers.a]\nargs = [\"a\"]]\n",
		"a bad escape":                 "[mcp_servers.a]\nurl = \"https://e.example/\\q\"\n",
		"mcp_servers that is no table": "mcp_servers = 3\n",
	} {
		if tables, _, _, err := userMCPServers(config, "aimebu"); err == nil || tables != "" {
			t.Errorf("%s: err %v tables %q", name, err, tables)
		}
		if overrides, _, err := dockerOverrides(config); err == nil || len(overrides) != 0 {
			t.Errorf("%s (docker): err %v overrides %v", name, err, overrides)
		}
	}
	if line := parseProblem(func() error { _, _, err := parseServers("[a\nsecret = 1"); return err }()); strings.Contains(line, "secret") || !strings.Contains(line, "line") {
		t.Errorf("a problem names the place, never the content: %q", line)
	}
	// An entry of mcp_servers that is not a table is left out, the rest is carried.
	tables, _, dropped, err := userMCPServers("[mcp_servers]\nodd = 5\n[mcp_servers.fine]\nurl = \"https://f.example\"\n", "aimebu")
	if servers := carried(t, tables); err != nil || len(servers) != 1 || servers["fine"] == nil || len(dropped) != 1 || dropped[0] != "odd" {
		t.Errorf("servers %v dropped %v err %v", servers, dropped, err)
	}
}

// A native turn: the private config.toml has exactly one aimebu table, the person's own servers and none of the v1 buses, the variables their
// servers name reach Codex (but never CODEX_HOME, which would point it at the person's whole config), and a read-only turn gets none of it.
func TestNativeTurnCarriesThePersonsOwnServersNextToAimebu(t *testing.T) {
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
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
	tools := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9", Key: "aak_k", Command: filepath.Join(root, "aimebu")}
	capture := filepath.Join(root, "captured")
	script := filepath.Join(root, "codex")
	body := "#!/bin/sh\ncat \"$CODEX_HOME/config.toml\" > " + capture + ".toml\nenv | grep -E '^(DOCS_TOKEN|UNRELATED|TEAM_ID|CODEX_HOME)=' | sort > " + capture + ".env\ncat >/dev/null\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}' '{\"type\":\"turn.started\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"messages\\\":[]}\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := testenv.WriteExecutable(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(mode adapter.Mode) (config, env string) {
		t.Helper()
		a := New(filepath.Join(root, "runtime-"+string(mode)))
		a.Command, a.AuthPath = script, filepath.Join(userHome, "auth.json")
		turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "own-" + string(mode), Agent: adapter.AgentSpec{ID: "agt_test", Model: "gpt-6-luna"}, Mode: mode,
			CheckoutPath: root, AgentTools: tools, NotesJSON: json.RawMessage(`[]`)})
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
	if servers := carried(t, config); servers["docs"] == nil || servers["quoted name"] == nil || servers["aimebu"] == nil {
		t.Fatalf("the person's servers next to aimebu's, in a valid config:\n%s", config)
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
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
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

[mcp_servers."quoted name"]
url = "https://q.example"
`

func TestDockerOverridesCarryOnlyThePlainRemoteServersAContainerCanUse(t *testing.T) {
	t.Parallel()
	overrides, skipped, err := dockerOverrides(dockerConfig)
	if err != nil || len(overrides) != 1 || overrides[0] != "mcp_servers.docs={url='https://docs.example/mcp',startup_timeout_sec=20}" {
		t.Fatalf("overrides = %q", overrides)
	}
	for name, why := range map[string]string{"aimebu": "v1", "literal": "other tables", "inline": "other tables", "tokened": "cannot carry (bearer_token_env_var)",
		"headered": "other tables", "off": "switched off", "listed": "cannot carry (disabled_tools)", "required": "cannot carry (required)", "stdio": "runs a command",
		"bus": "v1", "local": "host itself", "loopback6": "host itself", "aimebu_docker": "own entry", "quoted name": "command line"} {
		if !strings.Contains(skipped[name], why) {
			t.Errorf("%s: skipped for %q, want %q", name, skipped[name], why)
		}
	}
}

// The docker path fails closed: a server table is carried only when every line of it is one the override reproduces verbatim.
func TestDockerOverridesFailClosedOnAnythingTheyCannotReproduce(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ config, why string }{
		"dotted header":   {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\n[mcp_servers.docs.http_headers]\nX = \"y\"\n", "other tables"},
		"spaced header":   {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\n[ mcp_servers . docs . http_headers ]\nX = \"y\"\n", "other tables"},
		"quoted header":   {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\n[mcp_servers.\"docs\".http_headers]\nX = \"y\"\n", "other tables"},
		"dotted tools":    {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\ntools.search.approval_mode = \"approve\"\n", "other tables"},
		"dotted key":      {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\nhttp_headers.X = \"y\"\n", "other tables"},
		"inline server":   {"mcp_servers = { docs = { url = \"https://d.example/mcp\", http_headers = { X = \"y\" } } }\n", "other tables"},
		"unknown key":     {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\nexperimental = 1\n", "cannot carry (experimental)"},
		"timeout as text": {"[mcp_servers.docs]\nurl = \"https://d.example/mcp\"\nstartup_timeout_sec = \"20\"\n", "plain number"},
		"userinfo":        {"[mcp_servers.docs]\nurl = \"https://u:p@d.example/mcp\"\n", "user name"},
		"query":           {"[mcp_servers.docs]\nurl = \"https://d.example/mcp?token=abc\"\n", "query"},
	} {
		overrides, skipped, err := dockerOverrides(tc.config)
		if err != nil || len(overrides) != 0 {
			t.Errorf("%s: carried %q", name, overrides)
		}
		if !strings.Contains(skipped["docs"], tc.why) {
			t.Errorf("%s: skipped for %q, want %q", name, skipped["docs"], tc.why)
		}
	}
	// A plain server is carried whatever way it is written, and its numbers and quotes come out as TOML.
	for name, config := range map[string]string{
		"spaced header": "[ mcp_servers . docs ]\nurl = \"https://d.example/mcp\"\nstartup_timeout_sec = 20\ntool_timeout_sec = 2.5\n",
		"dotted keys":   "mcp_servers.docs.url = \"https://d.example/mcp\"\nmcp_servers.docs.startup_timeout_sec = 20\nmcp_servers.docs.tool_timeout_sec = 2.5\n",
		"inline table":  "mcp_servers = { docs = { url = \"https://d.example/mcp\", startup_timeout_sec = 20, tool_timeout_sec = 2.5 } }\n",
	} {
		overrides, _, err := dockerOverrides(config)
		if err != nil || len(overrides) != 1 || overrides[0] != "mcp_servers.docs={url='https://d.example/mcp',startup_timeout_sec=20,tool_timeout_sec=2.5}" {
			t.Errorf("%s: carried %q err %v", name, overrides, err)
		}
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
	config := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	path := filepath.Join(root, "codex-docker")
	argsPath := filepath.Join(root, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"messages\\\":[]}\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, Variant: "docker"}
	run := func(mode adapter.Mode) string {
		t.Helper()
		turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "docker-" + string(mode), Agent: adapter.AgentSpec{ID: "agt_test", Tool: "codex", Variant: "docker", Model: "gpt-6-sol"}, CheckoutPath: root, Mode: mode,
			AgentTools: config, NotesJSON: json.RawMessage(`[]`)})
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
	for _, want := range []string{"mcp_servers.aimebu={command=\"true\",enabled=false}", "mcp_servers.aimebu_docker=", "mcp_servers.docs={url='https://docs.example/mcp'", "--ignore-user-config"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q:\n%s", want, args)
		}
	}
	for _, bad := range []string{"tokened", "headered", "stdio", "v1.example", ":9997", "quoted name", "user-literal-token", "impostor", "off.example", "127.0.0.1", "listed.example"} {
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
