package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestUserMCPServersAreCopiedWithoutAimebu(t *testing.T) {
	dir := t.TempDir()
	config := `{"mcpServers":{"aimebu":{"command":"aimebu","args":["mcp"],"env":{"AIMEBU_URL":"http://v1"}},"docs":{"url":"https://docs.example/mcp","headers":{"Authorization":"Bearer ${DOCS_TOKEN}"}},"tool":{"command":"/usr/bin/tool","env":{"K":"${TOOL_KEY}"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	servers, env := userMCPServers(dir, "aimebu")
	if _, ok := servers["aimebu"]; ok || len(servers) != 2 {
		t.Fatalf("servers = %v", servers)
	}
	var docs struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(servers["docs"], &docs); err != nil || docs.URL != "https://docs.example/mcp" {
		t.Fatalf("docs = %s %v", servers["docs"], err)
	}
	if strings.Join(env, ",") != "DOCS_TOKEN,TOOL_KEY" {
		t.Fatalf("env = %v", env)
	}
	if got, _ := userMCPServers(filepath.Join(dir, "missing"), "aimebu"); len(got) != 0 {
		t.Fatal("a person without a config has no servers")
	}
	items := strings.Join(childEnv("/tmp/agent", env...), "\n")
	if strings.Contains(items, "TOOL_KEY=") {
		t.Fatal("a variable the environment does not have is not invented")
	}
}

// A native turn carries the person's own servers into its private mcp.json next to aimebu's entry, allows Pi's codemode tool for them, and
// leaves the v1 bus of the same name out.
func TestNativeTurnLoadsThePersonsOwnMCPServers(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "user-agent")
	if err := os.MkdirAll(userDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "mcp.json"), []byte(`{"mcpServers":{"aimebu":{"url":"http://v1.example/mcp"},"docs":{"url":"https://docs.example/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_CODING_AGENT_DIR", userDir)
	path := filepath.Join(root, "pi")
	argsPath := filepath.Join(root, "args")
	fixture, err := filepath.Abs(filepath.Join("testdata", "print.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsPath+"\ncat "+fixture+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Command: path, AllowNative: true, RuntimeRoot: filepath.Join(root, "runtime")}
	turn, err := a.Start(context.Background(), adapter.TurnSpec{TurnID: "own", Agent: adapter.AgentSpec{ID: "agt_test", Model: "ollama-cloud/gemma4:31b"},
		CheckoutPath: t.TempDir(), AgentTools: testToolsConfig(t, "agt_test"), NativeSessionHandle: testSession, Mode: adapter.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsPath)
	if !strings.Contains(string(args), "codemode") || strings.Contains(string(args), "mcp__") {
		t.Fatalf("the allowlist names codemode and no mcp__ entry:\n%s", args)
	}
	sum := sha256.Sum256([]byte("agt_test"))
	data, err := os.ReadFile(filepath.Join(root, "runtime", hex.EncodeToString(sum[:]), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Servers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
	if written.Servers["docs"].URL != "https://docs.example/mcp" || !strings.Contains(written.Servers["aimebu"].URL, "/api/agent/mcp") || strings.Contains(string(data), "v1.example") {
		t.Fatalf("the person's server next to aimebu's own entry, with no v1 bus: %s", data)
	}
}

// A person's entries are carried with their tools never exposed unprefixed, the v1 buses in any spelling are left out, a name that would
// undo the isolation is not passed, and a file that is not JSON carries nothing.
func TestUserMCPServersAreMadeSafeToCarry(t *testing.T) {
	dir := t.TempDir()
	config := `{"mcpServers":{
	  "AIMEBU":{"url":"http://upper-v1"},
	  "bus":{"command":"/usr/local/bin/aimebu","args":["mcp"]},
	  "port":{"url":"http://localhost:9997/mcp"},
	  "direct":{"url":"https://a.example","exposure":"direct","headers":{"Authorization":"Bearer ${DOCS_TOKEN}","X-Dir":"${PI_CODING_AGENT_DIR}"}},
	  "kept":{"url":"https://b.example","exposure":"deferred"},
	  "loud":{"url":"https://c.example","exposure":" Direct "}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	servers, env := userMCPServers(dir, "aimebu")
	if !strings.Contains(string(servers["loud"]), `"exposure":"codemode"`) {
		t.Fatalf("an exposure is decoded before it is compared: %s", servers["loud"])
	}
	if len(servers) != 3 || servers["AIMEBU"] != nil || servers["bus"] != nil || servers["port"] != nil {
		t.Fatalf("only the person's own servers: %v", servers)
	}
	if !strings.Contains(string(servers["direct"]), `"exposure":"codemode"`) || strings.Contains(string(servers["direct"]), `"direct"`) {
		t.Fatalf("a direct exposure becomes codemode: %s", servers["direct"])
	}
	if !strings.Contains(string(servers["kept"]), `"deferred"`) {
		t.Fatalf("another exposure stays: %s", servers["kept"])
	}
	if strings.Join(env, ",") != "DOCS_TOKEN" {
		t.Fatalf("a name that would point Pi at the person's config is not passed: %v", env)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"mcpServers": {`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, vars := userMCPServers(dir, "aimebu"); len(got) != 0 || len(vars) != 0 {
		t.Fatalf("a file that is not JSON carries nothing: %v %v", got, vars)
	}
}
