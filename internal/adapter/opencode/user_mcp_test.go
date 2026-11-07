package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

const userJSONC = `{
  // the person's own config
  "theme": "dark",
  "mcp": {
    "aimebu": { "type": "local", "command": ["aimebu", "mcp"], "environment": {"AIMEBU_URL": "http://v1"} },
    "docs": { "type": "remote", "url": "https://docs.example/mcp", "headers": {"Authorization": "Bearer {env:DOCS_TOKEN}"}, /* trailing */ },
    "files": { "type": "local", "command": ["tool", "{file:./key.txt}"] },
    "url-in-string": { "type": "remote", "url": "https://x.example/a//b" },
  },
}
`

func TestUserMCPEntriesAreCopiedWithoutAimebuOrFileReferences(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte(userJSONC), 0600); err != nil {
		t.Fatal(err)
	}
	entries, env := userMCPEntries(dir, "aimebu")
	if _, ok := entries["aimebu"]; ok {
		t.Fatal("a same-named v1 entry must not be copied")
	}
	if _, ok := entries["files"]; ok {
		t.Fatal("an entry that reads a file relative to the config directory is left out")
	}
	var docs struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(entries["docs"], &docs); err != nil || docs.URL != "https://docs.example/mcp" {
		t.Fatalf("docs entry: %s %v", entries["docs"], err)
	}
	var slashes struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(entries["url-in-string"], &slashes); err != nil || slashes.URL != "https://x.example/a//b" {
		t.Fatalf("a // inside a string is not a comment: %s %v", entries["url-in-string"], err)
	}
	if strings.Join(env, ",") != "DOCS_TOKEN" {
		t.Fatalf("env = %v", env)
	}
	if got, _ := userMCPEntries(filepath.Join(dir, "missing"), "aimebu"); len(got) != 0 {
		t.Fatal("a person without a config has no servers")
	}
}

func TestStripJSONC(t *testing.T) {
	t.Parallel()
	got := string(stripJSONC([]byte("{\"a\": \"x // y /* z */\", // c\n \"b\": [1, 2,], /* d */ \"c\": {\"d\": 1,},}")))
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil || v["a"] != "x // y /* z */" {
		t.Fatalf("%q -> %v %v", got, v, err)
	}
}

// A native turn: the inline config has aimebu's entry and the person's own servers and none of the v1 buses, the variables their servers name
// reach OpenCode (never one that would point it at the person's own config), and a read-only turn gets none of it.
func TestNativeTurnCarriesThePersonsOwnServersNextToAimebu(t *testing.T) {
	root := t.TempDir()
	userConfig := filepath.Join(root, "user-config")
	if err := os.MkdirAll(filepath.Join(userConfig, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	config := `{"mcp":{
	  "aimebu":{"type":"local","command":["aimebu","mcp"]},
	  "AIMEBU":{"type":"remote","url":"http://upper-v1"},
	  "bus":{"type":"local","command":["/usr/local/bin/aimebu","mcp"]},
	  "docs":{"type":"remote","url":"https://docs.example/mcp","headers":{"Authorization":"Bearer {env:DOCS_TOKEN}","X-Dir":"{env:OPENCODE_CONFIG_DIR}"}}
	}}`
	if err := os.WriteFile(filepath.Join(userConfig, "opencode", "opencode.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", userConfig)
	t.Setenv("DOCS_TOKEN", "tok")
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Join(root, "user-config-dir"))
	t.Setenv("OPENCODE_CONFIG", filepath.Join(userConfig, "opencode", "opencode.json"))
	capture := filepath.Join(root, "captured")
	path := filepath.Join(root, "opencode")
	body := "#!/bin/sh\nprintf '%s' \"$OPENCODE_CONFIG_CONTENT\" > " + capture + ".json\nenv | grep -E '^(DOCS_TOKEN|OPENCODE_CONFIG|OPENCODE_CONFIG_DIR|XDG_CONFIG_HOME)=' | sort > " + capture + ".env\ncat >/dev/null\ncat " + fixture(t, "ok.jsonl") + "\n"
	if err := testenv.WriteExecutable(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(mode adapter.Mode) (config, env string) {
		t.Helper()
		turnSpec := spec(t, t.TempDir())
		turnSpec.Mode = mode
		a := &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime-"+string(mode))}
		turn, err := a.Start(context.Background(), turnSpec)
		if err != nil {
			t.Fatal(err)
		}
		if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
			t.Fatalf("exit=%+v err=%v", exit, err)
		}
		c, _ := os.ReadFile(capture + ".json")
		e, _ := os.ReadFile(capture + ".env")
		return string(c), string(e)
	}
	inline, env := run(adapter.ModeNormal)
	var parsed struct {
		MCP map[string]struct {
			URL     string   `json:"url"`
			Command []string `json:"command"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(inline), &parsed); err != nil {
		t.Fatalf("inline config %q: %v", inline, err)
	}
	if !strings.HasPrefix(parsed.MCP["aimebu"].URL, "http://127.0.0.1:9897") || len(parsed.MCP["aimebu"].Command) != 0 {
		t.Fatalf("aimebu's entry alone under that name: %+v", parsed.MCP["aimebu"])
	}
	if parsed.MCP["docs"].URL != "https://docs.example/mcp" || len(parsed.MCP) != 2 || strings.Contains(inline, "upper-v1") {
		t.Fatalf("the person's server, and no v1 bus: %+v\n%s", parsed.MCP, inline)
	}
	if !strings.Contains(env, "DOCS_TOKEN=tok") || strings.Contains(env, "OPENCODE_CONFIG=") || strings.Contains(env, "OPENCODE_CONFIG_DIR=") || strings.Contains(env, "XDG_CONFIG_HOME="+userConfig) {
		t.Fatalf("a named variable reaches OpenCode, but nothing that points it at the person's config:\n%s", env)
	}
	inline, _ = run(adapter.ModeDiagnostic)
	if strings.Contains(inline, "docs") || strings.Contains(inline, "aimebu") {
		t.Fatalf("a read-only turn gets no MCP server:\n%s", inline)
	}
}

func TestAMalformedUserConfigIsLeftOut(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(`{"mcp": {"docs": {`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, env := userMCPEntries(dir, "aimebu"); len(got) != 0 || len(env) != 0 {
		t.Fatalf("a file that is not JSON carries nothing: %v %v", got, env)
	}
}

// A Docker turn carries the person's remote servers only: a command on the host is not in the container, and the variables the remote ones
// name are forwarded by name like the provider keys.
func TestDockerTurnCarriesOnlyTheRemoteServers(t *testing.T) {
	entries := map[string]json.RawMessage{
		"docs":  json.RawMessage(`{"type":"remote","url":"https://docs.example/mcp","headers":{"Authorization":"Bearer {env:DOCS_TOKEN}","X":"{env:OPENCODE_CONFIG}"}}`),
		"local": json.RawMessage(`{"type":"local","command":["tool","--serve"]}`),
		"odd":   json.RawMessage(`"not an object"`),
		"self":  json.RawMessage(`{"type":"remote","url":"http://localhost:3000/mcp"}`),
		"cred":  json.RawMessage(`{"type":"remote","url":"https://u:p@docs.example/mcp"}`),
		"query": json.RawMessage(`{"type":"remote","url":"https://docs.example/mcp?token=a"}`),
	}
	kept, env := remoteOnly(entries)
	if len(kept) != 1 || kept["docs"] == nil {
		t.Fatalf("kept = %v", kept)
	}
	if strings.Join(env, ",") != "DOCS_TOKEN" {
		t.Fatalf("env = %v", env)
	}
}

// A Docker turn gets the person's remote servers in the inline config next to aimebu's entry, the variables they name forwarded by name, and not
// the ones that run a command.
func TestDockerStartCarriesTheRemoteServersAndForwardsTheirVariables(t *testing.T) {
	root := t.TempDir()
	userConfig := filepath.Join(root, "user-config")
	if err := os.MkdirAll(filepath.Join(userConfig, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	config := `{"mcp":{
	  "aimebu":{"type":"remote","url":"http://v1"},
	  "docs":{"type":"remote","url":"https://docs.example/mcp","headers":{"Authorization":"Bearer {env:DOCS_TOKEN}"}},
	  "local":{"type":"local","command":["tool","--serve"]}
	}}`
	if err := os.WriteFile(filepath.Join(userConfig, "opencode", "opencode.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", userConfig)
	t.Setenv("DOCS_TOKEN", "tok")
	path, record := dockerLauncher(t, root, true, "cat "+fixture(t, "ok.jsonl")+"\n")
	a := &Adapter{Command: path, Variant: "docker"}
	turn, err := a.Start(context.Background(), dockerSpec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	data, _ := os.ReadFile(record)
	got := string(data)
	var inline string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "OPENCODE_CONFIG_CONTENT=") {
			inline = strings.TrimPrefix(l, "OPENCODE_CONFIG_CONTENT=")
		}
	}
	if !strings.Contains(inline, "docs.example/mcp") || strings.Contains(inline, `"local"`) || strings.Contains(inline, "http://v1") || !strings.Contains(inline, "host.docker.internal") {
		t.Fatalf("the remote server next to aimebu's entry, no command, no v1 bus:\n%s", inline)
	}
	if !strings.Contains(got, "HARNESS_DOCKER_FORWARD_ENV=") || !strings.Contains(strings.SplitN(strings.SplitN(got, "HARNESS_DOCKER_FORWARD_ENV=", 2)[1], "\n", 2)[0], "DOCS_TOKEN") {
		t.Fatalf("the variable the remote server names is forwarded by name:\n%s", got)
	}
}
